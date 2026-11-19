package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6/util"
)

// writeZip writes a zip archive at path holding the given files, by name. A name ending with a slash is a directory,
// whose content is ignored.
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	archive := zip.NewWriter(f)
	for name, content := range files {
		w, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if strings.HasSuffix(name, "/") {
			continue
		}

		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	image, err := newArchiveImage()
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"demo/HEAD":                      "ref: refs/heads/main\n",
		"demo/objects/pack/pack-1.pack":  "PACK",
		"team/tools/objects/12/3456789a": "loose object",
	}
	for name, content := range files {
		if err := util.WriteFile(image, name, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	if err := image.MkdirAll("demo/refs/heads", 0o777); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "repos.zip")
	if _, err := writeArchive(path, image); err != nil {
		t.Fatal(err)
	}

	// Git objects are stored as they are, the other files are deflated, and directories get entries of their own.
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	methods := map[string]uint16{}
	for _, entry := range reader.File {
		methods[entry.Name] = entry.Method
	}

	wantMethods := map[string]uint16{
		markerName:                       zip.Deflate,
		"demo/HEAD":                      zip.Deflate,
		"demo/objects/pack/pack-1.pack":  zip.Store,
		"team/tools/objects/12/3456789a": zip.Store,
		"demo/refs/heads/":               zip.Store,
	}
	for name, want := range wantMethods {
		if got, ok := methods[name]; !ok || got != want {
			t.Errorf("entry %q: method %d (present: %v), want %d", name, got, ok, want)
		}
	}

	loaded, _, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range files {
		if got, err := util.ReadFile(loaded, name); err != nil || string(got) != want {
			t.Errorf("file %q after a round trip: (%q, %v), want %q", name, got, err, want)
		}
	}

	if info, err := loaded.Stat("demo/refs/heads"); err != nil || !info.IsDir() {
		t.Errorf("empty directory after a round trip: (%v, %v), want a directory", info, err)
	}
}

func TestReadArchiveWithoutMarker(t *testing.T) {
	// A zip made by hand from a directory store, with Windows path separators.
	path := filepath.Join(t.TempDir(), "repos.zip")
	writeZip(t, path, map[string]string{
		"demo/HEAD":               "ref: refs/heads/main\n",
		"demo/config":             "[core]\n\tbare = true\n",
		`demo\refs\heads\main`:    "0123456789012345678901234567890123456789\n",
		"notes/readme.txt":        "not a repository",
		"team/project.git/HEAD":   "ref: refs/heads/main\n",
		"team/project.git/config": "[core]\n\tbare = true\n",
	})

	image, _, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}

	if names, err := listRepositories(image); err != nil || !slices.Equal(names, []string{"demo", "team/project.git"}) {
		t.Errorf("repositories = (%q, %v), want [demo team/project.git]", names, err)
	}

	if _, err := image.Stat("demo/refs/heads/main"); err != nil {
		t.Errorf("entry with Windows path separators: %v", err)
	}

	// The marker is added, and reaches the file with the next write.
	if err := checkMarker(image); err != nil {
		t.Errorf("marker added to the image: %v", err)
	}
}

func TestReadArchiveRefusesUnknownFormat(t *testing.T) {
	tests := []struct {
		marker string
		want   string
	}{
		{marker: `{"version":2}`, want: "unsupported archive format version 2"},
		{marker: "garbage", want: "invalid .gitserve marker"},
	}

	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "repos.zip")
		writeZip(t, path, map[string]string{markerName: tt.marker})

		if _, _, err := readArchive(path); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("marker %q: error %v, want an error containing %q", tt.marker, err, tt.want)
		}
	}
}

func TestReadArchiveRefusesInvalidEntryNames(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "demo//HEAD", "demo/./HEAD", "./"} {
		path := filepath.Join(t.TempDir(), "repos.zip")
		writeZip(t, path, map[string]string{name: "content"})

		if _, _, err := readArchive(path); err == nil || !strings.Contains(err.Error(), "invalid entry name") {
			t.Errorf("entry %q: error %v, want an invalid entry name error", name, err)
		}
	}
}

func TestWriteArchiveLeavesNoTemporaryFile(t *testing.T) {
	image, err := newArchiveImage()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if _, err := writeArchive(filepath.Join(dir, "repos.zip"), image); err != nil {
		t.Fatal(err)
	}

	// Replacing a directory fails once the temporary file is written: it must be removed.
	if err := os.Mkdir(filepath.Join(dir, "taken.zip"), 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := writeArchive(filepath.Join(dir, "taken.zip"), image); err == nil {
		t.Error("writeArchive over a directory: no error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	if !slices.Equal(names, []string{"repos.zip", "taken.zip"}) {
		t.Errorf("files next to the archives: %q, want [repos.zip taken.zip]", names)
	}
}
