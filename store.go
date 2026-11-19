package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
)

// store holds the bare repositories that gitserve serves.
// It is either a directory, or a zip archive loaded in memory and written back to disk after each change.
type store struct {
	path    string
	archive bool

	// mu makes changes to the repositories exclusive, from each other and from reads.
	mu sync.RWMutex
	// fs is the root of the repositories: the directory itself, or the in-memory image of the archive.
	fs billy.Filesystem
	// stamp identifies the state of the archive file that fs reflects. It is unused for a directory.
	// The zero stamp matches no archive, whose file is never empty: it makes the next access reload the file.
	stamp fileStamp
}

// fileStamp identifies a state of a file: it changes whenever the file is written.
type fileStamp struct {
	size    int64
	modTime int64
}

// statFile returns the stamp of the file at path.
func statFile(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}

	return stampOf(info), nil
}

// stampOf returns the stamp of a file from its description.
func stampOf(info fs.FileInfo) fileStamp {
	return fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
}

// isArchive reports whether the store at path is a zip archive rather than a directory.
func isArchive(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".zip")
}

// initStore creates an empty store at path, along with its missing parent directories. The store must not exist yet.
// An empty archive holds nothing but the marker recording its format version.
func initStore(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if !isArchive(path) {
		return os.MkdirAll(path, 0o777)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}

	image, err := newArchiveImage()
	if err != nil {
		return err
	}

	_, err = writeArchive(path, image)
	return err
}

// ensureStore initializes an empty store at path, unless something already exists there. It reports whether it did.
func ensureStore(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	return true, initStore(path)
}

// openStore opens the existing store at path.
func openStore(path string) (*store, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist", path)
	} else if err != nil {
		return nil, err
	}

	s := &store{path: path, archive: isArchive(path)}
	switch {
	case s.archive:
		if err := s.load(); err != nil {
			return nil, err
		}
	case info.IsDir():
		s.fs = osfs.New(path)
	default:
		return nil, fmt.Errorf("%s is neither a directory nor a zip archive", path)
	}

	return s, nil
}

// load reads the archive into memory.
func (s *store) load() error {
	image, stamp, err := readArchive(s.path)
	if err != nil {
		return err
	}

	s.fs, s.stamp = image, stamp
	return nil
}

// upToDate reports whether the in-memory image of the archive reflects its file, which may have been written
// by another gitserve command while the server is running. It is always true for a directory.
func (s *store) upToDate() bool {
	if !s.archive {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	stamp, err := statFile(s.path)
	return err == nil && stamp == s.stamp
}

// reloadIfChanged reloads the archive when its file changed since it was last read or written.
// The caller holds mu for writing.
func (s *store) reloadIfChanged() error {
	if !s.archive {
		return nil
	}

	stamp, err := statFile(s.path)
	if err != nil || stamp == s.stamp {
		return err
	}

	return s.load()
}

// view calls fn with the root of the repositories, which fn must only read. Views run concurrently.
func (s *store) view(fn func(fsys billy.Filesystem) error) error {
	if !s.upToDate() {
		// Reloading the archive replaces its image: it needs exclusive access.
		s.mu.Lock()
		err := s.reloadIfChanged()
		s.mu.Unlock()

		if err != nil {
			return err
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return fn(s.fs)
}

// change calls fn with exclusive access to the root of the repositories.
// fn may modify the repositories, then calls persist for an archive file to record the modifications.
func (s *store) change(fn func(fsys billy.Filesystem) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.reloadIfChanged(); err != nil {
		return err
	}

	return fn(s.fs)
}

// persist writes the in-memory image of an archive back to its file, and does nothing for a directory.
// It may only be called by a function that change runs.
//
// When the write fails, the image holds changes that the file lacks, and that may never reach it:
// persist then has the next access reload the file, so that these changes are discarded rather than served.
func (s *store) persist() error {
	if !s.archive {
		return nil
	}

	stamp, err := writeArchive(s.path, s.fs)
	if err != nil {
		s.stamp = fileStamp{}
		return err
	}

	s.stamp = stamp
	return nil
}

// close waits for the change in progress, if any, and prevents any other from starting,
// so that the process can exit without interrupting the write of an archive.
func (s *store) close() {
	s.mu.Lock()
}

// repositories returns the names of the repositories of the store, in lexical order.
func (s *store) repositories() ([]string, error) {
	var names []string
	err := s.view(func(fsys billy.Filesystem) error {
		var err error
		names, err = listRepositories(fsys)

		return err
	})

	return names, err
}

// createRepositories creates empty bare repositories, with HEAD pointing to branch.
// It returns the names of the repositories it created, and the errors joined.
func (s *store) createRepositories(names []string, branch string) ([]string, error) {
	return s.apply(names, func(fsys billy.Filesystem, name string) (string, error) {
		return name, createRepository(fsys, name, branch)
	})
}

// deleteRepositories deletes the repositories served under names.
// It returns the directories of the repositories it deleted, and the errors joined.
func (s *store) deleteRepositories(names []string) ([]string, error) {
	return s.apply(names, deleteRepository)
}

// apply calls op on each name within a single change, going on after a failure, then persists the store
// if any call succeeded. It returns the results of the successful calls, unless persisting fails, and the errors
// joined.
func (s *store) apply(names []string, op func(fsys billy.Filesystem, name string) (string, error)) ([]string, error) {
	var done []string
	err := s.change(func(fsys billy.Filesystem) error {
		var errs []error
		for _, name := range names {
			result, err := op(fsys, name)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			done = append(done, result)
		}

		if len(done) > 0 {
			if err := s.persist(); err != nil {
				done = nil
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	})

	return done, err
}
