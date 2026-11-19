package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
)

// spoolMemory is the size up to which a spool keeps its data in memory, before moving it to a temporary file.
const spoolMemory = 4 << 20

// spool stores data until it is read back: in memory, then in a temporary file once it outgrows spoolMemory.
// The server receives a whole request into a spool before locking the store, and sends the response from a spool
// once the store is unlocked, so that a slow client never keeps the store locked.
type spool struct {
	// size is the number of bytes written to the spool.
	size   int64
	memory bytes.Buffer
	file   *os.File
	// buffer gathers the writes to the file, which may come by a few bytes at a time.
	buffer *bufio.Writer
}

// Write appends p to the data of the spool.
func (s *spool) Write(p []byte) (int, error) {
	if s.file == nil && s.memory.Len()+len(p) > spoolMemory {
		if err := s.moveToFile(); err != nil {
			return 0, err
		}
	}

	var n int
	var err error
	if s.file == nil {
		n, err = s.memory.Write(p)
	} else {
		n, err = s.buffer.Write(p)
	}

	s.size += int64(n)
	return n, err
}

// moveToFile moves the data of the spool to a temporary file, which receives the next writes.
func (s *spool) moveToFile() error {
	file, err := os.CreateTemp("", "gitserve-*.spool")
	if err != nil {
		return err
	}

	s.file, s.buffer = file, bufio.NewWriterSize(file, 64<<10)
	_, err = s.memory.WriteTo(s.buffer)
	s.memory = bytes.Buffer{}

	return err
}

// reader returns a reader of the data of the spool, from its start. The writes must be over.
func (s *spool) reader() (io.Reader, error) {
	if s.file == nil {
		return bytes.NewReader(s.memory.Bytes()), nil
	}

	if err := s.buffer.Flush(); err != nil {
		return nil, err
	}

	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	return s.file, nil
}

// reset discards the data of the spool, which can then be written anew.
func (s *spool) reset() error {
	err := s.remove()
	s.memory, s.size = bytes.Buffer{}, 0

	return err
}

// remove deletes the temporary file of the spool, if any.
func (s *spool) remove() error {
	if s.file == nil {
		return nil
	}

	file := s.file
	s.file, s.buffer = nil, nil

	err := file.Close()
	if removeErr := os.Remove(file.Name()); err == nil {
		err = removeErr
	}

	return err
}
