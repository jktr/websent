package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	addr       string
	port       string
	slides     string
	stylesheet string
)

func init() {
	flag.Usage = func() {
		fmt.Printf("Usage: %s [OPTIONS] SLIDES\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&stylesheet, "stylesheet", "style.css", "path to extra stylesheet")
	flag.StringVar(&port, "port", "8080", "port to bind")
	flag.StringVar(&addr, "addr", "[::1]", "addr to bind")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}
	slides = os.Args[len(os.Args)-1]
}

type State struct {
	Current    int
	Total      int
	Generation int
}

func UpdateStream(ctx context.Context, next *State, cond *sync.Cond) <-chan interface{} {
	ch := make(chan interface{}, 1)
	go func() {
		defer close(ch)

		state := *next
		for {
			cond.L.Lock()
			cond.Wait()
			cond.L.Unlock()

			select {
			case <-ctx.Done():
				return
			default:
			}

			if next.Generation > state.Generation {
				ch <- "refresh"
				return
			}
			if next.Current != state.Current {
				state.Current = next.Current
				ch <- state.Current
			}

		}
	}()
	return ch
}

func NewSlideHandler(ctx context.Context, next *State, wg *sync.Cond) func(http.ResponseWriter, *http.Request) {
	f := func(w http.ResponseWriter, r *http.Request) {

		streamctx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := UpdateStream(streamctx, next, wg)

		fmt.Fprintf(w, `<!doctype html>
<meta charset='utf-8'>
<meta name='viewport' content='width=device-width, initial-scale=1.0, user-scalable=yes'>
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
}
`+"\n\n", next.Current, next.Total)

		sendFile(w, r, stylesheet)
		fmt.Fprintln(w, "</style>")
		sendFile(w, r, slides)
		if wf, ok := w.(http.Flusher); ok {
			wf.Flush()
		}

		for {
			select {
			case <-time.After(30 * time.Second):
				fmt.Fprintln(w)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			case <-ctx.Done():
				return
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
	return f
}

func sendFile(w http.ResponseWriter, r *http.Request, name string) {
	if f, err := os.Open(name); err != nil {
		log.Print(err)
	} else {
		if _, err = io.Copy(w, f); err != nil {
			log.Print(err)
		}
	}
}

func clamp(min int, max int, n int) int {
	if n < min {
		return min
	} else if max < n {
		return max
	} else {
		return n
	}
}

func control(state *State, cond *sync.Cond, shutdown func()) {
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

		switch string(b) {
		case "t":
			fallthrough
		case "j":
			state.Current = clamp(1, state.Total, state.Current+1)
			cond.Broadcast()
		case "n":
			fallthrough
		case "k":
			state.Current = clamp(1, state.Total, state.Current-1)
			cond.Broadcast()
		case "r":
			state.Generation++
			cond.Broadcast()
		case "q":
			shutdown()
			cond.Broadcast()
			return
		}
	}
}

func main() {
	state := &State{
		Current:    1,
		Total:      100,
		Generation: 0,
	}

	cond := sync.NewCond(&sync.Mutex{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go control(state, cond, cancel)

	mux := http.NewServeMux()
	mux.HandleFunc("/", NewSlideHandler(ctx, state, cond))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "favicon.ico")
	})

	srv := http.Server{Addr: addr + ":" + port, Handler: mux}

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
