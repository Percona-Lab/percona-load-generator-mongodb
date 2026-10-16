// Package trafficimport converts captured MongoDB traffic (mongod JSON
// slow-query logs, system.profile exports, or raw command documents) into a
// workload recording, so real application traffic can be replayed and
// compared like a PLGM recording (pt-upgrade's log replay).
//
// Entries are read with schemainfer.ForEachEntry and located the same way
// schemainfer does (attr.command for logs, ns/op/command for the profiler,
// the document itself for raw commands), but parsed as relaxed Extended JSON
// into ordered documents, so field order and BSON types survive.
package trafficimport

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/schemainfer"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Entry kinds.
const (
	SourceMongodLog = "mongod_log"
	SourceProfile   = "system_profile"
	SourceCommand   = "command"
)

// Reasons an entry is not converted.
const (
	SkipUnparseable     = "unparseable"
	SkipTruncated       = "truncated"
	SkipGetMore         = "getmore"
	SkipUnsupported     = "unsupported_command"
	SkipDuplicateWrite  = "duplicate_write_statement"
	SkipSystemNamespace = "system_namespace"
	SkipNoNamespace     = "no_namespace"
	SkipMalformed       = "malformed_command"
	SkipServerGenerated = "server_generated_values"
)

// MaxWorkers caps the workers of an import that maps one connection to one
// worker; more connections share workers.
const MaxWorkers = 1024

const maxWarnings = 20

// Note is stated in every import header and report.
const Note = "mongod logs and system.profile hold only operations slower than slowms (default 100 ms), and only the share sampled by slowOpSampleRate, unless profiling level 2 or slowms 0 was set while capturing; the import replays only what was captured. Offsets are start times estimated as the logged time minus the logged duration."

// NormalizeFunc turns an operation's arguments into the recorded command and
// its shape (mongo.ImportedOperation).
type NormalizeFunc func(ns, op string, args bson.D) (bson.D, *workloadrec.ShapeRef, error)

// Input is one captured-traffic file.
type Input struct {
	Name string
	R    io.Reader
}

// Options configure Convert.
type Options struct {
	// Workers > 0 maps connections onto that many workers; 0 gives every
	// connection its own worker (up to MaxWorkers).
	Workers int
	// DefaultDatabase is the database of raw commands without $db.
	DefaultDatabase string
	// Compression of the output: none (default), zstd, or gzip.
	Compression string
	// SkipServerGenerated skips operations whose values the server
	// generates instead of only flagging them.
	SkipServerGenerated bool
	Normalize           NormalizeFunc
}

// Report summarizes an import. It holds counts, names, and line numbers,
// never literal values.
type Report struct {
	Path                string            `json:"path,omitempty"`
	Status              string            `json:"status"`
	HeaderHash          string            `json:"header_hash,omitempty"`
	Sources             []string          `json:"sources"`
	Files               []string          `json:"files"`
	Entries             uint64            `json:"entries"`
	NonOperations       uint64            `json:"non_operations"`
	Converted           uint64            `json:"converted"`
	Skipped             uint64            `json:"skipped"`
	SkippedByReason     map[string]uint64 `json:"skipped_by_reason"`
	UnsupportedCommands map[string]uint64 `json:"unsupported_commands,omitempty"`
	Flagged             map[string]uint64 `json:"flagged"`
	DroppedOptions      map[string]uint64 `json:"dropped_options,omitempty"`
	OpsByType           map[string]uint64 `json:"ops_by_type"`
	Namespaces          []string          `json:"namespaces"`
	Connections         int               `json:"connections"`
	Workers             int               `json:"workers"`
	FoldedConnections   bool              `json:"folded_connections,omitempty"`
	FirstTimestamp      string            `json:"first_timestamp,omitempty"`
	LastTimestamp       string            `json:"last_timestamp,omitempty"`
	SpanMs              int64             `json:"span_ms"`
	Bytes               int64             `json:"bytes"`
	FileBytes           int64             `json:"file_bytes"`
	Compression         string            `json:"compression"`
	Note                string            `json:"note"`
	Warnings            []string          `json:"warnings"`
}

// ErrNoOperations is returned when nothing could be converted; no file is
// written.
var ErrNoOperations = errors.New("no operations could be converted")

// pending is one converted operation waiting to be ordered and written.
type pending struct {
	start  int64 // unix microseconds
	hasTS  bool
	conn   string
	ns, op string
	ok     bool
	flags  []string
	cmd    bson.Raw
	shape  *workloadrec.ShapeRef
}

type converter struct {
	opts    Options
	rep     *Report
	ops     []pending
	sources map[string]bool
	lastTS  int64
}

// Convert reads every input, converts the supported operations, orders them
// by estimated start time, maps connections to workers, and writes a
// recording to outPath (mode 0600; an existing file that is not a recording
// is never overwritten).
func Convert(inputs []Input, outPath string, opts Options) (*Report, error) {
	if opts.Normalize == nil {
		return nil, errors.New("trafficimport: Normalize is required")
	}
	if opts.Workers < 0 {
		return nil, errors.New("workers must be 0 (one per connection) or a positive number")
	}
	if opts.Compression == "" {
		opts.Compression = workloadrec.CompressionNone
	}
	if !workloadrec.ValidCompression(opts.Compression) {
		return nil, fmt.Errorf("unknown compression %q (none, zstd, or gzip)", opts.Compression)
	}
	c := &converter{opts: opts, sources: map[string]bool{}, rep: &Report{
		SkippedByReason: map[string]uint64{}, UnsupportedCommands: map[string]uint64{}, Flagged: map[string]uint64{},
		DroppedOptions: map[string]uint64{}, OpsByType: map[string]uint64{}, Files: []string{}, Namespaces: []string{},
		Sources: []string{}, Warnings: []string{}, Compression: opts.Compression, Note: Note,
	}}
	for i, in := range inputs {
		name := filepath.Base(in.Name)
		c.rep.Files = append(c.rep.Files, name)
		c.lastTS = 0
		err := schemainfer.ForEachEntry(in.R, func(b []byte, line int) error {
			c.rep.Entries++
			c.entry(i, name, line, b)
			return nil
		})
		if err != nil {
			return c.rep, fmt.Errorf("%s: %w", name, err)
		}
	}
	for s := range c.sources {
		c.rep.Sources = append(c.rep.Sources, s)
	}
	sort.Strings(c.rep.Sources)
	if len(c.ops) == 0 {
		c.rep.Status = "empty"
		return c.rep, ErrNoOperations
	}
	return c.rep, c.write(outPath)
}

func (c *converter) warn(format string, args ...interface{}) {
	if len(c.rep.Warnings) < maxWarnings {
		c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf(format, args...))
	} else if len(c.rep.Warnings) == maxWarnings {
		c.rep.Warnings = append(c.rep.Warnings, "more warnings not shown")
	}
}

func (c *converter) skip(reason string) {
	c.rep.Skipped++
	c.rep.SkippedByReason[reason]++
}

// captured is what an entry says about the operation besides its command.
type captured struct {
	source string
	ns     string
	conn   string
	ts     int64
	hasTS  bool
	failed bool
}

func (c *converter) entry(file int, name string, line int, b []byte) {
	var doc bson.D
	if err := bson.UnmarshalExtJSON(b, false, &doc); err != nil {
		// The parse error quotes the input, which may hold literal values.
		c.skip(SkipUnparseable)
		c.warn("%s line %d: not a JSON document", name, line)
		return
	}
	src := captured{}
	var cmd bson.D
	switch attr, isLog := get(doc, "attr").(bson.D); {
	case isLog:
		src.source = SourceMongodLog
		cmd, _ = get(attr, "command").(bson.D)
		if cmd == nil {
			c.rep.NonOperations++
			return
		}
		c.sources[src.source] = true
		if hasKey(doc, "truncated") || hasKey(attr, "truncated") || containsKey(cmd, "$truncated") {
			c.skip(SkipTruncated)
			return
		}
		switch t, _ := get(attr, "type").(string); t {
		case "update", "remove", "delete":
			// The command entry of the same write covers the statement.
			c.skip(SkipDuplicateWrite)
			return
		}
		src.ns, _ = get(attr, "ns").(string)
		ctx, _ := get(doc, "ctx").(string)
		src.conn = fmt.Sprintf("%d|%s", file, ctx)
		src.ts, src.hasTS = startTime(get(doc, "t"), get(attr, "durationMillis"))
		src.failed = hasKey(attr, "errCode") || isZero(get(attr, "ok"))
	case hasKey(doc, "op") && hasKey(doc, "ns"):
		src.source = SourceProfile
		c.sources[src.source] = true
		src.ns, _ = get(doc, "ns").(string)
		cmd, _ = get(doc, "command").(bson.D)
		if cmd == nil {
			cmd, _ = get(doc, "query").(bson.D)
		}
		if cmd == nil {
			c.skip(SkipMalformed)
			return
		}
		if containsKey(cmd, "$truncated") {
			c.skip(SkipTruncated)
			return
		}
		src.conn = fmt.Sprintf("%d|%s", file, profileConn(doc, cmd))
		src.ts, src.hasTS = startTime(get(doc, "ts"), get(doc, "millis"))
		src.failed = hasKey(doc, "errCode") || isZero(get(doc, "ok"))
		op, _ := get(doc, "op").(string)
		switch strings.ToLower(op) {
		case "getmore":
			c.skip(SkipGetMore)
			return
		case "update":
			if !hasKey(cmd, "update") {
				c.profileStatement(src, "update", cmd)
				return
			}
		case "remove", "delete":
			if !hasKey(cmd, "delete") {
				c.profileStatement(src, "delete", cmd)
				return
			}
		}
	default:
		if len(doc) == 0 || !knownVerb(doc[0].Key) {
			c.rep.NonOperations++
			return
		}
		src.source = SourceCommand
		c.sources[src.source] = true
		cmd = doc
		if containsKey(cmd, "$truncated") {
			c.skip(SkipTruncated)
			return
		}
		src.conn = fmt.Sprintf("%d|%s", file, sessionID(cmd))
	}
	c.command(src, cmd)
}

// startTime is the start of an operation in unix microseconds: its logged
// time (when it finished) minus its duration.
func startTime(t interface{}, durMs interface{}) (int64, bool) {
	dt, ok := t.(bson.DateTime)
	if !ok {
		return 0, false
	}
	us := dt.Time().UnixMicro()
	if d, ok := number(durMs); ok && d > 0 {
		us -= d * 1000
	}
	return us, true
}

func profileConn(doc, cmd bson.D) string {
	if id := sessionID(doc); id != "" {
		return id
	}
	if id := sessionID(cmd); id != "" {
		return id
	}
	client, _ := get(doc, "client").(string)
	app, _ := get(doc, "appName").(string)
	user, _ := get(doc, "user").(string)
	return "client:" + client + "|" + app + "|" + user
}

func sessionID(d bson.D) string {
	lsid, _ := get(d, "lsid").(bson.D)
	if bin, ok := get(lsid, "id").(bson.Binary); ok {
		return "lsid:" + hex.EncodeToString(bin.Data)
	}
	return ""
}

// knownVerb lists command names an entry can start with; the supported ones
// are converted and the others reported as unsupported.
func knownVerb(k string) bool {
	switch k {
	case "find", "aggregate", "insert", "update", "delete", "getMore", "count", "distinct", "findAndModify", "findandmodify", "mapReduce", "explain", "bulkWrite":
		return true
	}
	return false
}

// envelopeKeys are command fields that carry the session, routing, or
// write/read concern, not what the operation does; they are dropped quietly.
var envelopeKeys = map[string]bool{
	"$db": true, "lsid": true, "$clusterTime": true, "txnNumber": true, "autocommit": true, "startTransaction": true,
	"readConcern": true, "writeConcern": true, "$readPreference": true, "comment": true, "maxTimeMS": true,
	"apiVersion": true, "apiStrict": true, "apiDeprecationErrors": true, "$audit": true, "$client": true,
	"mayBypassWriteBlocking": true, "ordered": true, "bypassDocumentValidation": true, "cursor": true,
	"singleBatch": true, "noCursorTimeout": true, "allowPartialResults": true, "$configTime": true,
	"$topologyTime": true, "shardVersion": true, "databaseVersion": true, "clientOperationKey": true,
	"$configServerState": true, "$replData": true, "$gleStats": true, "$oplogQueryData": true,
	"fromMongos": true, "needsMerge": true, "$queryOptions": true, "$timeseries": true,
}

func (c *converter) command(src captured, cmd bson.D) {
	if len(cmd) == 0 {
		c.skip(SkipMalformed)
		return
	}
	verb := cmd[0].Key
	coll, collIsName := cmd[0].Value.(string)
	switch verb {
	case "getMore":
		c.skip(SkipGetMore)
		return
	case "find", "aggregate", "insert", "update", "delete":
	default:
		c.skip(SkipUnsupported)
		c.rep.UnsupportedCommands[verb]++
		return
	}
	if verb == "aggregate" && !collIsName {
		c.skip(SkipUnsupported)
		c.rep.UnsupportedCommands["aggregate (database level)"]++
		return
	}
	if !collIsName || coll == "" {
		c.skip(SkipMalformed)
		return
	}
	db, _ := get(cmd, "$db").(string)
	if db == "" {
		db, _, _ = strings.Cut(src.ns, ".")
	}
	if db == "" {
		db = c.opts.DefaultDatabase
	}
	if db == "" {
		c.skip(SkipNoNamespace)
		return
	}
	src.ns = db + "." + coll
	inTx := false
	if ac, ok := get(cmd, "autocommit").(bool); ok && !ac {
		inTx = true
	}
	switch verb {
	case "find":
		args := bson.D{{Key: "filter", Value: docOr(get(cmd, "filter"))}}
		for _, k := range []string{"projection", "sort", "hint"} {
			if v := get(cmd, k); v != nil {
				args = append(args, bson.E{Key: k, Value: v})
			}
		}
		if n, ok := number(get(cmd, "skip")); ok && n > 0 {
			args = append(args, bson.E{Key: "skip", Value: n})
		}
		limit, _ := number(get(cmd, "limit"))
		if limit < 0 {
			limit = -limit
		}
		batch, _ := number(get(cmd, "batchSize"))
		args = append(args, bson.E{Key: "limit", Value: limit}, bson.E{Key: "batchSize", Value: batch})
		c.add(src, "find", args, c.extraOptions(cmd, "find", "filter", "projection", "sort", "hint", "skip", "limit", "batchSize"), inTx, false)
	case "aggregate":
		if get(cmd, "explain") == true {
			c.skip(SkipUnsupported)
			c.rep.UnsupportedCommands["aggregate (explain)"]++
			return
		}
		pipe, ok := get(cmd, "pipeline").(bson.A)
		if !ok {
			c.skip(SkipMalformed)
			return
		}
		if len(pipe) > 0 {
			if st, ok := pipe[0].(bson.D); ok && len(st) > 0 && st[0].Key == "$changeStream" {
				c.skip(SkipUnsupported)
				c.rep.UnsupportedCommands["aggregate ($changeStream)"]++
				return
			}
		}
		args := bson.D{{Key: "pipeline", Value: pipe}}
		if v := get(cmd, "hint"); v != nil {
			args = append(args, bson.E{Key: "hint", Value: v})
		}
		c.add(src, "aggregate", args, c.extraOptions(cmd, "aggregate", "pipeline", "hint", "explain"), inTx, false)
	case "insert":
		docs, ok := get(cmd, "documents").(bson.A)
		if !ok || len(docs) == 0 {
			c.skip(SkipMalformed)
			return
		}
		gen := false
		for _, d := range docs {
			if dd, ok := d.(bson.D); !ok || !hasKey(dd, "_id") {
				gen = true
			}
		}
		dropped := c.extraOptions(cmd, "insert", "documents")
		if len(docs) == 1 {
			c.add(src, "insert", bson.D{{Key: "document", Value: docs[0]}}, dropped, inTx, gen)
		} else {
			c.add(src, "insertMany", bson.D{{Key: "documents", Value: docs}}, dropped, inTx, gen)
		}
	case "update":
		st, ok := get(cmd, "updates").(bson.A)
		if !ok || len(st) == 0 {
			c.skip(SkipMalformed)
			return
		}
		c.statements(src, "update", st, c.extraOptions(cmd, "update", "updates"), inTx)
	case "delete":
		st, ok := get(cmd, "deletes").(bson.A)
		if !ok || len(st) == 0 {
			c.skip(SkipMalformed)
			return
		}
		c.statements(src, "delete", st, c.extraOptions(cmd, "delete", "deletes"), inTx)
	}
}

// profileStatement converts a profiler entry of one update or delete
// statement: its command is {q, u, multi, upsert} or {q, limit}.
func (c *converter) profileStatement(src captured, verb string, st bson.D) {
	if _, coll, ok := strings.Cut(src.ns, "."); !ok || coll == "" {
		c.skip(SkipNoNamespace)
		return
	}
	c.statements(src, verb, bson.A{st}, nil, false)
}

// statements converts the statements of an update or delete, one operation
// each; dropped are the command-level options the replay does not send.
func (c *converter) statements(src captured, verb string, st bson.A, dropped []string, inTx bool) {
	split := len(st) > 1
	for _, x := range st {
		s, ok := x.(bson.D)
		if !ok {
			c.skip(SkipMalformed)
			continue
		}
		filter := docOr(get(s, "q"))
		var op string
		var args bson.D
		gen := false
		var stDropped []string
		if verb == "update" {
			u := get(s, "u")
			d, isDoc := u.(bson.D)
			if !isArray(u) && (!isDoc || len(d) == 0 || !strings.HasPrefix(d[0].Key, "$")) {
				c.skip(SkipUnsupported)
				c.rep.UnsupportedCommands["update (replacement document)"]++
				continue
			}
			multi, _ := get(s, "multi").(bool)
			upsert, _ := get(s, "upsert").(bool)
			op = "updateOne"
			if multi {
				op = "updateMany"
			}
			args = bson.D{{Key: "filter", Value: filter}, {Key: "update", Value: u}, {Key: "upsert", Value: upsert}}
			fd, _ := filter.(bson.D)
			gen = upsert && !hasKey(fd, "_id")
			stDropped = c.extraOptions(s, "", "q", "u", "multi", "upsert")
		} else {
			limit, _ := number(get(s, "limit"))
			op = "deleteMany"
			if limit == 1 {
				op = "deleteOne"
			}
			args = bson.D{{Key: "filter", Value: filter}}
			stDropped = c.extraOptions(s, "", "q", "limit")
		}
		c.addOp(src, op, args, append(append([]string(nil), dropped...), stDropped...), inTx, gen, split)
	}
}

// extraOptions counts the fields of cmd that are neither in keep, the
// command name, nor the envelope; the replay does not send them.
func (c *converter) extraOptions(cmd bson.D, verb string, keep ...string) []string {
	var out []string
	for i, e := range cmd {
		if (i == 0 && e.Key == verb) || envelopeKeys[e.Key] || contains(keep, e.Key) {
			continue
		}
		out = append(out, e.Key)
	}
	return out
}

func (c *converter) add(src captured, op string, args bson.D, dropped []string, inTx, gen bool) {
	c.addOp(src, op, args, dropped, inTx, gen, false)
}

func (c *converter) addOp(src captured, op string, args bson.D, dropped []string, inTx, gen, split bool) {
	if isSystemNamespace(src.ns) {
		c.skip(SkipSystemNamespace)
		return
	}
	gen = gen || usesServerValues(args)
	if gen && c.opts.SkipServerGenerated {
		c.skip(SkipServerGenerated)
		return
	}
	cmd, shape, err := c.opts.Normalize(src.ns, op, args)
	if err != nil {
		c.skip(SkipMalformed)
		c.warn("an operation on %s could not be converted (%s)", src.ns, err.Error())
		return
	}
	raw, err := bson.Marshal(cmd)
	if err != nil {
		c.skip(SkipMalformed)
		return
	}
	var flags []string
	addFlag := func(cond bool, f string) {
		if cond {
			flags = append(flags, f)
			c.rep.Flagged[f]++
		}
	}
	addFlag(gen, workloadrec.FlagServerGenerated)
	addFlag(len(dropped) > 0, workloadrec.FlagDroppedOptions)
	addFlag(inTx, workloadrec.FlagInTransaction)
	addFlag(split, workloadrec.FlagSplitStatement)
	addFlag(src.failed, workloadrec.FlagFailedOnSource)
	for _, k := range dropped {
		c.rep.DroppedOptions[k]++
	}
	ts, hasTS := src.ts, src.hasTS
	if hasTS {
		c.lastTS = ts
	} else {
		ts = c.lastTS
		addFlag(true, workloadrec.FlagNoTimestamp)
	}
	sort.Strings(flags)
	c.ops = append(c.ops, pending{start: ts, hasTS: hasTS, conn: src.conn, ns: src.ns, op: op, ok: !src.failed, flags: flags, cmd: raw, shape: shape})
}

func (c *converter) write(outPath string) error {
	sort.SliceStable(c.ops, func(i, j int) bool { return c.ops[i].start < c.ops[j].start })
	conns := map[string]int{}
	nsSeen := map[string]bool{}
	var minTS, maxTS int64
	anyTS := false
	for _, p := range c.ops {
		if _, ok := conns[p.conn]; !ok {
			conns[p.conn] = len(conns)
		}
		if !nsSeen[p.ns] {
			nsSeen[p.ns] = true
			c.rep.Namespaces = append(c.rep.Namespaces, p.ns)
		}
		if p.hasTS {
			if !anyTS || p.start < minTS {
				minTS = p.start
			}
			if !anyTS || p.start > maxTS {
				maxTS = p.start
			}
			anyTS = true
		}
	}
	sort.Strings(c.rep.Namespaces)
	c.rep.Connections = len(conns)
	workers := c.opts.Workers
	if workers == 0 {
		workers = min(len(conns), MaxWorkers)
		if len(conns) > MaxWorkers {
			c.rep.FoldedConnections = true
			c.warn("%d connections share %d workers (the maximum); set workers to choose how many", len(conns), MaxWorkers)
		}
	} else if len(conns) > workers {
		c.rep.FoldedConnections = true
	}
	c.rep.Workers = workers
	if anyTS {
		c.rep.FirstTimestamp = time.UnixMicro(minTS).UTC().Format(time.RFC3339Nano)
		c.rep.LastTimestamp = time.UnixMicro(maxTS).UTC().Format(time.RFC3339Nano)
		c.rep.SpanMs = (maxTS - minTS) / 1000
	}
	info := &workloadrec.ImportInfo{
		Sources: c.rep.Sources, Files: c.rep.Files, Entries: c.rep.Entries, Converted: uint64(len(c.ops)), Skipped: c.rep.Skipped,
		SkippedByReason: c.rep.SkippedByReason, Flagged: c.rep.Flagged, Connections: len(conns),
		FirstTimestamp: c.rep.FirstTimestamp, LastTimestamp: c.rep.LastTimestamp, Note: Note,
	}
	h := workloadrec.Header{
		PLGMVersion: config.BuildVersion, Workers: workers, Namespaces: c.rep.Namespaces,
		Config: workloadrec.ConfigSummary{Concurrency: workers}, Import: info,
	}
	rec, err := workloadrec.NewRecorder(outPath, workloadrec.Options{Backpressure: workloadrec.BackpressureBlock, Compression: c.opts.Compression}, h)
	if err != nil {
		return err
	}
	for _, p := range c.ops {
		offset := time.Duration(0)
		if anyTS && p.start > minTS {
			offset = time.Duration(p.start-minTS) * time.Microsecond
		}
		rec.Record(workloadrec.Entry{
			Worker: conns[p.conn]%workers + 1, Offset: offset, NS: p.ns, Op: p.op, OK: p.ok, Shape: p.shape, Command: p.cmd, Flags: p.flags,
		})
		c.rep.OpsByType[p.op]++
	}
	res := rec.Close()
	c.rep.Path, c.rep.Status, c.rep.Converted, c.rep.Bytes, c.rep.FileBytes = outPath, res.Status, res.Ops, res.Bytes, res.FileBytes
	if res.Status != workloadrec.StatusComplete {
		return fmt.Errorf("write %s: recording %s (%s)", outPath, res.Status, res.Reason)
	}
	fi, err := workloadrec.Scan(outPath)
	if err != nil {
		return fmt.Errorf("verify %s: %w", outPath, err)
	}
	c.rep.HeaderHash = fi.HeaderHash
	return nil
}

// SummaryText is the CLI summary of an import.
func SummaryText(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Imported %d of %d entries from %s (%s)\n", r.Converted, r.Entries, strings.Join(r.Files, ", "), strings.Join(r.Sources, ", "))
	fmt.Fprintf(&b, "  operations: %s\n", countsText(r.OpsByType))
	fmt.Fprintf(&b, "  connections %d -> workers %d", r.Connections, r.Workers)
	if r.FoldedConnections {
		b.WriteString(" (connections share workers)")
	}
	b.WriteString("\n")
	if r.FirstTimestamp != "" {
		fmt.Fprintf(&b, "  captured %s .. %s (%d ms)\n", r.FirstTimestamp, r.LastTimestamp, r.SpanMs)
	}
	fmt.Fprintf(&b, "  not operations: %d; skipped: %d %s\n", r.NonOperations, r.Skipped, countsText(r.SkippedByReason))
	if len(r.UnsupportedCommands) > 0 {
		fmt.Fprintf(&b, "  unsupported commands: %s\n", countsText(r.UnsupportedCommands))
	}
	if len(r.Flagged) > 0 {
		fmt.Fprintf(&b, "  flagged (replayed, not exactly as captured): %s\n", countsText(r.Flagged))
	}
	if len(r.DroppedOptions) > 0 {
		fmt.Fprintf(&b, "  options not replayed: %s\n", countsText(r.DroppedOptions))
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	fmt.Fprintf(&b, "  note: %s\n", r.Note)
	return b.String()
}

func countsText(m map[string]uint64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// --- helpers ---

func get(d bson.D, key string) interface{} {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

func hasKey(d bson.D, key string) bool {
	for _, e := range d {
		if e.Key == key {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func docOr(v interface{}) interface{} {
	if v == nil {
		return bson.D{}
	}
	return v
}

func isArray(v interface{}) bool {
	_, ok := v.(bson.A)
	return ok
}

func number(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func isZero(v interface{}) bool {
	n, ok := number(v)
	return ok && n == 0
}

func isSystemNamespace(ns string) bool {
	db, coll, _ := strings.Cut(ns, ".")
	switch db {
	case "admin", "local", "config":
		return true
	}
	return strings.HasPrefix(coll, "system.")
}

// containsKey reports whether key appears at any depth of v.
func containsKey(v interface{}, key string) bool {
	switch t := v.(type) {
	case bson.D:
		for _, e := range t {
			if e.Key == key || containsKey(e.Value, key) {
				return true
			}
		}
	case bson.A:
		for _, x := range t {
			if containsKey(x, key) {
				return true
			}
		}
	}
	return false
}

// usesServerValues reports operators and variables whose value the server
// picks at run time, so a replay sends the same command but gets other values.
func usesServerValues(v interface{}) bool {
	switch t := v.(type) {
	case bson.D:
		for _, e := range t {
			switch e.Key {
			case "$currentDate", "$rand", "$sample", "$sampleRate":
				return true
			}
			if usesServerValues(e.Value) {
				return true
			}
		}
	case bson.A:
		for _, x := range t {
			if usesServerValues(x) {
				return true
			}
		}
	case string:
		return t == "$$NOW" || t == "$$CLUSTER_TIME"
	}
	return false
}
