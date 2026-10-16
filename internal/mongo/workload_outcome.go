package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Outcome capture (workload_record_outcomes): the execute functions fill an
// Outcome when they are given one. Capturing reads only what the server
// already returned; it never changes the commands sent.

const (
	fastInsertShapeKey  = "insert|fast_batch"
	transactionShapeKey = "transaction|batch"
)

func fastFindShapeKey(batchSize int) string {
	return fmt.Sprintf("find|fast_in_patch_key|limit:%d", batchSize)
}

// newOutcome returns an Outcome to fill, or nil when capture is off.
func (c workloadConfig) newOutcome() *workloadrec.Outcome {
	if !c.outcomes {
		return nil
	}
	return &workloadrec.Outcome{}
}

// writeErrorNames names the server codes that write errors report without
// a name.
var writeErrorNames = map[int]string{
	2:     "BadValue",
	11:    "UserNotFound",
	13:    "Unauthorized",
	26:    "NamespaceNotFound",
	50:    "MaxTimeMSExpired",
	52:    "DollarPrefixedFieldName",
	55:    "InvalidDBRef",
	56:    "EmptyFieldName",
	57:    "DottedFieldName",
	61:    "ShardKeyNotFound",
	66:    "ImmutableField",
	112:   "WriteConflict",
	121:   "DocumentValidationFailure",
	251:   "NoSuchTransaction",
	10334: "BSONObjectTooLarge",
	11000: "DuplicateKey",
	17419: "DocumentTooLarge",
}

func codeName(code int, name string) string {
	if name != "" {
		return name
	}
	return writeErrorNames[code]
}

// errorCodeName is the server error code and name of err, or code 0 and a
// client-side category. The error message is never kept.
func errorCodeName(err error) (int, string) {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return int(ce.Code), codeName(int(ce.Code), ce.Name)
	}
	var we mongo.WriteException
	if errors.As(err, &we) {
		if len(we.WriteErrors) > 0 {
			return we.WriteErrors[0].Code, codeName(we.WriteErrors[0].Code, "")
		}
		if we.WriteConcernError != nil {
			return we.WriteConcernError.Code, codeName(we.WriteConcernError.Code, we.WriteConcernError.Name)
		}
	}
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) {
		if len(bwe.WriteErrors) > 0 {
			return bwe.WriteErrors[0].Code, codeName(bwe.WriteErrors[0].Code, "")
		}
		if bwe.WriteConcernError != nil {
			return bwe.WriteConcernError.Code, codeName(bwe.WriteConcernError.Code, bwe.WriteConcernError.Name)
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return 0, workloadrec.ErrorNameCancelled
	case errors.Is(err, context.DeadlineExceeded), mongo.IsTimeout(err):
		return 0, workloadrec.ErrorNameClientTimeout
	case mongo.IsNetworkError(err):
		return 0, workloadrec.ErrorNameNetwork
	}
	return 0, workloadrec.ErrorNameClient
}

// finishOutcome sets the latency and the error of out.
func finishOutcome(out *workloadrec.Outcome, dur time.Duration, err error) {
	out.LatencyUS = dur.Microseconds()
	out.OK = err == nil
	out.ErrorCode, out.ErrorName = 0, ""
	if err != nil {
		out.ErrorCode, out.ErrorName = errorCodeName(err)
	}
}

// insertedCount is how many of n documents an ordered insertMany stored.
func insertedCount(n int, err error) int64 {
	if err == nil {
		return int64(n)
	}
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) && len(bwe.WriteErrors) > 0 {
		return int64(bwe.WriteErrors[0].Index)
	}
	return 0
}

// resultReader counts the documents of a find or aggregate result and,
// with checksums, keeps a copy of each to hash once the latency window has
// closed, so hashing does not add to the measured latency.
type resultReader struct {
	checksums bool
	n         int64
	buf       []byte
	ends      []int
}

func newResultReader(out *workloadrec.Outcome, checksums bool) *resultReader {
	if out == nil {
		return nil
	}
	return &resultReader{checksums: checksums}
}

func (r *resultReader) add(doc bson.Raw) {
	if r == nil {
		return
	}
	r.n++
	if r.checksums {
		r.buf = append(r.buf, doc...)
		r.ends = append(r.ends, len(r.buf))
	}
}

// finish sets the returned count and checksum of out. A document that
// cannot be hashed makes the checksum "unavailable".
func (r *resultReader) finish(out *workloadrec.Outcome) {
	if r == nil || out == nil {
		return
	}
	out.Returned = workloadrec.Count(r.n)
	if !r.checksums {
		return
	}
	h := workloadrec.NewResultHash()
	start := 0
	for _, end := range r.ends {
		if err := h.Add(bson.Raw(r.buf[start:end])); err != nil {
			out.Checksum = "unavailable"
			return
		}
		start = end
	}
	out.Checksum = h.Sum()
}

// drainCursor reads every document of cur, then closes it. It returns the
// document count and the cursor's error.
func drainCursor(ctx context.Context, cur *mongo.Cursor, rr *resultReader) (int64, error) {
	n := int64(0)
	for cur.Next(ctx) {
		n++
		rr.add(cur.Current)
	}
	err := cur.Err()
	_ = cur.Close(ctx)
	return n, err
}

// fastInsertReply is the part of an insert reply an outcome reports.
type fastInsertReply struct {
	N           int64 `bson:"n"`
	WriteErrors []struct {
		Code int `bson:"code"`
	} `bson:"writeErrors"`
	WriteConcernError *struct {
		Code     int    `bson:"code"`
		CodeName string `bson:"codeName"`
	} `bson:"writeConcernError"`
}

// fastInsertOutcome fills out from a raw insert reply: a reply with write
// errors is not ok even though the command itself succeeded.
func fastInsertOutcome(out *workloadrec.Outcome, dur time.Duration, reply fastInsertReply, err error) {
	finishOutcome(out, dur, err)
	if err != nil {
		out.Inserted = workloadrec.Count(0)
		return
	}
	out.Inserted = workloadrec.Count(reply.N)
	switch {
	case len(reply.WriteErrors) > 0:
		out.OK, out.ErrorCode, out.ErrorName = false, reply.WriteErrors[0].Code, codeName(reply.WriteErrors[0].Code, "")
	case reply.WriteConcernError != nil:
		out.OK, out.ErrorCode, out.ErrorName = false, reply.WriteConcernError.Code, codeName(reply.WriteConcernError.Code, reply.WriteConcernError.CodeName)
	}
}

func transactionOutcome(out *workloadrec.Outcome, dur time.Duration, err error) {
	finishOutcome(out, dur, err)
	out.Transaction = workloadrec.TransactionCommit
	if err != nil {
		out.Transaction = workloadrec.TransactionAbort
	}
}

// outcomeFileInfo is the run metadata of an outcome file.
func outcomeFileInfo(source string, res workloadrec.OutcomeResult, checksums bool) *stats.WorkloadOutcomeFileInfo {
	info := &stats.WorkloadOutcomeFileInfo{Source: source, Path: res.Path, Ops: res.Ops, Bytes: res.Bytes, Status: res.Status, Reason: res.Reason, ResultChecksums: checksums, Waits: res.Waits}
	if res.Waits > 0 {
		ms := float64(res.WaitTotal.Microseconds()) / 1000
		info.WaitTotalMs = &ms
	}
	return info
}

func logOutcomeFile(res workloadrec.OutcomeResult, checksums bool) {
	msg := fmt.Sprintf("[Workload Outcomes] %s: %d outcomes, %d bytes written to %s (result checksums %t)", res.Status, res.Ops, res.Bytes, res.Path, checksums)
	if res.Waits > 0 {
		msg += fmt.Sprintf("; workers waited for the writer %d times, total %.1f ms", res.Waits, float64(res.WaitTotal.Microseconds())/1000)
	}
	if res.Reason != "" {
		msg += " (" + res.Reason + ")"
	}
	log.Print(msg)
}

// openOutcomeFile creates an outcome file for a replay or deterministic run.
func openOutcomeFile(ctx context.Context, db *mongo.Database, path string, cfg *config.AppConfig, h workloadrec.OutcomeHeader) (*workloadrec.OutcomeWriter, error) {
	h.PLGMVersion, h.ServerVersion, h.Topology, h.ResultChecksums = config.BuildVersion, serverVersionFn(ctx, db), serverTopologyFn(ctx, db), cfg.WorkloadRecordResultChecksums
	h.Target = targetFromContext(ctx).label
	w, err := workloadrec.NewOutcomeWriter(path, h)
	if err != nil {
		return nil, err
	}
	what := "latency, error codes, result counts"
	if h.ResultChecksums {
		what += ", result checksums"
	}
	log.Printf("[Workload Outcomes] writing the outcome of every %s operation to %s (mode 0600; %s; no literal values)", h.Source, path, what)
	return w, nil
}

// deterministicOutcomes is the outcome file of a deterministic run.
type deterministicOutcomes struct {
	w         *workloadrec.OutcomeWriter
	checksums bool
}

// close stamps the file with the traffic fingerprint and closes it.
func (d *deterministicOutcomes) close(traffic *trafficFingerprint, collector *stats.Collector) {
	if d == nil || d.w == nil {
		return
	}
	info := traffic.info()
	status, reason := workloadrec.StatusComplete, ""
	if info.Status != trafficStatusComplete {
		status, reason = info.Status, info.Reason
	}
	res := d.w.Close(workloadrec.OutcomeTrailer{CommandsHash: info.CommandsHash, TrafficFingerprint: info.Fingerprint, TrafficStatus: info.Status}, status, reason)
	fi := outcomeFileInfo(workloadrec.OutcomeSourceDeterministic, res, d.checksums)
	fi.TrafficFingerprint = info.Fingerprint
	if collector != nil {
		collector.SetWorkloadOutcomeFile(fi)
	}
	logOutcomeFile(res, d.checksums)
}

// deterministicOutcomeWriter returns the outcome file of a deterministic
// RunWorkload call and the func to call when its workers have stopped.
// Inside a recording session the file spans every iteration and the
// session's finish func closes it.
func deterministicOutcomeWriter(ctx context.Context, db *mongo.Database, cfg *config.AppConfig, det *deterministicRun, collector *stats.Collector) (*workloadrec.OutcomeWriter, func(), error) {
	if !cfg.WorkloadRecordOutcomes || cfg.WorkloadDeterministicOutcomesPath == "" {
		return nil, func() {}, nil
	}
	sess, _ := ctx.Value(recordingSessionKey{}).(*recordingSession)
	if sess != nil && sess.outcomes != nil {
		sess.collector = collector
		return sess.outcomes.w, func() {}, nil
	}
	h := det.traffic.header
	w, err := openOutcomeFile(ctx, db, cfg.WorkloadDeterministicOutcomesPath, cfg, workloadrec.OutcomeHeader{
		Source: workloadrec.OutcomeSourceDeterministic, Workers: h.Workers,
		CollectionsHash: h.CollectionsHash, QueriesHash: h.QueriesHash, RandomSeed: h.RandomSeed, OperationsPerWorker: h.OperationsPerWorker,
	})
	if err != nil {
		return nil, nil, err
	}
	d := &deterministicOutcomes{w: w, checksums: cfg.WorkloadRecordResultChecksums}
	if sess != nil {
		sess.outcomes, sess.collector = d, collector
		return w, func() {}, nil
	}
	return w, func() { d.close(det.traffic, collector) }, nil
}
