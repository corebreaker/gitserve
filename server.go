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
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

const shutdownTimeout = 5 * time.Second

// gitHandler serves the repositories of a store over the smart HTTP protocol of Git.
//
// It reads a whole request before locking the store, and sends the response once the store is unlocked,
// both through spools: a slow client never keeps the store locked, which would stall the other requests.
type gitHandler struct {
	store *store
	// prefix is the URL path under which the repositories are served. It starts and ends with a slash.
	prefix string
}

// gitRequest is a request for a Git service on a repository.
type gitRequest struct {
	// name is the name under which the repository is served.
	name string
	// service is transport.UploadPackService or transport.ReceivePackService.
	service string
	// advertise tells that the client only asks for the references of the repository,
	// as the first step of a fetch or a push.
	advertise bool
	// version is the version of the protocol that the client asks for, from the Git-Protocol header.
	version string
	// body is the content of the request. An advertisement has none.
	body *spool
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

	req := gitRequest{version: r.Header.Get("Git-Protocol")}
	method := http.MethodPost
	req.name, req.advertise = strings.CutSuffix(path, "/info/refs")
	switch {
	case req.advertise:
		method, req.service = http.MethodGet, r.URL.Query().Get("service")
	case strings.HasSuffix(path, "/"+transport.UploadPackService):
		req.name, req.service = strings.TrimSuffix(path, "/"+transport.UploadPackService), transport.UploadPackService
	case strings.HasSuffix(path, "/"+transport.ReceivePackService):
		req.name, req.service = strings.TrimSuffix(path, "/"+transport.ReceivePackService), transport.ReceivePackService
	default:
		http.NotFound(w, r)
		return
	}

	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if req.service != transport.UploadPackService && req.service != transport.ReceivePackService {
		// Only very old clients, that predate the smart protocol, ask for references without naming a service.
		http.Error(w, "only the smart HTTP protocol of Git is supported", http.StatusForbidden)
		return
	}

	if !req.advertise {
		body, err := readBody(r)
		if err != nil {
			log.Printf("Reading a request for %s %s: %v", req.service, req.name, err)
			http.Error(w, "cannot read the request", http.StatusBadRequest)
			return
		}
		defer body.remove()

		req.body = body
	}

	response := &spool{}
	defer response.remove()

	// Pushing changes the repository, everything else only reads it.
	access := h.store.view
	if req.service == transport.ReceivePackService && !req.advertise {
		access = h.store.change
	}

	err := access(func(fsys billy.Filesystem) error {
		return h.run(r.Context(), fsys, req, response)
	})

	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound) || errors.Is(err, errInvalidName):
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	case err != nil:
		log.Printf("%s %s: %v", req.service, req.name, err)
		if response.size == 0 {
			// Nothing to send yet: the failure can still show as an HTTP status.
			http.Error(w, "the request failed", http.StatusInternalServerError)
			return
		}
	}

	send(w, req, response)
}

// readBody reads the whole body of the request into a spool, decompressing it when it is gzipped.
func readBody(r *http.Request) (*spool, error) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()

		body = gzipReader
	}

	content := &spool{}
	if _, err := io.Copy(content, body); err != nil {
		content.remove()
		return nil, err
	}

	return content, nil
}

// run runs the service that req asks for, on the repository of fsys that it names, and writes the result
// to response.
func (h *gitHandler) run(ctx context.Context, fsys billy.Filesystem, req gitRequest, response *spool) error {
	storage, err := openRepository(fsys, req.name)
	if err != nil {
		return err
	}
	defer storage.Close()

	var body io.ReadCloser
	if req.body != nil {
		content, err := req.body.reader()
		if err != nil {
			return err
		}

		// The service closes what it reads and what it writes, but the handler owns both spools.
		body = io.NopCloser(content)
	}

	result := ioutil.WriteNopCloser(response)
	if req.service == transport.UploadPackService {
		return transport.UploadPack(ctx, storage, body, result, &transport.UploadPackRequest{
			GitProtocol:   req.version,
			AdvertiseRefs: req.advertise,
			StatelessRPC:  true,
		})
	}

	var saveErr error
	multiplexed := false
	err = transport.ReceivePack(ctx, storage, body, result, &transport.ReceivePackRequest{
		GitProtocol:   req.version,
		AdvertiseRefs: req.advertise,
		StatelessRPC:  true,
		Hooks: transport.ReceivePackHooks{
			// Once the push has updated references, write an archive back to its file.
			PostReceive: func(_ context.Context, info *transport.PostReceiveInfo) error {
				if len(info.Commands) == 0 {
					return nil
				}

				// go-git gives io.Discard for progress when the response has no side-band channel.
				multiplexed = info.Progress != io.Discard
				saveErr = h.store.persist()

				return saveErr
			},
		},
	})

	if saveErr != nil {
		return reportNotSaved(response, filepath.Base(h.store.path), saveErr, multiplexed)
	}

	return err
}

// reportNotSaved replaces the response to a push whose archive could not be saved with an error,
// that makes the push fail on the client: the store discarded the push, which the client must not take for done.
// With a side-band channel, the error goes on its error band, which the client displays as a "remote:" message;
// without one, the response is left empty, for the handler to answer with an HTTP error.
func reportNotSaved(response *spool, archive string, saveErr error, multiplexed bool) error {
	err := fmt.Errorf("the push could not be saved to %s, and was discarded: %w", archive, saveErr)
	if resetErr := response.reset(); resetErr != nil || !multiplexed {
		return errors.Join(err, resetErr)
	}

	// Packets of the side-band suit clients of the side-band-64k as well. The client ends the message with a newline.
	message := []byte("gitserve: " + err.Error())
	_, writeErr := sideband.NewMuxer(sideband.Sideband, response).WriteChannel(sideband.ErrorMessage, message)
	if writeErr == nil {
		writeErr = pktline.WriteFlush(response)
	}

	return errors.Join(err, writeErr)
}

// send sends the response that a service wrote to a spool, in large chunks of known total length.
func send(w http.ResponseWriter, req gitRequest, response *spool) {
	content, err := response.reader()
	if err != nil {
		log.Printf("Reading the response of %s %s: %v", req.service, req.name, err)
		http.Error(w, "the request failed", http.StatusInternalServerError)
		return
	}

	kind := "result"
	if req.advertise {
		kind = "advertisement"
	}

	w.Header().Set("Content-Type", "application/x-"+req.service+"-"+kind)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.FormatInt(response.size, 10))
	if _, err := io.Copy(w, content); err != nil {
		log.Printf("Sending the response of %s %s: %v", req.service, req.name, err)
	}
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
