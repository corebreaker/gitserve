package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArgsWithPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{prefix: "/", want: "/"},
		{prefix: "", want: "/"},
		{prefix: "repos", want: "/repos/"},
		{prefix: "/repos", want: "/repos/"},
		{prefix: "repos/", want: "/repos/"},
		{prefix: "/team/repos/", want: "/team/repos/"},
	}

	for _, tt := range tests {
		root, prefix, err := parseArgs([]string{"gitroot", tt.prefix})
		if err != nil {
			t.Fatalf("parseArgs(gitroot, %q): unexpected error: %v", tt.prefix, err)
		}

		if root != "gitroot" || prefix != tt.want {
			t.Errorf("parseArgs(gitroot, %q) = (%q, %q), want (%q, %q)", tt.prefix, root, prefix, "gitroot", tt.want)
		}
	}
}

func TestParseArgsWithPathOnly(t *testing.T) {
	paths := []string{
		"gitroot",
		filepath.Join("nested", "gitroot"),
		filepath.Join(t.TempDir(), "gitroot"),
	}

	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}

		root, prefix, err := parseArgs([]string{path})
		if err != nil {
			t.Fatalf("parseArgs(%q): unexpected error: %v", path, err)
		}

		if want := filepath.Dir(abs); root != want {
			t.Errorf("parseArgs(%q) root = %q, want %q", path, root, want)
		}

		if prefix != "/gitroot/" {
			t.Errorf("parseArgs(%q) prefix = %q, want %q", path, prefix, "/gitroot/")
		}
	}
}

func TestParseArgsWithWrongArgumentCount(t *testing.T) {
	for _, args := range [][]string{nil, {"gitroot", "/", "extra"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q): expected an error", args)
		}
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
