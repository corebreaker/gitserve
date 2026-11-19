package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
)

const (
	// markerName is the file, at the root of an archive, that records the version of the archive format.
	markerName = ".gitserve"
	// archiveVersion is the version of the archive format that gitserve reads and writes.
	archiveVersion = 1
)

// archiveMarker is the content of the marker file.
type archiveMarker struct {
	Version int `json:"version"`
}

// newArchiveImage returns the in-memory image of an empty archive: nothing but the marker.
func newArchiveImage() (billy.Filesystem, error) {
	image := memfs.New()
	if err := writeMarker(image); err != nil {
		return nil, err
	}

	return image, nil
}

// writeMarker writes the marker of the current archive format at the root of image.
func writeMarker(image billy.Filesystem) error {
	data, err := json.Marshal(archiveMarker{Version: archiveVersion})
	if err != nil {
		return err
	}

	return util.WriteFile(image, markerName, append(data, '\n'), 0o666)
}

// checkMarker checks that the archive format recorded in image is the one gitserve knows.
// An archive without marker, such as a zip made by hand from a directory store, is in the current format:
// the marker is added to its image, and reaches the file with the next write.
func checkMarker(image billy.Filesystem) error {
	data, err := util.ReadFile(image, markerName)
	if errors.Is(err, fs.ErrNotExist) {
		return writeMarker(image)
	} else if err != nil {
		return err
	}

	var marker archiveMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("invalid %s marker: %w", markerName, err)
	}

	if marker.Version != archiveVersion {
		return fmt.Errorf("unsupported archive format version %d (gitserve handles version %d)",
			marker.Version, archiveVersion)
	}

	return nil
}

// readArchive loads the archive at path in memory. It returns its image, and the stamp of the file it read.
func readArchive(path string) (billy.Filesystem, fileStamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fileStamp{}, err
	}

	reader, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("%s: %w", path, err)
	}

	image := memfs.New()
	for _, entry := range reader.File {
		if err := extractEntry(image, entry); err != nil {
			return nil, fileStamp{}, fmt.Errorf("%s: %s: %w", path, entry.Name, err)
		}
	}

	if err := checkMarker(image); err != nil {
		return nil, fileStamp{}, fmt.Errorf("%s: %w", path, err)
	}

	return image, stampOf(info), nil
}

// extractEntry copies an entry of a zip archive into image.
func extractEntry(image billy.Filesystem, entry *zip.File) error {
	// Some zip tools write Windows path separators.
	name := strings.TrimSuffix(strings.ReplaceAll(entry.Name, `\`, "/"), "/")
	if name == "." || !fs.ValidPath(name) {
		return errors.New("invalid entry name")
	}

	if entry.FileInfo().IsDir() {
		return image.MkdirAll(name, 0o777)
	}

	src, err := entry.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := image.Create(name)
	if err != nil {
		return err
	}

	_, err = io.Copy(dst, src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}

	return err
}

// writeArchive writes image to the archive at path, and returns the stamp of the written file.
// The archive is first written to a temporary file next to it, that then replaces it:
// an interrupted write leaves the previous archive intact.
func writeArchive(path string, image billy.Filesystem) (fileStamp, error) {
	perm := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fileStamp{}, err
	}

	err = zipImage(tmp, image)
	if err == nil {
		err = tmp.Sync()
	}

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Chmod(tmp.Name(), perm)
	}

	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		os.Remove(tmp.Name())
		return fileStamp{}, err
	}

	return statFile(path)
}

// zipImage writes every directory and file of image to w, as a zip archive.
// Git objects are stored as they are, since Git already compresses them. The other files are deflated.
func zipImage(w io.Writer, image billy.Filesystem) error {
	archive := zip.NewWriter(w)
	err := util.Walk(image, "", func(name string, info fs.FileInfo, err error) error {
		if err != nil || name == "" {
			return err
		}

		header := &zip.FileHeader{Name: filepath.ToSlash(name), Modified: info.ModTime()}
		if info.IsDir() {
			header.Name += "/"
			header.SetMode(fs.ModeDir | 0o755)
			_, err := archive.CreateHeader(header)

			return err
		}

		header.SetMode(0o644)
		header.Method = zip.Deflate
		if strings.Contains("/"+header.Name, "/objects/") {
			header.Method = zip.Store
		}

		dst, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		src, err := image.Open(name)
		if err != nil {
			return err
		}
		defer src.Close()

		_, err = io.Copy(dst, src)
		return err
	})
	if err != nil {
		return err
	}

	return archive.Close()
}
