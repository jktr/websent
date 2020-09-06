// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	bf "github.com/russross/blackfriday/v2"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// flags
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
	Slides     *string
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
	slides, err := loadSlides(presentation)
	if err != nil {
		return err
	}
	slidesConcat := strings.Join(slides, "\n")

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

	s.Total = len(slides)
	if s.Current > s.Total {
		s.Current = s.Total
	}
	s.Stylesheet = &style
	s.Slides = &slidesConcat

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
	fmt.Fprintf(w, documentHeader, h.state.Current, h.state.Total)
	fmt.Fprintln(w, *h.state.Stylesheet)
	fmt.Fprintln(w, "</style>")
	fmt.Fprintln(w, *h.state.Slides)
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

func cli(state *State, cond *sync.Cond, shutdown func()) {
	// XXX it's either this or ncurses :/
	exec.Command("stty", "-F", "/dev/tty", "cbreak", "min", "1").Run()
	var b []byte = make([]byte, 1)
	for {
		fmt.Printf("%d/%d> ", state.Current, state.Total)
		_, err := os.Stdin.Read(b)
		if err != nil {
			if err == io.EOF {
				shutdown()
				break
			} else {
				log.Print(err)
				continue
			}
		}
		fmt.Println()

		// TODO make this configurable
		// jk - vi-style forward/back on querty
		// tn - vi-style forward/back on dvorak (but shifted right by one)
		// r - reload
		// q - quit
		exit := false
		switch string(b) {
		case "t":
			fallthrough
		case "j":
			state.GotoSlide(state.Current + 1)
		case "n":
			fallthrough
		case "k":
			state.GotoSlide(state.Current - 1)
		case "r":
			state.Generation++
			err := state.Reload(presentation, stylesheet)
			if err != nil {
				log.Println(err)
				continue
			}
		case "q":
			shutdown()
			exit = true
			return
		}

		cond.Broadcast()
		if exit {
			return
		}
	}
}

func loadSlides(file string) ([]string, error) {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return []string{}, err
	}

	// drops trailing blank lines/slides
	content = bytes.TrimSpace(content)

	// FIXME custom blackfriday HTMLRenderer seems like a better solution
	slides := []string{}
	for idx, slide := range bytes.Split(content, []byte("\n\n\n")) {

		text := string(bf.Run(slide, bf.WithExtensions(bf.CommonExtensions)))
		text = "<section>\n" + text + "</section>\n"

		// auto-generate page title from markdown header line
		if idx == 0 && bytes.HasPrefix(slide, []byte("# ")) {
			// get first line in slide, then use everything after "# " as title
			title := string(bytes.SplitN(slide, []byte("\n"), 2)[0][2:])
			text = "<title>" + title + "</title>\n" + text
		}

		slides = append(slides, text)
	}
	return slides, nil
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
	fmt.Printf("Listening on http://%s:%s\n", addr, port)

	go cli(state, cond, cancel)

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
