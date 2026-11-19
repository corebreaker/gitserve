package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

const shutdownTimeout = 5 * time.Second

// gitHandler serves the repositories of a store over the smart HTTP protocol of Git.
type gitHandler struct {
	store *store
	// prefix is the URL path under which the repositories are served. It starts and ends with a slash.
	prefix string
}

// ServeHTTP routes a request of the smart HTTP protocol to the service it asks for.
// A repository answers at <prefix><name>/info/refs, <prefix><name>/git-upload-pack
// and <prefix><name>/git-receive-pack.
func (h *gitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, h.prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}

	method, service := http.MethodPost, ""
	name, advertise := strings.CutSuffix(path, "/info/refs")
	switch {
	case advertise:
		method, service = http.MethodGet, r.URL.Query().Get("service")
	case strings.HasSuffix(path, "/"+transport.UploadPackService):
		name, service = strings.TrimSuffix(path, "/"+transport.UploadPackService), transport.UploadPackService
	case strings.HasSuffix(path, "/"+transport.ReceivePackService):
		name, service = strings.TrimSuffix(path, "/"+transport.ReceivePackService), transport.ReceivePackService
	default:
		http.NotFound(w, r)
		return
	}

	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if service != transport.UploadPackService && service != transport.ReceivePackService {
		// Only very old clients, that predate the smart protocol, ask for references without naming a service.
		http.Error(w, "only the smart HTTP protocol of Git is supported", http.StatusForbidden)
		return
	}

	run := func(fsys billy.Filesystem) error {
		h.run(w, r, fsys, name, service, advertise)
		return nil
	}

	// Pushing changes the repository, everything else only reads it.
	access := h.store.view
	if service == transport.ReceivePackService && !advertise {
		access = h.store.change
	}

	if err := access(run); err != nil {
		log.Printf("Store %s: %v", h.store.path, err)
		http.Error(w, "store unavailable", http.StatusInternalServerError)
	}
}

// run runs a service on the repository served under name: it reads the client request from the HTTP request,
// if any, and streams the result as the HTTP response. With advertise, the service only advertises the references
// of the repository, as the first step of a fetch or a push.
func (h *gitHandler) run(w http.ResponseWriter, r *http.Request, fsys billy.Filesystem, name, service string,
	advertise bool,
) {
	storage, err := openRepository(fsys, name)
	if errors.Is(err, transport.ErrRepositoryNotFound) || errors.Is(err, errInvalidName) {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("Opening repository %s: %v", name, err)
		http.Error(w, "cannot open the repository", http.StatusInternalServerError)
		return
	}
	defer storage.Close()

	var request io.ReadCloser
	result := "advertisement"
	if !advertise {
		result = "result"
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid gzip request body", http.StatusBadRequest)
				return
			}
			defer gzipReader.Close()

			body = gzipReader
		}

		// The service closes the request once read, but the HTTP server owns the body.
		request = io.NopCloser(body)
	}

	w.Header().Set("Content-Type", "application/x-"+service+"-"+result)
	w.Header().Set("Cache-Control", "no-cache")

	response := &responseStream{w: w, controller: http.NewResponseController(w)}
	version := r.Header.Get("Git-Protocol")
	if service == transport.UploadPackService {
		err = transport.UploadPack(r.Context(), storage, request, response, &transport.UploadPackRequest{
			GitProtocol:   version,
			AdvertiseRefs: advertise,
			StatelessRPC:  true,
		})
	} else {
		err = transport.ReceivePack(r.Context(), storage, request, response, &transport.ReceivePackRequest{
			GitProtocol:   version,
			AdvertiseRefs: advertise,
			StatelessRPC:  true,
			Hooks:         transport.ReceivePackHooks{PostReceive: h.persist},
		})
	}

	if err != nil {
		log.Printf("%s %s: %v", service, name, err)
		if !response.started.Load() {
			http.Error(w, "service failed", http.StatusInternalServerError)
		}
	}
}

// persist runs once a push has updated references: it writes an archive back to its file.
// A failure is reported to the pushing client, which displays it as a "remote:" message.
func (h *gitHandler) persist(_ context.Context, info *transport.PostReceiveInfo) error {
	if len(info.Commands) == 0 {
		return nil
	}

	if err := h.store.persist(); err != nil {
		log.Printf("Saving %s: %v", h.store.path, err)
		fmt.Fprintf(info.Progress, "gitserve: the push could not be saved to %s: %v\n",
			filepath.Base(h.store.path), err)

		return err
	}

	return nil
}

// responseStream writes the result of a service to an HTTP response.
// It flushes each write, so that the client displays progress messages without delay.
type responseStream struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	// started records whether the response started, after which an error can no longer change its status.
	// It is atomic, because the services write from goroutines of their own.
	started atomic.Bool
}

// Write writes p to the response, and flushes it.
func (s *responseStream) Write(p []byte) (int, error) {
	s.started.Store(true)

	n, err := s.w.Write(p)
	if err == nil {
		err = s.controller.Flush()
	}

	return n, err
}

// Close does nothing: the HTTP server ends the response once the handler returns.
func (s *responseStream) Close() error {
	return nil
}

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

// Unwrap returns the wrapped http.ResponseWriter, so that http.ResponseController can flush it.
func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
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
