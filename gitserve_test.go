package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/util"
)

// runCLI runs the command line made of args, and returns its exit status and outputs.
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	status := run(args, &stdout, &stderr)

	return status, stdout.String(), stderr.String()
}

// syncBuffer is a bytes.Buffer safe for concurrent use, to capture the logs of a server running in a goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestNormalizePrefix(t *testing.T) {
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
		if got := normalizePrefix(tt.prefix); got != tt.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

func TestRunWithoutCommandShowsHelp(t *testing.T) {
	status, stdout, _ := runCLI()
	if status != 0 || !strings.Contains(stdout, "Available Commands") {
		t.Errorf("gitserve: status %d, output %q; want status 0 and the help", status, stdout)
	}
}

func TestRunReportsUsageErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gitroot")
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"unknown"}, want: `unknown command "unknown"`},
		{args: []string{"serve"}, want: "accepts 1 arg(s), received 0"},
		{args: []string{"serve", root, "/"}, want: "accepts 1 arg(s), received 2"},
		{args: []string{"serve", "--port", "65536", root}, want: "invalid port 65536"},
		{args: []string{"serve", "--port", "http", root}, want: `invalid argument "http"`},
		{args: []string{"init"}, want: "accepts 1 arg(s), received 0"},
		{args: []string{"create", root}, want: "requires at least 2 arg(s), only received 1"},
		{args: []string{"create", "--branch", "a..b", root, "demo"}, want: `invalid branch name "a..b"`},
		{args: []string{"delete", root}, want: "requires at least 2 arg(s), only received 1"},
		{args: []string{"list", "--unknown", root}, want: "unknown flag: --unknown"},
	}

	for _, tt := range tests {
		status, _, stderr := runCLI(tt.args...)
		if status != 2 || !strings.Contains(stderr, tt.want) || !strings.Contains(stderr, "--help' for usage") {
			t.Errorf("gitserve %s: status %d, error output %q; want status 2 and an error containing %q",
				strings.Join(tt.args, " "), status, stderr, tt.want)
		}
	}

	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a command ran despite a usage error, and created the store (stat error: %v)", err)
	}
}

func TestRunInitCreatesEmptyStore(t *testing.T) {
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing", "parent", "store"+kind.ext)
			status, stdout, stderr := runCLI("init", path)
			if status != 0 || stdout != "Initialized empty store "+path+"\n" {
				t.Fatalf("init: status %d, output %q, error output %q", status, stdout, stderr)
			}

			if status, stdout, stderr := runCLI("list", path); status != 0 || stdout != "" {
				t.Errorf("list: status %d, output %q, error output %q; want an empty list", status, stdout, stderr)
			}

			if status, _, stderr := runCLI("init", path); status != 1 || !strings.Contains(stderr, "already exists") {
				t.Errorf("second init: status %d, error output %q; want status 1 and an error", status, stderr)
			}
		})
	}
}

func TestRunManagesRepositories(t *testing.T) {
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store"+kind.ext)

			// Creating repositories initializes the missing store.
			status, stdout, stderr := runCLI("create", path, "demo.git", "team/tools")
			want := "Initialized empty store " + path + "\nCreated repository demo.git\nCreated repository team/tools\n"
			if status != 0 || stdout != want {
				t.Fatalf("create: status %d, output %q, error output %q; want output %q", status, stdout, stderr, want)
			}

			// A failure on a repository does not prevent creating the others.
			status, stdout, stderr = runCLI("create", "-b", "trunk", path, "demo", "other")
			if status != 1 || stdout != "Created repository other\n" ||
				!strings.Contains(stderr, "gitserve: repository demo.git already exists") {
				t.Fatalf("create with a conflict: status %d, output %q, error output %q", status, stdout, stderr)
			}

			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}

			if head := readStoreFile(t, st, "other/HEAD"); head != "ref: refs/heads/trunk\n" {
				t.Errorf("HEAD of the repository created with --branch trunk: %q", head)
			}

			if status, stdout, _ := runCLI("list", path); status != 0 || stdout != "demo.git\nother\nteam/tools\n" {
				t.Fatalf("list: status %d, output %q", status, stdout)
			}

			// As the server does, delete finds a repository by its name without the .git suffix.
			status, stdout, stderr = runCLI("delete", path, "demo", "missing")
			if status != 1 || stdout != "Deleted repository demo.git\n" ||
				!strings.Contains(stderr, "gitserve: repository not found: missing") {
				t.Fatalf("delete: status %d, output %q, error output %q", status, stdout, stderr)
			}

			if status, stdout, _ := runCLI("list", path); status != 0 || stdout != "other\nteam/tools\n" {
				t.Errorf("list after delete: status %d, output %q", status, stdout)
			}
		})
	}
}

func TestRunFailsOnMissingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.zip")
	for _, args := range [][]string{{"list", path}, {"delete", path, "demo"}} {
		status, _, stderr := runCLI(args...)
		if status != 1 || !strings.Contains(stderr, "does not exist") {
			t.Errorf("gitserve %s: status %d, error output %q; want status 1 and an error",
				strings.Join(args, " "), status, stderr)
		}
	}

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a command created the missing store (stat error: %v)", err)
	}
}

func TestRunServerInitializesAndServesStore(t *testing.T) {
	logs := &syncBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := filepath.Join(t.TempDir(), "repos.zip")
	stop := make(chan os.Signal, 1)
	served := make(chan error, 1)
	go func() { served <- runServer(path, "127.0.0.1:0", "/git/", stop) }()

	port := waitForLog(t, logs, regexp.MustCompile(`on port (\d+)`))
	if !strings.Contains(logs.String(), "Initialized empty store "+path) {
		t.Errorf("logs %q do not report the initialization of the store", logs.String())
	}

	// A repository created by another command while the server is running gets served.
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.createRepositories([]string{"demo"}, "main"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://127.0.0.1:" + port + "/git/demo/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("references of the new repository: status %d, want %d", resp.StatusCode, http.StatusOK)
	}

	stop <- os.Interrupt

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return after the signal")
	}
}

// waitForLog waits for pattern to match logs, and returns its first submatch.
func waitForLog(t *testing.T, logs *syncBuffer, pattern *regexp.Regexp) string {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := pattern.FindStringSubmatch(logs.String()); match != nil {
			return match[1]
		}
	}

	t.Fatalf("logs %q do not match %q", logs.String(), pattern)
	return ""
}

// readStoreFile returns the content of the file at name in the store.
func readStoreFile(t *testing.T, st *store, name string) string {
	t.Helper()

	var data []byte
	err := st.view(func(fsys billy.Filesystem) error {
		var err error
		data, err = util.ReadFile(fsys, name)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}
