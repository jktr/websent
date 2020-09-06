// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell"
	"github.com/gdamore/tcell/views"
	bf "github.com/russross/blackfriday/v2"
)

var (
	addr         string
	port         string
	assets       string
	stylesheet   string
	presentation string
)

// minimal stylesheet to get the slideshow effect
const documentHeader = `<!doctype html>
<meta charset='utf-8'>
<meta name='viewport' content='width=device-width, initial-scale=1.0, user-scalable=yes'>
<base href='/assets/'>
<style>
:root { --slide:%d; --total-slides:'%d'; }
body {
  height:100vh; width:100%%;
  position:fixed; overflow:hidden;
  padding: 0; margin: 0;
}
body > section {
  display: flex; flex-direction: column;
  align-items: center; justify-content: center;
  top: calc(-100vh * (var(--slide) - 1));
  position: relative;
  width: 100%%; height: 100vh;
  font-size: 7vh;
  overflow: hidden;
  margin: 0; padding: 0;
}
body > section code {
  font-size: 5vh;
  line-height: 0.5rem;
}`

func init() {
	flag.Usage = func() {
		fmt.Printf("Usage: %s [OPTIONS] SLIDES\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&stylesheet, "stylesheet", "style.css", "path to extra stylesheet")
	flag.StringVar(&assets, "asset-dir", "assets/", "path to dir with images and the like")
	flag.StringVar(&port, "port", "8080", "port to bind")
	flag.StringVar(&addr, "addr", "[::1]", "addr to bind")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}
	presentation = os.Args[len(os.Args)-1]
}

type State struct {
	Current    int // 1-indexed
	Total      int
	Generation int
	Title      string
	Slides     []string
	SlidesRaw  []string
	Stylesheet *string
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

	if len(slides) < 1 {
		return errors.New("tried to load a presentation without slides")
	}

	styleBytes, err := ioutil.ReadFile(stylesheet)
	if err != nil {
		return err
	}
	style := string(styleBytes)

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
	s.Stylesheet = &style

	return nil
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

func (h SlideHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	streamctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	events := h.state.EventStream(streamctx, h.cond)

	// send header, user stylesheet, and slides
	h.state.M.RLock()
	{
		fmt.Fprintf(w, documentHeader, h.state.Current, h.state.Total)
		fmt.Fprintln(w, *h.state.Stylesheet)
		fmt.Fprintf(w, "</style>\n<title>%s</title>\n", h.state.Title)
		for _, slide := range h.state.Slides {
			fmt.Fprintln(w, slide)
		}
	}
	h.state.M.RUnlock()

	if wf, ok := w.(http.Flusher); ok {
		wf.Flush()
	}

	for {
		select {
		case <-h.ctx.Done():
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
				fmt.Fprintf(w, "<style>:root{--slide:%d;}</style>\n", e)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			case string:
				switch e {
				case "refresh":
					fmt.Fprintln(w, "<script>location.reload();</script>")
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
			}
		}
	}
}

type Event int16

const (
	Unknown Event = iota
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
		case tcell.Button3:
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

	slide := views.NewText()
	slide.SetAlignment(views.VAlignCenter)
	panel.SetContent(slide)

	status := views.NewTextBar()
	status.SetRight(fmt.Sprintf("http://%s:%s ", addr, port), tcell.StyleDefault)
	panel.SetStatus(status)

	refreshTitle := func() {
		header.SetCenter(state.Title, tcell.StyleDefault)
		header.Draw()
	}
	refreshSlide := func() {
		status.SetLeft(
			fmt.Sprintf(" %d/%d", state.Current, state.Total),
			tcell.StyleDefault)
		status.Draw()

		raw := state.SlidesRaw[state.Current-1]
		prefix := "\n   "
		raw = prefix + strings.Join(strings.Split(raw, "\n"), prefix)
		slide.SetText(raw)
		slide.Draw()
	}

	refreshTitle()
	refreshSlide()

	panel.Draw()
	for {
		screen.Show()

		event := screen.PollEvent()
		switch decodeTcellEvent(event) {
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

func loadSlides(file string) (string, []string, []string, error) {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return "", []string{}, []string{}, err
	}

	// drops trailing blank lines/slides
	content = bytes.TrimSpace(content)

	// FIXME custom blackfriday HTMLRenderer seems like a better solution
	title := "websent"
	slidesHTML := []string{}
	slidesMarkdown := []string{}
	for _, slide := range bytes.Split(content, []byte("\n\n\n")) {

		text := string(bf.Run(slide, bf.WithExtensions(bf.CommonExtensions)))
		text = "<section>\n" + text + "</section>\n"

		slidesHTML = append(slidesHTML, text)
		slidesMarkdown = append(slidesMarkdown, string(slide)+"\n")
	}

	if bytes.HasPrefix(content, []byte("# ")) {
		// string containing everything after "# " in first line
		title = string(bytes.SplitN(content, []byte("\n"), 2)[0][2:])
	}

	return title, slidesHTML, slidesMarkdown, nil
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

	mux := http.NewServeMux()
	mux.Handle("/", NewSlideHandler(ctx, state, cond))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.Handle("/assets/", http.FileServer(http.Dir(assets)))
	mux.Handle("/favicon.ico", http.RedirectHandler(
		"/assets/favicon.ico", http.StatusTemporaryRedirect))

	srv := http.Server{Addr: addr + ":" + port, Handler: mux}

	go tui(state, cond, cancel)

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
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
