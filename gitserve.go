package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AaronO/go-git-http"
)

const (
	listenAddr      = ":8085"
	shutdownTimeout = 5 * time.Second
	usage           = "usage: gitserve <path> [<prefix>]"
)

// statusRecorder wraps an http.ResponseWriter to remember the status code sent to the client.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

// WriteHeader records the status code, then sends it.
func (rec *statusRecorder) WriteHeader(code int) {
	rec.code = code
	rec.ResponseWriter.WriteHeader(code)
}

// requestLogger logs every request with the status code of its response.
type requestLogger struct {
	next http.Handler
}

// ServeHTTP serves the request with the wrapped handler, then logs it.
func (l *requestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wrappedWriter := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	l.next.ServeHTTP(wrappedWriter, r)
	log.Printf("HTTP-RESP: [From:%s] %s %s (%d)", r.RemoteAddr, r.Method, r.RequestURI, wrappedWriter.code)
}

// parseArgs returns the repositories root and the URL prefix to serve, from the command-line arguments
// without the program name.
//
// With two arguments, they are the root and the prefix. With one argument, the root is the parent directory
// of that path, and the prefix is its base name. go-git-http looks a repository up as the root joined with
// the whole URL path, so the prefix always names a directory under the root.
// The prefix is normalized to start and end with a slash.
func parseArgs(args []string) (string, string, error) {
	var root, prefix string

	switch len(args) {
	case 1:
		abs, err := filepath.Abs(args[0])
		if err != nil {
			return "", "", err
		}

		root, prefix = filepath.Dir(abs), filepath.Base(abs)
	case 2:
		root, prefix = args[0], args[1]
	default:
		return "", "", fmt.Errorf("expected 1 or 2 arguments, got %d", len(args))
	}

	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return root, prefix, nil
}

// serve serves HTTP requests on listener until a signal arrives on stop, then shuts the server down gracefully:
// in-flight requests get shutdownTimeout to complete, then the remaining connections are closed.
// It returns once the shutdown is over, or as soon as serving fails.
func serve(server *http.Server, listener net.Listener, stop <-chan os.Signal) error {
	serveFailed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		var sig os.Signal
		select {
		case sig = <-stop:
		case <-serveFailed:
			return
		}

		log.Printf("Received %v, shutting down", sig)

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Shutdown: %v", err)

			if err := server.Close(); err != nil {
				log.Printf("Close: %v", err)
			}
		}
	}()

	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		// Serving failed on its own: release the goroutine waiting for a signal, and let it exit.
		close(serveFailed)
		<-shutdownDone

		return err
	}

	// Serve returns as soon as Shutdown starts: wait until in-flight requests are drained.
	<-shutdownDone

	return nil
}

func main() {
	root, prefix, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gitserve: %v\n%s\n", err, usage)
		os.Exit(2)
	}

	log.Printf("Root: %s", root)
	log.Printf("Prefix: %s", prefix)

	// Get git handler to serve a directory of repos
	git := githttp.New(root)

	// Attach handler to http server
	mux := http.NewServeMux()
	mux.Handle(prefix, git)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal("Listen: ", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	server := &http.Server{Handler: &requestLogger{next: mux}}
	if err := serve(server, listener, stop); err != nil {
		log.Fatal("Serve: ", err)
	}
}
