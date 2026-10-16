package mongo

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var serverVersionFn = serverVersion

func serverVersion(ctx context.Context, db *mongo.Database) string {
	if db == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var res struct {
		Version string `bson:"version"`
	}
	if err := db.RunCommand(cctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&res); err != nil {
		return ""
	}
	return res.Version
}

var serverTopologyFn = serverTopology

// serverTopology is "sharded" (mongos), "replica_set", or "standalone", or
// empty when hello fails.
func serverTopology(ctx context.Context, db *mongo.Database) string {
	if db == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var res struct {
		Msg     string `bson:"msg"`
		SetName string `bson:"setName"`
	}
	if err := db.RunCommand(cctx, bson.D{{Key: "hello", Value: 1}}).Decode(&res); err != nil {
		return ""
	}
	switch {
	case res.Msg == "isdbgrid":
		return "sharded"
	case res.SetName != "":
		return "replica_set"
	}
	return "standalone"
}

// workloadNamespaces lists the namespaces of collections, sorted.
func workloadNamespaces(db *mongo.Database, collections []config.CollectionDefinition) []string {
	seen := map[string]bool{}
	var out []string
	for _, col := range collections {
		ns := collectionNamespace(db, col)
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

func operationMixSummary(cfg *config.AppConfig) string {
	return fmt.Sprintf("find=%d update=%d delete=%d insert=%d insertMany=%d aggregate=%d transaction=%d",
		cfg.FindPercent, cfg.UpdatePercent, cfg.DeletePercent, cfg.InsertPercent, cfg.BulkInsertPercent, cfg.AggregatePercent, cfg.TransactionPercent)
}

// workloadRecordHeader describes the run a recording is made from. It holds
// hashes of the definitions, never the connection settings or query values.
func workloadRecordHeader(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, workers int) (workloadrec.Header, error) {
	colHash, err := workloadrec.DefinitionHash(collections)
	if err != nil {
		return workloadrec.Header{}, fmt.Errorf("hash collection definitions: %w", err)
	}
	queryHash, err := workloadrec.DefinitionHash(queries)
	if err != nil {
		return workloadrec.Header{}, fmt.Errorf("hash query definitions: %w", err)
	}
	return workloadrec.Header{
		Outcomes:        cfg.WorkloadRecordOutcomes,
		ResultChecksums: cfg.WorkloadRecordOutcomes && cfg.WorkloadRecordResultChecksums,
		PLGMVersion:     config.BuildVersion,
		ServerVersion:   serverVersionFn(ctx, db),
		Topology:        serverTopologyFn(ctx, db),
		RandomSeed:      cfg.RandomSeed,
		Workers:         workers,
		CollectionsHash: colHash,
		QueriesHash:     queryHash,
		Namespaces:      workloadNamespaces(db, collections),
		Config: workloadrec.ConfigSummary{
			Concurrency:     cfg.Concurrency,
			Duration:        cfg.Duration,
			DocumentsCount:  cfg.DocumentsCount,
			OperationMix:    operationMixSummary(cfg),
			LoadProfile:     strings.TrimSpace(cfg.LoadProfile.Kind),
			AccessPattern:   strings.TrimSpace(cfg.AccessPattern.Kind),
			ThinkTimeMs:     cfg.ThinkTimeMs,
			ThinkJitterMs:   cfg.ThinkJitterMs,
			FindBatchSize:   cfg.FindBatchSize,
			FindLimit:       cfg.FindLimit,
			InsertBatchSize: cfg.InsertBatchSize,
			UseTransactions: cfg.UseTransactions,
		},
	}, nil
}

func workloadRecordOptions(cfg *config.AppConfig) workloadrec.Options {
	return workloadrec.Options{MaxBytes: cfg.WorkloadRecordMaxBytes, Backpressure: cfg.WorkloadRecordBackpressure, Compression: cfg.WorkloadRecordCompression}
}

func maxBytesText(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("max %d bytes", n)
}

// startWorkloadRecording opens the recording file for a run with workers
// workers and iterations iterations.
func startWorkloadRecording(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, workers, iterations int) (*workloadrec.Recorder, error) {
	h, err := workloadRecordHeader(ctx, db, collections, queries, cfg, workers)
	if err != nil {
		return nil, err
	}
	if iterations > 1 {
		h.Iterations, h.IntervalDelay = iterations, strings.TrimSpace(cfg.IntervalDelay)
	}
	if err := recordDataFingerprint(ctx, db, collections, cfg, &h); err != nil {
		return nil, err
	}
	opts := workloadRecordOptions(cfg)
	rec, err := workloadrec.NewRecorder(cfg.WorkloadRecordPath, opts, h)
	if err != nil {
		return nil, err
	}
	log.Printf("[Workload Record] WARNING: %s holds the literal values of every operation (documents, filters, updates). It is written with mode 0600 and is never part of reports or insights; protect it like the data itself.", cfg.WorkloadRecordPath)
	log.Printf("[Workload Record] recording to %s (%s uncompressed, compression %s, %d iteration(s)). Policy: %s", cfg.WorkloadRecordPath, maxBytesText(opts.MaxBytes), cfg.WorkloadRecordCompression, max(iterations, 1), opts.Policy())
	return rec, nil
}

func workloadRecordingInfo(res workloadrec.Result, maxBytes int64) *stats.WorkloadRecordingInfo {
	info := &stats.WorkloadRecordingInfo{
		Path: res.Path, OpsWritten: res.Ops, Bytes: res.Bytes, Status: res.Status, Reason: res.Reason, CommandsHash: res.CommandsHash,
		MaxBytes: max(maxBytes, 0), Backpressure: res.Backpressure, Compression: res.Compression, FileBytes: res.FileBytes, Iterations: res.Iterations,
		Outcomes: res.Outcomes, ResultChecksums: res.ResultChecksums,
	}
	if res.FileBytes > 0 && res.Compression != workloadrec.CompressionNone {
		info.CompressionRatio = math.Round(float64(res.Bytes)/float64(res.FileBytes)*100) / 100
	}
	if res.Backpressure == workloadrec.BackpressureBlock {
		total, maxWait := float64(res.WaitTotal.Microseconds())/1000, float64(res.WaitMax.Microseconds())/1000
		info.Waits, info.WaitTotalMs, info.WaitMaxMs = res.Waits, &total, &maxWait
	}
	return info
}

// finishWorkloadRecording closes the recording after every worker stopped
// and logs its size on disk.
func finishWorkloadRecording(rec *workloadrec.Recorder, collector *stats.Collector, maxBytes int64) {
	res := rec.Close()
	info := workloadRecordingInfo(res, maxBytes)
	if collector != nil {
		collector.SetWorkloadRecording(info)
	}
	msg := fmt.Sprintf("[Workload Record] %s: %d operations, %d bytes (uncompressed) written to %s; %d bytes on disk", res.Status, res.Ops, res.Bytes, res.Path, res.FileBytes)
	if info.CompressionRatio > 0 {
		msg += fmt.Sprintf(" (%s, ratio %.2fx)", res.Compression, info.CompressionRatio)
	}
	if res.Iterations > 1 {
		msg += fmt.Sprintf("; %d iterations", res.Iterations)
	}
	if info.WaitTotalMs != nil {
		msg += fmt.Sprintf("; workers waited for the writer %d times, total %.1f ms, max %.1f ms", res.Waits, *info.WaitTotalMs, *info.WaitMaxMs)
	}
	if res.Reason != "" {
		msg += " (" + res.Reason + ")"
	}
	log.Print(msg)
}

type recordingSessionKey struct{}

// recordingSession keeps one recording open, and one traffic fingerprint
// growing, across the iterations of a run.
type recordingSession struct {
	rec       *workloadrec.Recorder
	iteration int
	collector *stats.Collector
	maxBytes  int64

	traffic          *trafficFingerprint
	trafficIteration int
	outcomes         *deterministicOutcomes
}

// WithWorkloadRecordingSession returns a context in which the RunWorkload
// calls of a run's iterations append to one recording, and a finish func to
// call once after the last iteration. finish closes the recording and
// reports it on the last iteration's collector; it does nothing when the
// run recorded nothing. The session also numbers the iterations of a
// deterministic run and keeps one traffic fingerprint and one outcome file
// for all of them. Call RunWorkload from one goroutine at a time.
func WithWorkloadRecordingSession(ctx context.Context) (context.Context, func()) {
	s := &recordingSession{}
	var once sync.Once
	return context.WithValue(ctx, recordingSessionKey{}, s), func() {
		once.Do(func() {
			if s.rec != nil {
				finishWorkloadRecording(s.rec, s.collector, s.maxBytes)
			}
			if s.outcomes != nil {
				s.outcomes.close(s.traffic, s.collector)
			}
		})
	}
}

// workloadRecorder returns the recorder for one RunWorkload call and the
// func to call when its workers have stopped. Inside a recording session
// the recorder spans every iteration; without one, a run records a single
// iteration.
func workloadRecorder(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, workers int, collector *stats.Collector) (*workloadrec.Recorder, func(), error) {
	sess, _ := ctx.Value(recordingSessionKey{}).(*recordingSession)
	if sess == nil {
		if cfg.Iterations > 1 {
			return nil, nil, fmt.Errorf("recording %d iterations needs one recording for the whole run (WithWorkloadRecordingSession)", cfg.Iterations)
		}
		rec, err := startWorkloadRecording(ctx, db, collections, queries, cfg, workers, 1)
		if err != nil {
			return nil, nil, err
		}
		collector.SetWorkloadRecording(workloadRecordingInfo(rec.Snapshot(), cfg.WorkloadRecordMaxBytes))
		return rec, func() { finishWorkloadRecording(rec, collector, cfg.WorkloadRecordMaxBytes) }, nil
	}
	if sess.rec == nil {
		rec, err := startWorkloadRecording(ctx, db, collections, queries, cfg, workers, cfg.Iterations)
		if err != nil {
			return nil, nil, err
		}
		sess.rec, sess.maxBytes = rec, cfg.WorkloadRecordMaxBytes
	}
	sess.iteration++
	sess.rec.SetIteration(sess.iteration)
	sess.collector = collector
	snapshot := func() {
		collector.SetWorkloadRecording(workloadRecordingInfo(sess.rec.Snapshot(), sess.maxBytes))
	}
	snapshot()
	return sess.rec, snapshot, nil
}
