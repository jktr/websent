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
	"strings"
	"sync"
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

	// FIXME custom blackfriday HTMLRenderer seems like a better solution
	title := "websent"
	slidesHTML := []template.HTML{}
	slidesMarkdown := []string{}
	for _, slide := range bytes.Split(content, []byte("\n\n\n")) {

		text := string(bf.Run(slide, bf.WithExtensions(bf.CommonExtensions)))
		text = "<section>\n" + text + "</section>\n"

		slidesHTML = append(slidesHTML, template.HTML(text))
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
	ctx   context.Context
	state *State
	cond  *sync.Cond
}

func NewSlideHandler(ctx context.Context, state *State, cond *sync.Cond) SlideHandler {
	return SlideHandler{
		ctx:   ctx,
		state: state,
		cond:  cond,
	}
}

func (h SlideHandler) Dump(w io.Writer) error {
	h.state.M.RLock()
	if err := templates.ExecuteTemplate(w, "main.html", h.state); err != nil {
		return err
	}
}

func (h SlideHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// "/" matches all paths, so check that it's actually "/"
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

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
					reload, _ := embedded.ReadFile("partials/reload.html")
					w.Write(reload)
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

func tui(state *State, cond *sync.Cond, shutdown func()) {

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

	go tui(state, cond, cancel)

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

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Fatal("server shutdown failed")
	}
}
