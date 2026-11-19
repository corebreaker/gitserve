package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
)

// storeKinds are the two kinds of stores, told apart by the extension of their path.
var storeKinds = []struct {
	name string
	ext  string
}{
	{name: "directory", ext: ""},
	{name: "archive", ext: ".zip"},
}

// newTestStore initializes and opens an empty store, of the kind given by the extension ext.
func newTestStore(t *testing.T, ext string) *store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "store"+ext)
	if err := initStore(path); err != nil {
		t.Fatal(err)
	}

	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	return st
}

// newMemoryStore returns a store whose repositories live in memory only.
func newMemoryStore() *store {
	return &store{path: "memory", fs: memfs.New()}
}

// listStore returns the repositories of the store at path, opened anew as another process would.
func listStore(t *testing.T, path string) []string {
	t.Helper()

	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	names, err := st.repositories()
	if err != nil {
		t.Fatal(err)
	}

	return names
}

func TestIsArchive(t *testing.T) {
	for path, want := range map[string]bool{
		"repos.zip":        true,
		"REPOS.ZIP":        true,
		"usb/repos.zip":    true,
		"gitroot":          false,
		"repos.zip.d":      false,
		"repos.git":        false,
		"archive.tar.gz":   false,
		"usb/zip/gitroot":  false,
		"usb/.zip/gitroot": false,
	} {
		if got := isArchive(path); got != want {
			t.Errorf("isArchive(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestInitStoreRefusesExistingPath(t *testing.T) {
	for _, kind := range storeKinds {
		path := filepath.Join(t.TempDir(), "store"+kind.ext)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		if err := initStore(path); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("initStore(%q) on an existing file: error %v, want an error", path, err)
		}
	}
}

func TestEnsureStoreKeepsExistingStore(t *testing.T) {
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store"+kind.ext)
			if created, err := ensureStore(path); err != nil || !created {
				t.Fatalf("ensureStore on a missing store = (%v, %v), want (true, nil)", created, err)
			}

			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := st.createRepositories([]string{"demo"}, "main"); err != nil {
				t.Fatal(err)
			}

			if created, err := ensureStore(path); err != nil || created {
				t.Fatalf("ensureStore on an existing store = (%v, %v), want (false, nil)", created, err)
			}

			if names := listStore(t, path); !slices.Equal(names, []string{"demo"}) {
				t.Errorf("repositories after ensureStore: %q, want [demo]", names)
			}
		})
	}
}

func TestOpenStoreErrors(t *testing.T) {
	dir := t.TempDir()
	notZip := filepath.Join(dir, "repos.zip")
	plainFile := filepath.Join(dir, "gitroot")
	for _, path := range []string{notZip, plainFile} {
		if err := os.WriteFile(path, []byte("not a store"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		path string
		want string
	}{
		{path: filepath.Join(dir, "missing"), want: "does not exist"},
		{path: notZip, want: "not a valid zip file"},
		{path: plainFile, want: "neither a directory nor a zip archive"},
	}

	for _, tt := range tests {
		if _, err := openStore(tt.path); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("openStore(%q): error %v, want an error containing %q", tt.path, err, tt.want)
		}
	}
}

func TestStorePersistsChanges(t *testing.T) {
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) {
			st := newTestStore(t, kind.ext)
			if _, err := st.createRepositories([]string{"demo.git", "team/tools"}, "main"); err != nil {
				t.Fatal(err)
			}

			want := []string{"demo.git", "team/tools"}
			if names := listStore(t, st.path); !slices.Equal(names, want) {
				t.Fatalf("repositories after create: %q, want %q", names, want)
			}

			dirs, err := st.deleteRepositories([]string{"demo"})
			if err != nil || !slices.Equal(dirs, []string{"demo.git"}) {
				t.Fatalf("deleteRepositories(demo) = (%q, %v), want ([demo.git], nil)", dirs, err)
			}

			if names := listStore(t, st.path); !slices.Equal(names, []string{"team/tools"}) {
				t.Errorf("repositories after delete: %q, want [team/tools]", names)
			}
		})
	}
}

func TestStoreReportsEachFailure(t *testing.T) {
	st := newTestStore(t, ".zip")
	created, err := st.createRepositories([]string{"demo", "../escape", "demo", "other"}, "main")
	if !slices.Equal(created, []string{"demo", "other"}) {
		t.Errorf("created %q, want [demo other]", created)
	}

	if err == nil || !strings.Contains(err.Error(), "invalid repository name") ||
		!strings.Contains(err.Error(), "repository demo already exists") {
		t.Errorf("error %v, want both failures reported", err)
	}

	// The successful creations were saved despite the failures.
	if names := listStore(t, st.path); !slices.Equal(names, []string{"demo", "other"}) {
		t.Errorf("repositories in the archive: %q, want [demo other]", names)
	}
}

func TestStoreReloadsArchiveChangedOnDisk(t *testing.T) {
	server := newTestStore(t, ".zip")
	if _, err := server.createRepositories([]string{"first"}, "main"); err != nil {
		t.Fatal(err)
	}

	// Another gitserve command changes the archive while the server is running.
	command, err := openStore(server.path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := command.createRepositories([]string{"second"}, "main"); err != nil {
		t.Fatal(err)
	}

	names, err := server.repositories()
	if err != nil || !slices.Equal(names, []string{"first", "second"}) {
		t.Fatalf("server repositories = (%q, %v), want [first second]", names, err)
	}

	// The next change of the server keeps the repository created by the command.
	if _, err := server.createRepositories([]string{"third"}, "main"); err != nil {
		t.Fatal(err)
	}

	if names := listStore(t, server.path); !slices.Equal(names, []string{"first", "second", "third"}) {
		t.Errorf("repositories in the archive: %q, want [first second third]", names)
	}
}

func TestStoreDiscardsUnsavedChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usb")
	path := filepath.Join(dir, "repos.zip")
	if err := initStore(path); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// The archive cannot be written anymore, as when its USB key is removed.
	err = st.change(func(fsys billy.Filesystem) error {
		if err := createRepository(fsys, "demo", "main"); err != nil {
			return err
		}

		if err := os.RemoveAll(dir); err != nil {
			return err
		}

		return st.persist()
	})
	if err == nil {
		t.Fatal("persist without the directory of the archive: no error")
	}

	// The key is back, with the archive as it was: the change that could not be saved is not served.
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, saved, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if names, err := st.repositories(); err != nil || len(names) != 0 {
		t.Errorf("repositories after a failed save = (%q, %v), want none", names, err)
	}
}

func TestStoreFailsWhenArchiveDisappears(t *testing.T) {
	st := newTestStore(t, ".zip")
	if err := os.Remove(st.path); err != nil {
		t.Fatal(err)
	}

	if _, err := st.repositories(); err == nil {
		t.Error("repositories of a deleted archive: no error")
	}
}
