package main

import (
    "path/filepath"
    "os"
    "os/signal"
    "log"
    "net/http"
    "context"
    "time"

    "github.com/AaronO/go-git-http"
)

type tResponseWriter struct {
    w    http.ResponseWriter
    code int
}

func (self *tResponseWriter) Header() http.Header         { return self.w.Header() }
func (self *tResponseWriter) Write(b []byte) (int, error) { return self.w.Write(b) }
func (self *tResponseWriter) WriteHeader(code int) {
    self.code = code
    self.w.WriteHeader(code)
}

type tHandler struct {
    h http.Handler
}

// Implement the http.Handler interface
func (self *tHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    wrapped_w := &tResponseWriter{w: w, code: 200}
    self.h.ServeHTTP(wrapped_w, r)
    log.Printf("HTTP-RESP: [From:%s] %s %s (%d)", r.RemoteAddr, r.Method, r.RequestURI, wrapped_w.code)
}

func main() {
    path := os.Args[1]
    name := path
    if len(os.Args) > 2 {
        name = os.Args[2]
    } else {
        p, err := filepath.Abs(path)
        if err != nil {
            panic(err)
        }

        path = filepath.Dir(p)
    }

    log.Printf("Root: %s", path)
    log.Printf("Name: %s", name)

    if name[0] != '/' {
        name = "/" + name
    }

    if name[len(name)-1] != '/' {
        name += "/"
    }

    // Get git handler to serve a directory of repos
    git := githttp.New(path)

    // Attach handler to http serveri
    mux := http.NewServeMux()

    mux.Handle(name, git)
    server := &http.Server{Addr: ":8085", Handler: &tHandler{mux}}

    end := make(chan struct{})

    stop_signal := make(chan os.Signal)
    defer close(stop_signal)

    signal.Notify(stop_signal, os.Interrupt)

    go func() {
        <-stop_signal
        log.Println("Server interrupted")
        ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)

        server.Shutdown(ctx)

        cancel()
    }()

    // Start HTTP server
    go func() {
        defer close(end)

        err := server.ListenAndServe()
        if err != nil {
            log.Fatal("ListenAndServe: ", err)
        }
    }()

    <-end
}
