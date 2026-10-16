// Package workloadrec reads and writes workload recordings: NDJSON files that
// hold every operation a run sent, with its literal values, so the same
// operations can be replayed on another instance.
//
// A file is a header line, one line per operation, and a trailer line. Each
// operation's command is canonical MongoDB Extended JSON v2, which keeps BSON
// types (ObjectId, Date, int32/int64, Decimal128) and the order of ordered
// documents. Go maps have no key order; they are written with sorted keys.
package workloadrec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"reflect"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// FormatVersion identifies the file layout.
	FormatVersion = "plgm-workload-record/1"

	LineHeader  = "header"
	LineOp      = "op"
	LineTrailer = "trailer"

	StatusRecording = "recording"
	StatusComplete  = "complete"
	StatusTruncated = "truncated"
	StatusFailed    = "failed"
	// StatusIncomplete is reported for a file without a trailer (the writer
	// did not finish, for example because the process stopped).
	StatusIncomplete = "incomplete"

	// PathFastInsertBatch and PathFastFind mark operations of the optimized
	// worker paths, which send raw commands with RunCommand.
	PathFastInsertBatch = "fast_insert_batch"
	PathFastFind        = "fast_find"

	// Backpressure policies: what a worker does when the writer queue is full.
	BackpressureFail  = "fail"
	BackpressureBlock = "block"

	// FailurePolicy (backpressure fail) and BlockPolicy (backpressure block)
	// are stated in every header and in the logs.
	FailurePolicy = "fail_on_backpressure: when the writer queue is full the recording stops and is marked failed; operations are never dropped silently, and the file keeps a prefix of every worker's stream"
	BlockPolicy   = "block_on_backpressure: when the writer queue is full workers wait for the writer, so every operation is recorded and throughput may drop; the trailer and run metadata report the total and maximum wait"

	// Compression of a recording file. Readers detect it from the magic bytes.
	CompressionNone = "none"
	CompressionZstd = "zstd"
	CompressionGzip = "gzip"
)

// Operations the replay understands.
var knownOps = map[string]bool{
	"find": true, "aggregate": true, "updateOne": true, "updateMany": true,
	"deleteOne": true, "deleteMany": true, "insert": true, "insertMany": true,
	"transaction": true,
}

// ConfigSummary is the workload configuration a recording was made with. It
// never holds the connection URI, credentials, or query content.
type ConfigSummary struct {
	Concurrency     int    `json:"concurrency"`
	Duration        string `json:"duration,omitempty"`
	DocumentsCount  int    `json:"documents_count,omitempty"`
	OperationMix    string `json:"operation_mix,omitempty"`
	LoadProfile     string `json:"load_profile,omitempty"`
	AccessPattern   string `json:"access_pattern,omitempty"`
	ThinkTimeMs     int    `json:"think_time_ms,omitempty"`
	ThinkJitterMs   int    `json:"think_jitter_ms,omitempty"`
	FindBatchSize   int    `json:"find_batch_size,omitempty"`
	FindLimit       int64  `json:"find_limit,omitempty"`
	InsertBatchSize int    `json:"insert_batch_size,omitempty"`
	UseTransactions bool   `json:"use_transactions,omitempty"`
}

// Header is the first line of a recording.
type Header struct {
	Type            string        `json:"type"`
	Format          string        `json:"format"`
	PLGMVersion     string        `json:"plgm_version"`
	CreatedAt       string        `json:"created_at"`
	ServerVersion   string        `json:"server_version,omitempty"`
	Topology        string        `json:"topology,omitempty"`
	RandomSeed      int64         `json:"random_seed"`
	Workers         int           `json:"workers"`
	CollectionsHash string        `json:"collections_hash"`
	QueriesHash     string        `json:"queries_hash"`
	Namespaces      []string      `json:"namespaces"`
	Config          ConfigSummary `json:"config"`
	FailurePolicy   string        `json:"failure_policy"`
	// MaxBytes caps the uncompressed bytes of the file; 0 is unlimited.
	MaxBytes int64 `json:"max_bytes"`
	// Backpressure is set for BackpressureBlock only; empty is fail.
	Backpressure string `json:"backpressure,omitempty"`
	// Iterations and IntervalDelay are set when the run had more than one
	// iteration; op lines then carry their iteration.
	Iterations    int    `json:"iterations,omitempty"`
	IntervalDelay string `json:"interval_delay,omitempty"`
	// Outcomes is set when op lines carry their outcome; ResultChecksums
	// when find and aggregate outcomes carry a result checksum.
	Outcomes        bool `json:"outcomes,omitempty"`
	ResultChecksums bool `json:"result_checksums,omitempty"`
	// DataFingerprint is the starting data of the recorded run
	// (workload_data_fingerprint); a replay compares each target with it.
	DataFingerprint *DataFingerprint `json:"data_fingerprint,omitempty"`
	// Import is set when the recording was converted from captured traffic
	// (mongod logs or system.profile) instead of recorded by a run; the
	// collection and query definition hashes are then empty.
	Import *ImportInfo `json:"import,omitempty"`
}

// ImportInfo describes the captured traffic an imported recording came from.
// It holds counts and file names, never literal values.
type ImportInfo struct {
	// Sources lists the entry kinds read: mongod_log, system_profile,
	// command.
	Sources         []string          `json:"sources"`
	Files           []string          `json:"files,omitempty"`
	Entries         uint64            `json:"entries"`
	Converted       uint64            `json:"converted"`
	Skipped         uint64            `json:"skipped"`
	SkippedByReason map[string]uint64 `json:"skipped_by_reason,omitempty"`
	Flagged         map[string]uint64 `json:"flagged,omitempty"`
	Connections     int               `json:"connections"`
	FirstTimestamp  string            `json:"first_timestamp,omitempty"`
	LastTimestamp   string            `json:"last_timestamp,omitempty"`
	Note            string            `json:"note"`
}

// Flags an imported operation can carry: it is replayed, but not exactly as
// it ran when captured.
const (
	FlagServerGenerated = "server_generated_values"
	FlagDroppedOptions  = "dropped_options"
	FlagInTransaction   = "in_transaction"
	FlagSplitStatement  = "split_statement"
	FlagFailedOnSource  = "failed_on_source"
	FlagNoTimestamp     = "no_timestamp"
)

// QueryRef names the query definition an operation came from.
type QueryRef struct {
	Name     string `json:"name,omitempty"`
	Index    int    `json:"index"`
	Label    string `json:"label,omitempty"`
	Source   string `json:"source,omitempty"`
	Workload string `json:"workload,omitempty"`
	File     string `json:"file,omitempty"`
	ID       string `json:"id,omitempty"`
}

// ShapeRef is the redacted shape identity the run reported for an operation,
// so a replay reports the same shapes.
type ShapeRef struct {
	Key             string   `json:"key"`
	Summary         string   `json:"summary,omitempty"`
	Fields          []string `json:"fields,omitempty"`
	QuerySummary    string   `json:"query_summary,omitempty"`
	PipelineSummary string   `json:"pipeline_summary,omitempty"`
}

// OpLine is one recorded operation. Cmd is canonical Extended JSON v2.
type OpLine struct {
	Type      string `json:"type"`
	Seq       uint64 `json:"seq"`
	Worker    int    `json:"worker"`
	WorkerSeq uint64 `json:"worker_seq"`
	OffsetUS  int64  `json:"offset_us"`
	// Iteration is 1-based in multi-iteration recordings and 0 otherwise.
	Iteration int             `json:"iteration,omitempty"`
	NS        string          `json:"ns"`
	Op        string          `json:"op"`
	Path      string          `json:"path,omitempty"`
	OK        bool            `json:"ok"`
	Existing  bool            `json:"existing,omitempty"`
	Query     *QueryRef       `json:"query,omitempty"`
	Shape     *ShapeRef       `json:"shape,omitempty"`
	Cmd       json.RawMessage `json:"cmd"`
	// Outcome is set when the recording captured outcomes.
	Outcome *Outcome `json:"outcome,omitempty"`
	// Flags are set on imported operations only (Flag* constants).
	Flags []string `json:"flags,omitempty"`
}

// Trailer is the last line of a finished recording. CommandsHash is the
// sha256 of the per-worker command hashes in worker order; a replay that
// sends the same commands per worker reports the same hash.
type Trailer struct {
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	Reason       string            `json:"reason,omitempty"`
	Ops          uint64            `json:"ops"`
	Bytes        int64             `json:"bytes"`
	WorkerOps    map[string]uint64 `json:"worker_ops,omitempty"`
	WorkerHashes map[string]string `json:"worker_hashes,omitempty"`
	CommandsHash string            `json:"commands_hash,omitempty"`
	// IterationOps counts operations per iteration in multi-iteration
	// recordings.
	IterationOps map[string]uint64 `json:"iteration_ops,omitempty"`
	// Waits, WaitTotalUS, and WaitMaxUS report how often and how long
	// workers waited for the writer (backpressure block only).
	Waits       uint64 `json:"waits,omitempty"`
	WaitTotalUS int64  `json:"wait_total_us,omitempty"`
	WaitMaxUS   int64  `json:"wait_max_us,omitempty"`
}

// OpIteration is the 1-based iteration of an operation.
func (op *OpLine) OpIteration() int {
	if op.Iteration < 1 {
		return 1
	}
	return op.Iteration
}

// Canonical returns v with every map turned into a document with sorted keys
// and every slice into an array, so encoding is deterministic. Ordered
// documents (bson.D) keep their order; a multi-key $sort is never sorted.
func Canonical(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]interface{}:
		return sortedDoc(len(t), func(yield func(string, interface{})) {
			for k, x := range t {
				yield(k, x)
			}
		})
	case bson.M:
		return Canonical(map[string]interface{}(t))
	case bson.D:
		out := make(bson.D, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: Canonical(e.Value)}
		}
		return out
	case []interface{}:
		out := make(bson.A, len(t))
		for i, x := range t {
			out[i] = Canonical(x)
		}
		return out
	case bson.A:
		return Canonical([]interface{}(t))
	case bson.Raw, []byte, string, bool, int, int32, int64, float64, bson.ObjectID, bson.DateTime, bson.Decimal128:
		return v
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		return sortedDoc(rv.Len(), func(yield func(string, interface{})) {
			it := rv.MapRange()
			for it.Next() {
				yield(it.Key().String(), it.Value().Interface())
			}
		})
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := make(bson.A, rv.Len())
		for i := range out {
			out[i] = Canonical(rv.Index(i).Interface())
		}
		return out
	}
	return v
}

func sortedDoc(n int, each func(yield func(string, interface{}))) bson.D {
	out := make(bson.D, 0, n)
	each(func(k string, x interface{}) { out = append(out, bson.E{Key: k, Value: x}) })
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	for i := range out {
		out[i].Value = Canonical(out[i].Value)
	}
	return out
}

// EncodeCommand encodes a command document as canonical Extended JSON v2.
// cmd is a bson.D, a map, or a bson.Raw document.
func EncodeCommand(cmd interface{}) ([]byte, error) {
	if raw, ok := cmd.(bson.Raw); ok {
		return bson.MarshalExtJSON(raw, true, false)
	}
	return bson.MarshalExtJSON(Canonical(cmd), true, false)
}

// DecodeCommand decodes a recorded command. Documents decode as bson.D and
// arrays as bson.A, so order and BSON types are kept.
func DecodeCommand(raw []byte) (bson.D, error) {
	var d bson.D
	if err := bson.UnmarshalExtJSON(raw, true, &d); err != nil {
		return nil, fmt.Errorf("decode recorded command: %w", err)
	}
	return d, nil
}

// HashBytes is the hex sha256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DefinitionHash hashes a definition list as JSON. encoding/json writes
// struct fields in declaration order and map keys sorted, so equal
// definitions hash equal.
func DefinitionHash(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// StreamHash hashes one worker's commands in order.
type StreamHash struct {
	h hash.Hash
}

func NewStreamHash() *StreamHash { return &StreamHash{h: sha256.New()} }

// Add adds one command (its recorded Extended JSON bytes).
func (s *StreamHash) Add(cmd []byte) {
	_, _ = s.h.Write(cmd)
	_, _ = s.h.Write([]byte{'\n'})
}

func (s *StreamHash) Sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

// CombineHashes is the sha256 of the per-worker hashes in worker order.
func CombineHashes(workerHashes map[int]string) string {
	ids := make([]int, 0, len(workerHashes))
	for id := range workerHashes {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	h := sha256.New()
	for _, id := range ids {
		fmt.Fprintf(h, "%d:%s\n", id, workerHashes[id])
	}
	return hex.EncodeToString(h.Sum(nil))
}
