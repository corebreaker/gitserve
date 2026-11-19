package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6"
)

func TestGitHandlerRoutesRequests(t *testing.T) {
	st := newMemoryStore()
	if err := createRepository(st.fs, "demo.git", "main"); err != nil {
		t.Fatal(err)
	}

	const notAllowed = http.StatusMethodNotAllowed

	handler := &gitHandler{store: st, prefix: "/git/"}
	tests := []struct {
		method      string
		target      string
		code        int
		contentType string
	}{
		{
			method:      http.MethodGet,
			target:      "/git/demo/info/refs?service=git-upload-pack",
			code:        http.StatusOK,
			contentType: "application/x-git-upload-pack-advertisement",
		},
		{
			method:      http.MethodGet,
			target:      "/git/demo.git/info/refs?service=git-receive-pack",
			code:        http.StatusOK,
			contentType: "application/x-git-receive-pack-advertisement",
		},
		{method: http.MethodGet, target: "/demo/info/refs?service=git-upload-pack", code: http.StatusNotFound},
		{method: http.MethodGet, target: "/git/missing/info/refs?service=git-upload-pack", code: http.StatusNotFound},
		{method: http.MethodGet, target: "/git/../demo/info/refs?service=git-upload-pack", code: http.StatusNotFound},
		{method: http.MethodGet, target: "/git/demo/info/refs", code: http.StatusForbidden},
		{method: http.MethodGet, target: "/git/demo/info/refs?service=git-upload-archive", code: http.StatusForbidden},
		{method: http.MethodPost, target: "/git/demo/info/refs?service=git-upload-pack", code: notAllowed},
		{method: http.MethodGet, target: "/git/demo/git-upload-pack", code: notAllowed},
		{method: http.MethodGet, target: "/git/demo/git-receive-pack", code: notAllowed},
		{method: http.MethodGet, target: "/git/demo/HEAD", code: http.StatusNotFound},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))

		if rec.Code != tt.code {
			t.Errorf("%s %s: status %d, want %d", tt.method, tt.target, rec.Code, tt.code)
		}

		if tt.contentType != "" && rec.Header().Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s: content type %q, want %q", tt.method, tt.target, rec.Header().Get("Content-Type"),
				tt.contentType)
		}
	}
}

func TestGitHandlerAdvertisesReferences(t *testing.T) {
	st := newMemoryStore()
	if err := createRepository(st.fs, "demo", "main"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler := &gitHandler{store: st, prefix: "/"}
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/demo/info/refs?service=git-upload-pack", nil))

	// A smart HTTP advertisement starts with a pkt-line naming the service, followed by a flush-pkt.
	if want := "001e# service=git-upload-pack\n0000"; !strings.HasPrefix(rec.Body.String(), want) {
		t.Errorf("advertisement %q does not start with %q", rec.Body.String(), want)
	}

	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("advertisement cache control %q, want no-cache", rec.Header().Get("Cache-Control"))
	}

	// The response is sent whole, with its length.
	if length := rec.Header().Get("Content-Length"); length != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("advertisement content length %q, want %d", length, rec.Body.Len())
	}
}

// blockingReader is the body of a request from a slow client: it blocks until released, then ends.
type blockingReader struct {
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingReader() *blockingReader {
	return &blockingReader{reading: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.reading) })
	<-r.release

	return 0, io.EOF
}

// blockingWriter is the response to a slow client: its writes block until released.
type blockingWriter struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		writing:          make(chan struct{}),
		release:          make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.release

	return w.ResponseRecorder.Write(p)
}

// assertStoreAvailable checks that the store can be changed, which needs every other access to be over.
func assertStoreAvailable(t *testing.T, st *store) {
	t.Helper()

	changed := make(chan error, 1)
	go func() { changed <- st.change(func(billy.Filesystem) error { return nil }) }()

	select {
	case err := <-changed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the store stays locked while a slow client transfers data")
	}
}

func TestGitHandlerDoesNotLockStoreWhileReceiving(t *testing.T) {
	st := newMemoryStore()
	if err := createRepository(st.fs, "demo", "main"); err != nil {
		t.Fatal(err)
	}

	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// A push from a slow client is still sending its request.
	body := newBlockingReader()
	served := make(chan struct{})
	go func() {
		defer close(served)

		req := httptest.NewRequest(http.MethodPost, "/demo/git-receive-pack", body)
		(&gitHandler{store: st, prefix: "/"}).ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-body.reading
	assertStoreAvailable(t, st)

	close(body.release)
	<-served
}

func TestGitHandlerDoesNotLockStoreWhileSending(t *testing.T) {
	st := newMemoryStore()
	if err := createRepository(st.fs, "demo", "main"); err != nil {
		t.Fatal(err)
	}

	// A slow client is still receiving the references of the repository.
	w := newBlockingWriter()
	served := make(chan struct{})
	go func() {
		defer close(served)

		req := httptest.NewRequest(http.MethodGet, "/demo/info/refs?service=git-upload-pack", nil)
		(&gitHandler{store: st, prefix: "/"}).ServeHTTP(w, req)
	}()

	<-w.writing
	assertStoreAvailable(t, st)

	close(w.release)
	<-served

	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Errorf("advertisement to the slow client: status %d, %d bytes", w.Code, w.Body.Len())
	}
}

func TestGitHandlerRejectsInvalidGzipBody(t *testing.T) {
	st := newMemoryStore()
	if err := createRepository(st.fs, "demo", "main"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/demo/git-upload-pack", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")

	rec := httptest.NewRecorder()
	(&gitHandler{store: st, prefix: "/"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid gzip body: status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestReportNotSaved(t *testing.T) {
	saveErr := errors.New("disk full")
	message := "gitserve: the push could not be saved to repos.zip, and was discarded: disk full"

	// With a side-band channel, the report of the push gives way to a packet on the error band, and a flush-pkt.
	response := &spool{}
	defer response.remove()

	fmt.Fprint(response, "report of a successful push")
	if err := reportNotSaved(response, "repos.zip", saveErr, true); !errors.Is(err, saveErr) {
		t.Errorf("reportNotSaved with side-band: error %v, want the save error", err)
	}

	want := fmt.Sprintf("%04x\x03%s0000", 4+1+len(message), message)
	if got := string(readSpool(t, response)); got != want {
		t.Errorf("response with side-band: %q, want %q", got, want)
	}

	// Without one, the response is left empty, for the handler to answer with an HTTP error.
	if err := response.reset(); err != nil {
		t.Fatal(err)
	}

	fmt.Fprint(response, "report of a successful push")
	if err := reportNotSaved(response, "repos.zip", saveErr, false); !errors.Is(err, saveErr) {
		t.Errorf("reportNotSaved without side-band: error %v, want the save error", err)
	}

	if response.size != 0 {
		t.Errorf("response without side-band: %d bytes left, want none", response.size)
	}
}

func TestStatusRecorderFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := http.NewResponseController(&statusRecorder{ResponseWriter: rec}).Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder: %v", err)
	}

	if !rec.Flushed {
		t.Error("statusRecorder did not flush the wrapped response writer")
	}
}

func TestRequestLoggerLogsStatus(t *testing.T) {
	var logs bytes.Buffer

	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	tests := []struct {
		name    string
		handler http.Handler
		want    string
	}{
		{
			name:    "explicit status",
			handler: http.NotFoundHandler(),
			want:    "GET /demo.git/info/refs (404)",
		},
		{
			name: "implicit status",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "ok")
			}),
			want: "GET /demo.git/info/refs (200)",
		},
	}

	for _, tt := range tests {
		logs.Reset()

		logger := &requestLogger{next: tt.handler}
		logger.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/demo.git/info/refs", nil))

		if !strings.Contains(logs.String(), tt.want) {
			t.Errorf("%s: log %q does not contain %q", tt.name, logs.String(), tt.want)
		}
	}
}

func TestServeReturnsServeError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	listener.Close()

	// No signal is ever sent: serve must return on its own, without leaving its goroutine blocked.
	served := make(chan error, 1)
	go func() { served <- serve(&http.Server{}, listener, make(chan os.Signal)) }()

	select {
	case err := <-served:
		if err == nil {
			t.Fatal("serve returned nil, want the error from Serve")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after Serve failed")
	}
}

func TestServeWaitsForInFlightRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})}

	stop := make(chan os.Signal, 1)
	served := make(chan error, 1)
	go func() { served <- serve(server, listener, stop) }()

	type response struct {
		body string
		err  error
	}

	responses := make(chan response, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String() + "/")
		if err != nil {
			responses <- response{err: err}
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		responses <- response{body: string(body), err: err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	stop <- os.Interrupt

	select {
	case err := <-served:
		t.Fatalf("serve returned while a request was still in flight (error: %v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case got := <-responses:
		if got.err != nil || got.body != "done" {
			t.Fatalf("in-flight request: body %q, error %v; want body %q", got.body, got.err, "done")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the shutdown")
	}
}
