package workloadrec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// MaxLineBytes bounds one line of a recording.
const MaxLineBytes = 64 << 20

// FileInfo describes a recording after a full validating scan.
type FileInfo struct {
	Path       string
	Header     Header
	HeaderHash string
	Trailer    *Trailer
	// Status is the trailer status, or StatusIncomplete without a trailer.
	Status    string
	Ops       uint64
	WorkerOps map[int]uint64
	// Workers is the highest worker id in the header or the operations.
	Workers int
	// Compression is detected from the file's magic bytes.
	Compression string
	// Iterations is the highest iteration of the operations (1 for a
	// single-iteration recording); IterationOps counts operations per
	// iteration.
	Iterations   int
	IterationOps map[int]uint64
}

type recordingFile struct {
	f           *os.File
	release     func()
	compression string
}

func (rf *recordingFile) Close() error {
	rf.release()
	return rf.f.Close()
}

var (
	opPrefix      = []byte(`{"type":"op",`)
	headerPrefix  = []byte(`{"type":"header",`)
	trailerPrefix = []byte(`{"type":"trailer",`)
)

type lineReader struct {
	r    *bufio.Reader
	line int
}

func (lr *lineReader) next() ([]byte, error) {
	for {
		var buf []byte
		for {
			b, err := lr.r.ReadSlice('\n')
			buf = append(buf, b...)
			if len(buf) > MaxLineBytes {
				return nil, fmt.Errorf("line %d is longer than %d bytes", lr.line+1, MaxLineBytes)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			// A compressed file whose writer stopped early ends mid-frame.
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if err != nil && len(buf) == 0 {
				return nil, io.EOF
			}
			// The writer ends every line with a newline: a last line without
			// one that is not valid JSON was cut off when the writer stopped.
			if err != nil && !json.Valid(buf) {
				return nil, io.EOF
			}
			break
		}
		lr.line++
		line := bytes.TrimRight(buf, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

func lineType(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, opPrefix):
		return LineOp, nil
	case bytes.HasPrefix(b, headerPrefix):
		return LineHeader, nil
	case bytes.HasPrefix(b, trailerPrefix):
		return LineTrailer, nil
	}
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", err
	}
	return t.Type, nil
}

// open opens a recording, uncompressed or compressed with zstd or gzip.
func open(path string) (*recordingFile, *lineReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open workload recording: %w", err)
	}
	content, compression, release, err := decompress(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	r, ok := content.(*bufio.Reader)
	if !ok {
		r = bufio.NewReaderSize(content, 1<<20)
	}
	return &recordingFile{f: f, release: release, compression: compression}, &lineReader{r: r}, nil
}

func readHeader(lr *lineReader) (Header, string, error) {
	var h Header
	b, err := lr.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return h, "", fmt.Errorf("workload recording is empty")
		}
		return h, "", err
	}
	if t, err := lineType(b); err != nil || t != LineHeader {
		return h, "", fmt.Errorf("workload recording does not start with a header line")
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, "", fmt.Errorf("workload recording header: %w", err)
	}
	if h.Format != FormatVersion {
		return h, "", fmt.Errorf("unsupported workload recording format %q (expected %q)", h.Format, FormatVersion)
	}
	return h, HashBytes(b), nil
}

func parseOp(b []byte, line int) (*OpLine, error) {
	var op OpLine
	if err := json.Unmarshal(b, &op); err != nil {
		return nil, fmt.Errorf("line %d: %w", line, err)
	}
	switch {
	case op.Worker < 1:
		return nil, fmt.Errorf("line %d: worker must be at least 1", line)
	case !knownOps[op.Op]:
		return nil, fmt.Errorf("line %d: unknown operation %q", line, op.Op)
	case op.NS == "":
		return nil, fmt.Errorf("line %d: namespace is missing", line)
	case len(op.Cmd) == 0 || op.Cmd[0] != '{':
		return nil, fmt.Errorf("line %d: command is missing", line)
	case op.Path != "" && op.Path != PathFastInsertBatch && op.Path != PathFastFind:
		return nil, fmt.Errorf("line %d: unknown path %q", line, op.Path)
	}
	return &op, nil
}

// File kinds returned by DetectKind.
const (
	KindRecording = "recording"
	KindOutcomes  = "outcomes"
)

// DetectKind reads the first line of path (compressed or not) and returns
// KindRecording or KindOutcomes.
func DetectKind(path string) (string, error) {
	f, lr, err := open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := lr.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("the file is empty")
		}
		return "", err
	}
	switch {
	case bytes.HasPrefix(b, headerPrefix):
		return KindRecording, nil
	case bytes.HasPrefix(b, outcomeHeaderPrefix):
		return KindOutcomes, nil
	}
	return "", fmt.Errorf("not a workload recording or outcome file (the first line is not a header)")
}

// Scan reads and validates a whole recording: the header, every operation
// (per-worker sequence numbers must run 1, 2, 3, ...), and the trailer.
func Scan(path string) (*FileInfo, error) {
	f, lr, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h, hh, err := readHeader(lr)
	if err != nil {
		return nil, err
	}
	info := &FileInfo{Path: path, Header: h, HeaderHash: hh, WorkerOps: map[int]uint64{}, Workers: h.Workers, Compression: f.compression, IterationOps: map[int]uint64{}}
	for {
		b, err := lr.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if info.Trailer != nil {
			return nil, fmt.Errorf("line %d: data after the trailer", lr.line)
		}
		t, err := lineType(b)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lr.line, err)
		}
		switch t {
		case LineOp:
			op, err := parseOp(b, lr.line)
			if err != nil {
				return nil, err
			}
			if want := info.WorkerOps[op.Worker] + 1; op.WorkerSeq != want {
				return nil, fmt.Errorf("line %d: worker %d sequence %d out of order (expected %d)", lr.line, op.Worker, op.WorkerSeq, want)
			}
			it := op.OpIteration()
			if it < info.Iterations {
				return nil, fmt.Errorf("line %d: iteration %d after iteration %d", lr.line, it, info.Iterations)
			}
			if h.Iterations > 0 && it > h.Iterations {
				return nil, fmt.Errorf("line %d: iteration %d exceeds the recorded %d iterations", lr.line, it, h.Iterations)
			}
			info.Iterations = it
			info.IterationOps[it]++
			info.WorkerOps[op.Worker] = op.WorkerSeq
			info.Ops++
			if op.Worker > info.Workers {
				info.Workers = op.Worker
			}
		case LineTrailer:
			var tr Trailer
			if err := json.Unmarshal(b, &tr); err != nil {
				return nil, fmt.Errorf("line %d: trailer: %w", lr.line, err)
			}
			info.Trailer = &tr
		default:
			return nil, fmt.Errorf("line %d: unexpected %q line", lr.line, t)
		}
	}
	info.Status = StatusIncomplete
	if info.Trailer != nil {
		info.Status = info.Trailer.Status
		if info.Trailer.Ops != info.Ops {
			return nil, fmt.Errorf("trailer counts %d operations but the file has %d", info.Trailer.Ops, info.Ops)
		}
	}
	return info, nil
}

// Stream calls fn for every operation in file order. It stops at the first
// error from fn or when ctx is done.
func Stream(ctx context.Context, path string, fn func(*OpLine) error) error {
	f, lr, err := open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, _, err := readHeader(lr); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := lr.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		t, err := lineType(b)
		if err != nil {
			return fmt.Errorf("line %d: %w", lr.line, err)
		}
		if t != LineOp {
			continue
		}
		op, err := parseOp(b, lr.line)
		if err != nil {
			return err
		}
		if err := fn(op); err != nil {
			return err
		}
	}
}
