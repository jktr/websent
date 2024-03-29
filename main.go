// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/views"

	bf "github.com/russross/blackfriday/v2"
)

var (
	bind         string
	assets       string
	stylesheet   string
	presentation string
	output       string

	//go:embed partials/*.html styles/*.css
	embedded  embed.FS
	templates *template.Template
)

func init() {
	// argparsing
	flag.Usage = func() {
		fmt.Printf("Usage: %s [OPTIONS] SLIDES [OUTPUT]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&bind, "bind", "localhost:8080", "address and port to bind")
	flag.StringVar(&stylesheet, "style", "builtin:none",
		"path to extra stylesheet, or a builtin")
	flag.StringVar(&assets, "asset-dir", ".",
		"path to dir with images, fonts, etc")
	flag.Parse()

	switch flag.NArg() {
	case 1:
		presentation = os.Args[len(os.Args)-1]
	case 2:
		presentation = os.Args[len(os.Args)-2]
		output = os.Args[len(os.Args)-1]
	default:
		flag.Usage()
		os.Exit(1)
	}

	// embedded template loading
	templates = template.Must(template.ParseFS(embedded, "partials/*.html"))
}

type State struct {
	Current    int // 1-indexed
	Total      int
	Generation int
	Title      string
	Slides     []template.HTML
	SlidesRaw  []string
	UserStyle  template.CSS
	M          *sync.RWMutex
}

func (s *State) GotoSlide(slide int) {
	s.M.Lock()
	defer s.M.Unlock()

	if slide < 1 {
		s.Current = 1
	} else if slide > s.Total {
		s.Current = s.Total
	} else {
		s.Current = slide
	}
}

func (s *State) Reload(presentation, stylesheet string) error {
	title, slides, slidesRaw, err := loadSlides(presentation)
	if err != nil {
		return err
	}

	userstyle, err := loadUserStyle(stylesheet)
	if err != nil {
		return err
	}

	s.M.Lock()
	defer s.M.Unlock()

	s.Generation++
	s.Total = len(slides)
	if s.Current > s.Total {
		s.Current = s.Total
	}
	s.Title = title
	s.Slides = slides
	s.SlidesRaw = slidesRaw
	s.UserStyle = userstyle

	return nil
}

func loadUserStyle(stylesheet string) (template.CSS, error) {
	if strings.HasPrefix(stylesheet, "builtin:") {
		stylename := strings.TrimPrefix(stylesheet, "builtin:")

		style, err := embedded.ReadFile("styles/" + stylename + ".css")
		if err != nil {
			return "", fmt.Errorf(`tried to load builtin stylesheet "%s" that does not exist`, stylename)
		}
		return template.CSS(style), nil
	}

	style, err := os.ReadFile(stylesheet)
	return template.CSS(style), err
}

func loadSlides(file string) (string, []template.HTML, []string, error) {
	if !strings.HasSuffix(file, ".md") {
		return "", []template.HTML{}, []string{},
			errors.New(file + " doesn't end in '.md'; not markdown?")
	}

	content, err := os.ReadFile(file)
	if err != nil {
		return "", []template.HTML{}, []string{}, err
	}

	// drops trailing blank lines/slides
	content = bytes.TrimSpace(content)

	// XXX horrifying fixup for resizing images without `:has(> img)`
	imageSingle := regexp.MustCompile(`<p>(<img[^<>]+/>)</p>`)
	imageMulti := regexp.MustCompile(`<p>(?P<x>(<img[^<>]+/>\n?){2,}\n?)</p>`)
	imageCaptionBefore := regexp.MustCompile(`<p>(.+)\n(<img[^<>]+/>)</p>`)
	imageCaptionAfter := regexp.MustCompile(`<p>(<img[^<>]+/>)\n(.+)</p>`)

	// FIXME custom blackfriday HTMLRenderer seems like a better solution
	title := "websent"
	slidesHTML := []template.HTML{}
	slidesMarkdown := []string{}
	class := regexp.MustCompile(`^\.(.)+\n`)
	bfRenderer := bf.WithRenderer(bf.NewHTMLRenderer(bf.HTMLRendererParameters{
		Flags: bf.CommonHTMLFlags | bf.HrefTargetBlank | bf.NoreferrerLinks,
	}))
	for idx, slide := range bytes.Split(content, []byte("\n\n\n")) {

		macro := class.Find(slide)
		prefix := "<section id='s" + strconv.Itoa(idx+1) + "'"
		if len(macro) > 0 {
			prefix += " class='" + string(macro[1:len(macro)-1]) + "'"
			slide = bytes.TrimPrefix(slide, macro)
		}
		prefix += ">\n"
		suffix := "</section>\n"

		text := string(bf.Run(slide, bf.WithExtensions(bf.CommonExtensions), bfRenderer))

		// apply hacky fix-ups
		text = imageSingle.ReplaceAllString(text, "$1")
		text = imageCaptionBefore.ReplaceAllString(text, "<p>$1</p>\n$2")
		text = imageCaptionAfter.ReplaceAllString(text, "$1\n<p>$2</p>")
		text = imageMulti.ReplaceAllString(text, "$x")

		slidesHTML = append(slidesHTML, template.HTML(prefix+text+suffix))
		slidesMarkdown = append(slidesMarkdown, string(slide)+"\n")
	}

	if len(slidesMarkdown) < 1 {
		return title, slidesHTML, slidesMarkdown, errors.New("tried to load a presentation without slides")
	}

	if bytes.HasPrefix(content, []byte("# ")) {
		// string containing everything after "# " in first line
		title = string(bytes.SplitN(content, []byte("\n"), 2)[0][2:])
	}

	return title, slidesHTML, slidesMarkdown, nil
}

func (s *State) EventStream(ctx context.Context, cond *sync.Cond) <-chan interface{} {
	ch := make(chan interface{}, 1)
	go func() {
		defer close(ch)

		s.M.RLock()
		generation := s.Generation
		current := s.Current
		s.M.RUnlock()

		for {
			// ref: https://golang.org/pkg/sync/#Cond.Wait
			cond.L.Lock()
			cond.Wait()
			cond.L.Unlock()

			select {
			case <-ctx.Done():
				return
			default:
			}

			s.M.RLock()
			{
				if s.Generation > generation {
					ch <- "refresh"

					s.M.RUnlock()
					return
				}
				if s.Current != current {
					current = s.Current
					ch <- current
				}
			}
			s.M.RUnlock()

		}
	}()
	return ch
}

type SlideHandler struct {
	ctx       context.Context
	state     *State
	cond      *sync.Cond
	connected *int32
}

func NewSlideHandler(ctx context.Context, state *State, cond *sync.Cond) SlideHandler {
	return SlideHandler{
		ctx:       ctx,
		state:     state,
		cond:      cond,
		connected: new(int32),
	}
}

func (h SlideHandler) Dump(w io.Writer) error {
	h.state.M.RLock()
	if err := templates.ExecuteTemplate(w, "main.html", h.state); err != nil {
		return err
	}
	h.state.M.RUnlock()
	trailer, _ := embedded.ReadFile("partials/trailer.html")
	_, err := w.Write(trailer)
	return err
}

func (h SlideHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// "/" matches all paths, so check that it's actually just "/"
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// As long as we're not shutting down, issue a refresh directive.
	// This allows hot-reloading the page, but forces us to serve a
	// final reload of the presentation after we've initiated shutdown.
	select {
	case <-h.ctx.Done():
	default:
		w.Header().Set("Refresh", "0")
	}

	atomic.AddInt32(h.connected, 1)
	defer atomic.AddInt32(h.connected, -1)

	streamctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	events := h.state.EventStream(streamctx, h.cond)

	// send presentation content
	h.state.M.RLock()
	if err := templates.ExecuteTemplate(w, "main.html", h.state); err != nil {
		h.state.M.RUnlock()
		return
	}
	h.state.M.RUnlock()

	if wf, ok := w.(http.Flusher); ok {
		wf.Flush()
	}

	for {
		select {
		case <-h.ctx.Done():
			trailer, _ := embedded.ReadFile("partials/trailer.html")
			w.Write(trailer)
			return
		case <-time.After(30 * time.Second):
			// Trickle a byte so client doesn't close connection.
			// Browsers typically time out connections after 1 minute.
			fmt.Fprintln(w)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case e, more := <-events:
			if !more {
				return
			}

			switch e := e.(type) {
			case int:
				templates.ExecuteTemplate(w, "slidechange.html",
					struct{ Current int }{Current: e})
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			case string:
				switch e {
				case "refresh":
					// there's a Refresh header, so return triggers a a reload
					return
				}
			}
		}
	}
}

type Event int16

const (
	Unknown Event = iota
	First
	Last
	Next
	Prev
	Reload
	Quit
	Redraw
)

func decodeTcellEvent(event tcell.Event) Event {
	switch event := event.(type) {
	case *tcell.EventResize:
		return Redraw
	case *tcell.EventMouse:
		switch event.Buttons() {
		case tcell.Button1:
			return Next
		case tcell.Button2:
			return Prev
		case tcell.WheelDown:
			return Next
		case tcell.WheelUp:
			return Prev
		}
	case *tcell.EventKey:
		key := event.Key()

		if key == tcell.KeyRune {
			// TODO make these configurable
			switch event.Rune() {
			case 'g':
				return First
			case 'G':
				return Last
			case 'j': // vi-style forward
				return Next
			case 'k': // vi-style back
				return Prev
			case 't': // dvorak-style forward
				return Next
			case 'n': // dvorak-style back
				return Prev
			case 'r':
				return Reload
			case 'q':
				return Quit
			}
		} else {
			switch key {
			case tcell.KeyCtrlD:
				return Quit
			case tcell.KeyCtrlC:
				return Quit
			}
		}
	}
	return Unknown
}

func tui(state *State, cond *sync.Cond, connected *int32, dropping *int32, shutdown func()) {

	screen, err := tcell.NewScreen()
	if err != nil {
		log.Fatal(err)
	}

	if err = screen.Init(); err != nil {
		log.Fatal(err)
	}

	screen.HideCursor()
	screen.EnableMouse()
	defer screen.DisableMouse()

	panel := views.NewPanel()
	panel.SetView(screen)

	header := views.NewTextBar()
	header.SetStyle(tcell.StyleDefault.Reverse(true))
	panel.SetTitle(header)

	slides := views.NewBoxLayout(views.Orientation(1))
	panel.SetContent(slides)

	window := [3]*views.Text{}
	for idx := range window {
		w := views.NewText()
		slides.AddWidget(w, 0.3)
		window[idx] = w
	}

	status := views.NewTextBar()
	status.SetLeft(" http://"+bind, tcell.StyleDefault)
	status.SetRight("j|next k|prev r|eload q|uit ", tcell.StyleDefault)
	panel.SetStatus(status)

	refreshTitle := func() {
		header.SetCenter(state.Title, tcell.StyleDefault)
		header.SetRight(fmt.Sprintf("%d ", *connected), tcell.StyleDefault)
		header.Draw()
	}
	refreshSlide := func() {
		status.SetCenter(
			fmt.Sprintf(" %d/%d", state.Current, state.Total),
			tcell.StyleDefault)
		status.Draw()

		for idx, r := range [3]string{} {
			indent := "\n         "
			prefix := indent
			if state.Current+idx <= state.Total {
				if idx == 0 {
					prefix = fmt.Sprintf("\n\n > %3d   ", state.Current+idx)
				} else {
					prefix = fmt.Sprintf("   %3d   ", state.Current+idx)
				}
				r = state.SlidesRaw[state.Current+idx-1]
			}

			content := strings.Join(strings.Split(r, "\n"), indent)
			window[idx].SetText(prefix + content)
		}
		slides.Draw()
	}

	refreshTitle()
	refreshSlide()

	panel.Draw()
	for {
		screen.Show()

		event := screen.PollEvent()
		refreshTitle()
		switch decodeTcellEvent(event) {
		case First:
			state.GotoSlide(1)
			cond.Broadcast()
			refreshSlide()
		case Last:
			state.GotoSlide(state.Total)
			cond.Broadcast()
			refreshSlide()
		case Next:
			state.GotoSlide(state.Current + 1)
			cond.Broadcast()
			refreshSlide()
		case Prev:
			state.GotoSlide(state.Current - 1)
			cond.Broadcast()
			refreshSlide()
		case Reload:
			err := state.Reload(presentation, stylesheet)
			if err != nil {
				log.Println(err)
				continue
			}
			cond.Broadcast()
			refreshTitle()
			refreshSlide()
		case Quit:
			*dropping = *connected
			shutdown()
			cond.Broadcast()
			screen.Fini()
			return
		case Redraw:
			panel.Resize()
			panel.Draw()
		}
	}
}

func main() {
	state := &State{
		Current: 1,
		M:       &sync.RWMutex{},
	}

	err := state.Reload(presentation, stylesheet)
	if err != nil {
		log.Fatal(err)
	}

	cond := sync.NewCond(&sync.Mutex{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sh := NewSlideHandler(ctx, state, cond)

	if output != "" {
		time.Sleep(time.Second)
		file, err := os.Create(output)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		w := bufio.NewWriter(file)
		if err := sh.Dump(w); err != nil {
			log.Fatal(err)
		}
		w.Flush()
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/", sh)
	mux.Handle("/assets/", http.StripPrefix("/assets", http.FileServer(http.Dir(assets))))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.Handle("/favicon.ico", http.RedirectHandler(
		"/assets/favicon.ico", http.StatusTemporaryRedirect))

	srv := http.Server{Addr: bind, Handler: mux}

	dropping := int32(0)
	go tui(state, cond, sh.connected, &dropping, cancel)

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	select {
	case <-ctx.Done():
	case <-sigchan:
		cancel()
	}

	// Due to the way refresh works, we need to allow clients some time to
	// do a final reload of the page once we've initiated shutdown. This
	// allows us to skip that shutdown delay if no clients were connected.
	if dropping > 0 {
		time.Sleep(time.Second)
	}

	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Fatal("server shutdown failed")
	}
}
