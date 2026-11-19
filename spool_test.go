package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"testing"
)

// readSpool returns the data of a spool.
func readSpool(t *testing.T, s *spool) []byte {
	t.Helper()

	content, err := s.reader()
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(content)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// fillSpool writes size bytes to a spool, by small writes as the services do, and returns them.
func fillSpool(t *testing.T, s *spool, size int) []byte {
	t.Helper()

	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}

	for chunk := range slices.Chunk(data, 1000) {
		if _, err := s.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}

	return data
}

func TestSpoolKeepsSmallDataInMemory(t *testing.T) {
	s := &spool{}
	defer s.remove()

	data := fillSpool(t, s, spoolMemory)
	if s.file != nil {
		t.Errorf("a spool of %d bytes moved to a file", spoolMemory)
	}

	if s.size != int64(len(data)) || !bytes.Equal(readSpool(t, s), data) {
		t.Errorf("spool data: size %d, want %d bytes back", s.size, len(data))
	}
}

func TestSpoolMovesLargeDataToFile(t *testing.T) {
	s := &spool{}
	data := fillSpool(t, s, spoolMemory+10_000)
	if s.file == nil {
		t.Fatalf("a spool of %d bytes stayed in memory", len(data))
	}

	name := s.file.Name()
	if s.size != int64(len(data)) || !bytes.Equal(readSpool(t, s), data) {
		t.Errorf("spool data: size %d, want %d bytes back", s.size, len(data))
	}

	if err := s.remove(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file of the spool remains after remove (stat error: %v)", err)
	}
}

func TestSpoolReset(t *testing.T) {
	s := &spool{}
	defer s.remove()

	fillSpool(t, s, spoolMemory+10_000)
	name := s.file.Name()
	if err := s.reset(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file of the spool remains after reset (stat error: %v)", err)
	}

	if _, err := s.Write([]byte("anew")); err != nil {
		t.Fatal(err)
	}

	if got := readSpool(t, s); s.size != 4 || string(got) != "anew" {
		t.Errorf("spool after reset and write: size %d, data %q; want 4 bytes, %q", s.size, got, "anew")
	}
}
