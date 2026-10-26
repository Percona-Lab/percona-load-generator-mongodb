package workloadrec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// MaxPeekHeaderBytes bounds the header line PeekRecording reads.
	MaxPeekHeaderBytes = 1 << 20
	// maxPeekTrailerBytes bounds the tail read for the trailer of an
	// uncompressed recording.
	maxPeekTrailerBytes = 256 << 10
)

// ErrNotRecording is returned by PeekRecording for a file that is empty,
// an outcome file, or not a workload recording at all.
var ErrNotRecording = errors.New("not a workload recording")

// Summary describes a recording from its header and, when it is cheap to
// read, its trailer. Unlike Scan it never reads the operations.
type Summary struct {
	Header      Header
	Compression string
	// Trailer is read only from uncompressed files (the end of a compressed
	// file can only be reached by decompressing all of it); nil when the
	// file is compressed or has no trailer yet.
	Trailer *Trailer
}

// PeekRecording reads the header line of a recording (at most
// MaxPeekHeaderBytes after decompression) and, for an uncompressed file,
// the trailer in its last bytes. Outcome files and other files are
// reported as ErrNotRecording.
func PeekRecording(path string) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open workload recording: %w", err)
	}
	defer f.Close()
	content, compression, release, err := decompress(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	defer release()
	b, err := bufio.NewReader(io.LimitReader(content, MaxPeekHeaderBytes)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read workload recording: %w", err)
	}
	b = bytes.TrimRight(b, "\r\n")
	switch {
	case len(bytes.TrimSpace(b)) == 0:
		return nil, fmt.Errorf("%w: the file is empty", ErrNotRecording)
	case bytes.HasPrefix(b, outcomeHeaderPrefix):
		return nil, fmt.Errorf("%w: this is an outcome file", ErrNotRecording)
	case !bytes.HasPrefix(b, headerPrefix):
		return nil, fmt.Errorf("%w: the first line is not a recording header", ErrNotRecording)
	}
	s := &Summary{Compression: compression}
	if err := json.Unmarshal(b, &s.Header); err != nil {
		return nil, fmt.Errorf("workload recording header: %w", err)
	}
	if s.Header.Format != FormatVersion {
		return nil, fmt.Errorf("unsupported workload recording format %q (expected %q)", s.Header.Format, FormatVersion)
	}
	if compression == CompressionNone {
		s.Trailer = peekTrailer(f)
	}
	return s, nil
}

// peekTrailer returns the trailer on the last line of an uncompressed
// recording, or nil.
func peekTrailer(f *os.File) *Trailer {
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - maxPeekTrailerBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	buf = bytes.TrimRight(buf, "\r\n")
	line := buf[bytes.LastIndexByte(buf, '\n')+1:]
	if !bytes.HasPrefix(line, trailerPrefix) {
		return nil
	}
	var t Trailer
	if json.Unmarshal(line, &t) != nil {
		return nil
	}
	return &t
}
