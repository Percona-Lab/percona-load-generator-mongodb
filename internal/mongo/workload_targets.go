package mongo

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type workloadTargetKey struct{}

// workloadTarget names the target of one run of a multi-target replay.
type workloadTarget struct {
	label      string
	concurrent bool
	progress   *TargetProgress
}

// TargetProgress is the live replay progress of one target.
type TargetProgress struct {
	Sent, Skipped, Total atomic.Uint64
}

// WithWorkloadTarget returns a context for the run of one target of a
// multi-target replay or deterministic run: its outcome file names the
// target, and a concurrent replay measures how far each target falls behind
// the recorded timeline (client-side lag), because the targets share the
// PLGM host. A replay counts its progress in progress when it is not nil.
func WithWorkloadTarget(ctx context.Context, label string, concurrent bool, progress *TargetProgress) context.Context {
	return context.WithValue(ctx, workloadTargetKey{}, workloadTarget{label: label, concurrent: concurrent, progress: progress})
}

func targetFromContext(ctx context.Context) workloadTarget {
	t, _ := ctx.Value(workloadTargetKey{}).(workloadTarget)
	return t
}

var dataFingerprintFn = dataFingerprint

// dataFingerprint reads the starting data of collections: the exact
// document count and the index list of each, plus the dbHash collection
// hash in dbhash mode. dbHash needs the dbHash privilege and does not run
// through mongos; a failure is kept as an error name, not a run error.
func dataFingerprint(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, mode string) (*workloadrec.DataFingerprint, error) {
	out := &workloadrec.DataFingerprint{Mode: mode}
	if db == nil {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	seen := map[string]bool{}
	hashes := map[string]map[string]string{}
	hashErr := map[string]string{}
	for _, col := range collections {
		ns := collectionNamespace(db, col)
		if seen[ns] {
			continue
		}
		seen[ns] = true
		dbName, colName, _ := strings.Cut(ns, ".")
		c := db.Client().Database(dbName).Collection(colName)
		count, err := c.CountDocuments(cctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", ns, err)
		}
		idx, err := indexFingerprints(cctx, c)
		if err != nil {
			return nil, fmt.Errorf("list indexes of %s: %w", ns, err)
		}
		cf := workloadrec.CollectionFingerprint{NS: ns, Count: count, Indexes: idx}
		if mode == workloadrec.FingerprintDBHash {
			if _, done := hashes[dbName]; !done && hashErr[dbName] == "" {
				h, err := dbHashes(cctx, db.Client().Database(dbName))
				if err != nil {
					_, name := errorCodeName(err)
					hashErr[dbName] = name
				} else {
					hashes[dbName] = h
				}
			}
			cf.DBHash, cf.DBHashError = hashes[dbName][colName], hashErr[dbName]
			if cf.DBHashError == "" && cf.DBHash == "" && count > 0 {
				cf.DBHashError = "not_reported"
			}
		}
		out.Collections = append(out.Collections, cf)
	}
	sort.Slice(out.Collections, func(i, j int) bool { return out.Collections[i].NS < out.Collections[j].NS })
	return out, nil
}

func dbHashes(ctx context.Context, db *mongo.Database) (map[string]string, error) {
	var res struct {
		Collections map[string]string `bson:"collections"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "dbHash", Value: 1}}).Decode(&res); err != nil {
		return nil, err
	}
	return res.Collections, nil
}

// indexFingerprints lists a collection's indexes by name with a canonical
// spec: the keys (numeric directions as int32) and the options that change
// what the index holds. Fields the server adds (v, ns) are left out.
func indexFingerprints(ctx context.Context, c *mongo.Collection) ([]workloadrec.IndexFingerprint, error) {
	cur, err := c.Indexes().List(ctx)
	if err != nil {
		if code, _ := errorCodeName(err); code == 26 {
			return []workloadrec.IndexFingerprint{}, nil
		}
		return nil, err
	}
	var specs []bson.D
	if err := cur.All(ctx, &specs); err != nil {
		return nil, err
	}
	return indexFingerprintsFromSpecs(specs), nil
}

func indexFingerprintsFromSpecs(specs []bson.D) []workloadrec.IndexFingerprint {
	out := make([]workloadrec.IndexFingerprint, 0, len(specs))
	for _, s := range specs {
		name, _ := docValue(s, "name").(string)
		spec := bson.D{}
		if key, ok := docValue(s, "key").(bson.D); ok {
			k := make(bson.D, len(key))
			for i, e := range key {
				k[i] = bson.E{Key: e.Key, Value: normalizeKeyValue(e.Value)}
			}
			spec = append(spec, bson.E{Key: "key", Value: k})
		}
		for _, opt := range []string{"unique", "sparse", "hidden", "expireAfterSeconds", "partialFilterExpression", "collation", "weights", "2dsphereIndexVersion"} {
			if v := docValue(s, opt); v != nil {
				if n, ok := intValue(v); ok {
					v = n
				}
				spec = append(spec, bson.E{Key: opt, Value: v})
			}
		}
		b, err := bson.MarshalExtJSON(spec, false, false)
		if err != nil {
			b = []byte(fmt.Sprint(spec))
		}
		out = append(out, workloadrec.IndexFingerprint{Name: name, Spec: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// recordDataFingerprint adds the starting-data fingerprint to a recording
// header when workload_data_fingerprint is on.
func recordDataFingerprint(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, cfg *config.AppConfig, h *workloadrec.Header) error {
	mode := config.DataFingerprintMode(cfg)
	if mode == config.DataFingerprintOff {
		return nil
	}
	fp, err := dataFingerprintFn(ctx, db, collections, mode)
	if err != nil {
		return fmt.Errorf("starting-data fingerprint: %w", err)
	}
	h.DataFingerprint = fp
	log.Printf("[Workload Record] starting-data fingerprint (%s) of %d collection(s) stored in the header", mode, len(fp.Collections))
	return nil
}

// checkDataFingerprint compares the target's starting data with the
// recording's fingerprint. It returns nil when the recording has none.
func checkDataFingerprint(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, recorded *workloadrec.DataFingerprint) (*workloadrec.FingerprintCheck, error) {
	if recorded == nil {
		return nil, nil
	}
	got, err := dataFingerprintFn(ctx, db, collections, recorded.Mode)
	if err != nil {
		return nil, fmt.Errorf("starting-data fingerprint: %w", err)
	}
	c := workloadrec.CompareDataFingerprints(recorded, got)
	return &c, nil
}

// isReplayWrite reports whether a recorded operation changes data. An
// aggregate is a write when its pipeline has a $out or $merge stage; a
// transaction when any of its operations is not a find.
func isReplayWrite(op *workloadrec.OpLine) bool {
	if op.Path == workloadrec.PathFastInsertBatch {
		return true
	}
	switch op.Op {
	case "find":
		return false
	case "aggregate":
		if !bytes.Contains(op.Cmd, []byte(`"$out"`)) && !bytes.Contains(op.Cmd, []byte(`"$merge"`)) {
			return false
		}
		cmd, err := workloadrec.DecodeCommand(op.Cmd)
		if err != nil {
			return true
		}
		p, _ := docValue(cmd, "pipeline").(bson.A)
		for _, st := range p {
			if d, ok := st.(bson.D); ok && (hasKey(d, "$out") || hasKey(d, "$merge")) {
				return true
			}
		}
		return false
	case "transaction":
		cmd, err := workloadrec.DecodeCommand(op.Cmd)
		if err != nil {
			return true
		}
		ops, _ := docValue(cmd, "ops").(bson.A)
		for _, x := range ops {
			d, _ := x.(bson.D)
			if t, _ := docValue(d, "op").(string); t != "find" {
				return true
			}
		}
		return false
	}
	return true
}
