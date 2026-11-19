package main

import (
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// gitClient runs the git command line client, isolated from the configuration of the machine.
type gitClient struct {
	t   *testing.T
	env []string
}

// newGitClient returns a git client, or skips the test when git is not installed, except on CI.
func newGitClient(t *testing.T) *gitClient {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is required on CI")
		}

		t.Skip("git is not installed")
	}

	config := filepath.Join(t.TempDir(), "gitconfig")
	content := "[user]\n\tname = gitserve\n\temail = gitserve@example.com\n" +
		"[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n"
	if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Variables such as GIT_DIR, set when the tests run from a Git hook, would redirect the commands.
	var env []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "GIT_") {
			env = append(env, variable)
		}
	}

	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+config, "GIT_TERMINAL_PROMPT=0")
	return &gitClient{t: t, env: env}
}

// try runs git with args in dir, and returns its trimmed output.
func (g *gitClient) try(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// run runs git with args in dir, and returns its trimmed output. It fails the test if git fails.
func (g *gitClient) run(dir string, args ...string) string {
	g.t.Helper()

	out, err := g.try(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// commit makes n commits in the clone at dir, each of them adding a line to the file name.
func (g *gitClient) commit(dir, name string, n int) {
	g.t.Helper()

	for range n {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			g.t.Fatal(err)
		}

		_, err = fmt.Fprintln(f, "one more line")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}

		if err != nil {
			g.t.Fatal(err)
		}

		g.run(dir, "add", name)
		g.run(dir, "commit", "-q", "-m", "Change "+name)
	}
}

// TestGitClientEndToEnd drives the git client against the server, for each kind of store.
func TestGitClientEndToEnd(t *testing.T) {
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) {
			g := newGitClient(t)

			log.SetOutput(io.Discard)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			st := newTestStore(t, kind.ext)
			if _, err := st.createRepositories([]string{"demo.git"}, "main"); err != nil {
				t.Fatal(err)
			}

			server := httptest.NewServer(&requestLogger{next: &gitHandler{store: st, prefix: "/git/"}})
			defer server.Close()

			// The repository is served without its .git suffix too.
			url := server.URL + "/git/demo"
			work := t.TempDir()
			first, second := filepath.Join(work, "first"), filepath.Join(work, "second")

			g.run(work, "clone", "-q", url, first)
			g.commit(first, "first.txt", 3)
			g.run(first, "push", "-q", "origin", "main")

			g.run(work, "clone", "-q", url, second)
			if count := g.run(second, "rev-list", "--count", "HEAD"); count != "3" {
				t.Fatalf("second clone: %s commits, want 3", count)
			}

			// The first clone gets local commits that the server does not know: the negotiation of its fetches
			// takes several rounds, each one an HTTP request of its own, before reaching commits in common.
			g.run(first, "switch", "-q", "-c", "local")
			for i := range 20 {
				g.run(first, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("local %d", i))
			}

			var head string
			for _, version := range []string{"2", "0"} {
				g.commit(second, "second.txt", 1)
				g.run(second, "push", "-q", "origin", "main")
				head = g.run(second, "rev-parse", "HEAD")

				g.run(first, "-c", "protocol.version="+version, "fetch", "-q", "origin")
				if fetched := g.run(first, "rev-parse", "origin/main"); fetched != head {
					t.Fatalf("protocol version %s: fetched main at %s, want %s", version, fetched, head)
				}
			}

			g.run(second, "tag", "v1")
			g.run(second, "push", "-q", "origin", "v1")
			g.run(first, "fetch", "-q", "--tags", "origin")
			if tag := g.run(first, "rev-parse", "v1"); tag != head {
				t.Fatalf("fetched tag v1 at %s, want %s", tag, head)
			}

			// Push a new branch, then delete it.
			g.run(first, "push", "-q", "origin", "local:feature")
			feature := g.run(first, "rev-parse", "local")
			want := feature + "\trefs/heads/feature\n" + head + "\trefs/heads/main"
			if refs := g.run(work, "ls-remote", "--heads", url); refs != want {
				t.Fatalf("remote branches after pushing feature:\n%s\nwant:\n%s", refs, want)
			}

			g.run(second, "push", "-q", "origin", "--delete", "feature")
			if refs := g.run(work, "ls-remote", "--heads", url); refs != head+"\trefs/heads/main" {
				t.Fatalf("remote branches after deleting feature:\n%s", refs)
			}

			// Concurrent clones read the store together.
			var wg sync.WaitGroup
			errs := make([]error, 3)
			for i := range errs {
				wg.Go(func() {
					out, err := g.try(work, "clone", "-q", url, filepath.Join(work, fmt.Sprintf("concurrent-%d", i)))
					if err != nil {
						errs[i] = fmt.Errorf("%w: %s", err, out)
					}
				})
			}

			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent clone %d: %v", i, err)
				}
			}

			out, err := g.try(work, "ls-remote", server.URL+"/git/missing")
			if err == nil || !strings.Contains(out, "not found") {
				t.Errorf("ls-remote of a missing repository: error %v, output %q; want a not found error", err, out)
			}

			// The pushes reached the store: open it anew, as a new process would.
			if main := readBranch(t, st.path, "demo", "main"); main != head {
				t.Errorf("main in the reopened store: %s, want %s", main, head)
			}
		})
	}
}

// readBranch returns the commit of a branch of a repository, from the store at path opened anew.
func readBranch(t *testing.T, path, repository, branch string) string {
	t.Helper()

	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	var hash string
	err = st.view(func(fsys billy.Filesystem) error {
		storage, err := openRepository(fsys, repository)
		if err != nil {
			return err
		}
		defer storage.Close()

		ref, err := storage.Reference(plumbing.NewBranchReferenceName(branch))
		if err != nil {
			return err
		}

		hash = ref.Hash().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return hash
}
