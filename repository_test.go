package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// newRepositories returns an in-memory filesystem holding empty repositories with the given names.
func newRepositories(t *testing.T, names ...string) billy.Filesystem {
	t.Helper()

	fsys := memfs.New()
	for _, name := range names {
		if err := createRepository(fsys, name, "main"); err != nil {
			t.Fatal(err)
		}
	}

	return fsys
}

func TestCheckName(t *testing.T) {
	for _, name := range []string{"demo", "demo.git", "team/demo", ".dotfiles", "my repo", "été"} {
		if err := checkName(name); err != nil {
			t.Errorf("checkName(%q): %v", name, err)
		}
	}

	for _, name := range []string{"", ".", "..", "/demo", "demo/", "team//demo", "team/../demo", "./demo", `team\demo`,
		"\xff"} {
		if err := checkName(name); !errors.Is(err, errInvalidName) {
			t.Errorf("checkName(%q): error %v, want errInvalidName", name, err)
		}
	}
}

func TestCreateRepository(t *testing.T) {
	fsys := newRepositories(t)
	if err := createRepository(fsys, "team/demo.git", "trunk"); err != nil {
		t.Fatal(err)
	}

	if !isRepository(fsys, "team/demo.git") {
		t.Fatal("no repository after createRepository")
	}

	if head, err := util.ReadFile(fsys, "team/demo.git/HEAD"); err != nil || string(head) != "ref: refs/heads/trunk\n" {
		t.Errorf("HEAD = (%q, %v), want a reference to the trunk branch", head, err)
	}

	config, err := util.ReadFile(fsys, "team/demo.git/config")
	if err != nil || !strings.Contains(string(config), "bare = true") {
		t.Errorf("config = (%q, %v), want a bare repository", config, err)
	}
}

func TestCreateRepositoryRefusesConflicts(t *testing.T) {
	fsys := newRepositories(t, "demo.git", "team/project")
	if err := util.WriteFile(fsys, "file", nil, 0o666); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		want string
	}{
		{name: "demo.git", want: "repository demo.git already exists"},
		// The URL of demo already serves demo.git.
		{name: "demo", want: "repository demo.git already exists"},
		{name: "file", want: "file already exists"},
		{name: "team", want: "team already exists"},
		{name: "team/project/nested", want: "team/project/nested would be inside repository team/project"},
		{name: "../escape", want: "invalid repository name"},
	}

	for _, tt := range tests {
		if err := createRepository(fsys, tt.name, "main"); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("createRepository(%q): error %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

func TestFindRepository(t *testing.T) {
	fsys := newRepositories(t, "demo.git", "plain", "team/tools")
	if err := util.WriteFile(fsys, "notes/HEAD", nil, 0o666); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
		err  error
	}{
		{name: "demo", dir: "demo.git"},
		{name: "demo.git", dir: "demo.git"},
		{name: "plain", dir: "plain"},
		{name: "team/tools", dir: "team/tools"},
		{name: "team", err: transport.ErrRepositoryNotFound},
		{name: "notes", err: transport.ErrRepositoryNotFound},
		{name: "missing", err: transport.ErrRepositoryNotFound},
		{name: "../demo", err: errInvalidName},
	}

	for _, tt := range tests {
		dir, err := findRepository(fsys, tt.name)
		if dir != tt.dir || !errors.Is(err, tt.err) {
			t.Errorf("findRepository(%q) = (%q, %v), want (%q, %v)", tt.name, dir, err, tt.dir, tt.err)
		}
	}
}

func TestDeleteRepository(t *testing.T) {
	fsys := newRepositories(t, "demo.git")
	if err := util.WriteFile(fsys, "notes/readme.txt", nil, 0o666); err != nil {
		t.Fatal(err)
	}

	if dir, err := deleteRepository(fsys, "demo"); err != nil || dir != "demo.git" {
		t.Fatalf("deleteRepository(demo) = (%q, %v), want (demo.git, nil)", dir, err)
	}

	if _, err := fsys.Stat("demo.git"); err == nil {
		t.Error("the repository still exists after deleteRepository")
	}

	// A directory that is not a repository is left alone.
	if _, err := deleteRepository(fsys, "notes"); !errors.Is(err, transport.ErrRepositoryNotFound) {
		t.Errorf("deleteRepository(notes): error %v, want ErrRepositoryNotFound", err)
	}

	if _, err := fsys.Stat("notes/readme.txt"); err != nil {
		t.Errorf("deleteRepository(notes) removed a directory that is not a repository: %v", err)
	}
}

func TestListRepositories(t *testing.T) {
	fsys := newRepositories(t, "b", "a.git", "team/z", "team/project")
	if err := fsys.MkdirAll("empty/dir", 0o777); err != nil {
		t.Fatal(err)
	}

	// A repository inside another one, like a submodule, is not listed.
	for _, name := range []string{"a.git/modules/sub/HEAD", "a.git/modules/sub/config"} {
		if err := util.WriteFile(fsys, name, nil, 0o666); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"a.git", "b", "team/project", "team/z"}
	if names, err := listRepositories(fsys); err != nil || !slices.Equal(names, want) {
		t.Errorf("listRepositories = (%q, %v), want %q", names, err, want)
	}
}
