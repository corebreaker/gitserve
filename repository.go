package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/util"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// errInvalidName is returned for a name that cannot name a repository.
var errInvalidName = errors.New("invalid repository name")

// checkName checks that name can name a repository: a relative path, whose slash-separated elements are neither
// empty nor "." or "..", and contain no backslash.
func checkName(name string) error {
	if name == "." || !fs.ValidPath(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("%w: %q", errInvalidName, name)
	}

	return nil
}

// isRepository reports whether dir holds a bare Git repository.
func isRepository(fsys billy.Filesystem, dir string) bool {
	for _, name := range []string{"HEAD", "config"} {
		info, err := fsys.Stat(fsys.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}

	return true
}

// findRepository returns the directory of the repository served under name: name itself, or else name followed by
// a ".git" suffix.
func findRepository(fsys billy.Filesystem, name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}

	for _, dir := range []string{name, name + ".git"} {
		if isRepository(fsys, dir) {
			return dir, nil
		}
	}

	return "", fmt.Errorf("%w: %s", transport.ErrRepositoryNotFound, name)
}

// openRepository opens the storage of the repository served under name. The caller closes it.
func openRepository(fsys billy.Filesystem, name string) (*filesystem.Storage, error) {
	dir, err := findRepository(fsys, name)
	if err != nil {
		return nil, err
	}

	repoFS, err := fsys.Chroot(dir)
	if err != nil {
		return nil, err
	}

	return filesystem.NewStorage(repoFS, cache.NewObjectLRUDefault()), nil
}

// createRepository creates an empty bare repository named name, with HEAD pointing to branch.
func createRepository(fsys billy.Filesystem, name, branch string) error {
	if err := checkName(name); err != nil {
		return err
	}

	if dir, err := findRepository(fsys, name); err == nil {
		return fmt.Errorf("repository %s already exists", dir)
	}

	if _, err := fsys.Lstat(name); err == nil {
		return fmt.Errorf("%s already exists", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		if isRepository(fsys, dir) {
			return fmt.Errorf("%s would be inside repository %s", name, dir)
		}
	}

	repoFS, err := fsys.Chroot(name)
	if err != nil {
		return err
	}

	storage := filesystem.NewStorage(repoFS, cache.NewObjectLRUDefault())
	repo, err := git.Init(storage, git.WithDefaultBranch(plumbing.NewBranchReferenceName(branch)))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	return repo.Close()
}

// deleteRepository deletes the repository served under name, and returns its directory.
func deleteRepository(fsys billy.Filesystem, name string) (string, error) {
	dir, err := findRepository(fsys, name)
	if err != nil {
		return "", err
	}

	return dir, util.RemoveAll(fsys, dir)
}

// listRepositories returns the directories of the repositories under the root of fsys, in lexical order.
// It does not look for repositories inside other ones.
func listRepositories(fsys billy.Filesystem) ([]string, error) {
	var dirs []string
	err := util.Walk(fsys, "", func(name string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if name == "" || !info.IsDir() || !isRepository(fsys, name) {
			return nil
		}

		dirs = append(dirs, filepath.ToSlash(name))
		return filepath.SkipDir
	})

	slices.Sort(dirs)
	return dirs, err
}
