package mongo

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The execute functions below are the only places a worker sends workload
// operations. Generated and replayed operations both go through them, so a
// replay reports the same stats, shapes, and explain samples as the run it
// was recorded from.

// opIdentity is the shape and query reference reported for an operation.
type opIdentity struct {
	shapeKey, shapeSummary string
	fields                 []string
	queryLabel             string
	querySource            string
	workloadName           string
	queryFile              string
	queryDefID             string
	querySummary           string
	pipelineSummary        string
	queryDefIndex          int
}

// opRequest is one find, aggregate, update, delete, insert, or insertMany.
// filter, pipeline, update, projection, doc, and docs are the values sent:
// generated maps, or the decoded bson.D/bson.A of a recording.
type opRequest struct {
	opType       string
	col          config.CollectionDefinition
	filter       interface{}
	pipeline     interface{}
	update       interface{}
	projection   interface{}
	upsert       bool
	doc          interface{}
	docs         []interface{}
	findLimit    int64
	findBatch    int32
	usedExisting bool
	// findSort, findSkip, and hint come only from imported traffic;
	// generated operations never set them.
	findSort interface{}
	findSkip int64
	hint     interface{}

	// Generated operations: the definition, the runtime filter/pipeline used
	// for shapes, and the documents registered in the record pool.
	q          config.QueryDefinition
	filterMap  map[string]interface{}
	pipelineIn []interface{}
	poolDoc    map[string]interface{}
	poolDocs   []interface{}

	// Replayed operations: the recorded line.
	line *workloadrec.OpLine
}

func (req *opRequest) identity() opIdentity {
	if req.line != nil && req.line.Shape != nil {
		id := opIdentity{shapeKey: req.line.Shape.Key, shapeSummary: req.line.Shape.Summary, fields: req.line.Shape.Fields,
			querySummary: req.line.Shape.QuerySummary, pipelineSummary: req.line.Shape.PipelineSummary, queryDefIndex: -1}
		if qr := req.line.Query; qr != nil {
			id.queryLabel, id.querySource, id.workloadName, id.queryFile, id.queryDefID, id.queryDefIndex = qr.Label, qr.Source, qr.Workload, qr.File, qr.ID, qr.Index
		}
		return id
	}
	filter, pipeline := req.filterMap, req.pipelineIn
	q := req.q
	if req.line != nil {
		filter, pipeline = req.runtimeFilter(), req.runtimePipeline()
		q = config.QueryDefinition{Projection: req.runtimeProjection(), Upsert: req.upsert, DefinitionIndex: -1}
		if qr := req.line.Query; qr != nil {
			q = config.QueryDefinition{Name: qr.Name, Label: qr.Label, SourceType: qr.Source, WorkloadName: qr.Workload, SourceFile: qr.File, ID: qr.ID, DefinitionIndex: qr.Index, Projection: q.Projection, Upsert: q.Upsert}
		}
	}
	var id opIdentity
	id.shapeKey, id.shapeSummary, id.fields = buildOperationShape(req.opType, filter, pipeline, shapeOptions{projection: q.Projection, limit: req.findLimit, upsert: q.Upsert})
	id.queryLabel, id.querySource, id.workloadName, id.queryFile, id.queryDefID, id.querySummary, id.pipelineSummary, id.queryDefIndex = buildQueryReferenceFromDefinition(q, req.opType, filter, pipeline)
	return id
}

func (req *opRequest) executableQuery() *stats.ExecutableQuery {
	if req.line == nil {
		return buildExecutableQuery(req.opType, req.col.DatabaseName, req.q, req.filterMap, req.pipelineIn, req.findLimit)
	}
	update, _ := runtimeValue(req.update, "", false).(map[string]interface{})
	q := config.QueryDefinition{Projection: req.runtimeProjection(), Update: update, Upsert: req.upsert}
	return buildExecutableQuery(req.opType, req.col.DatabaseName, q, req.runtimeFilter(), req.runtimePipeline(), req.findLimit)
}

func (req *opRequest) runtimeFilter() map[string]interface{} {
	m, _ := runtimeValue(req.filter, "", false).(map[string]interface{})
	return m
}

func (req *opRequest) runtimeProjection() map[string]interface{} {
	m, _ := runtimeValue(req.projection, "", false).(map[string]interface{})
	return m
}

func (req *opRequest) runtimePipeline() []interface{} {
	p, _ := runtimeValue(req.pipeline, "", false).([]interface{})
	return p
}

// runtimeValue turns decoded recorded values into the form generated
// operations use: objects become maps except at sort positions, which stay
// ordered bson.D, and arrays become []interface{}.
func runtimeValue(v interface{}, owner string, sortPos bool) interface{} {
	switch t := v.(type) {
	case bson.D:
		if sortPos {
			out := make(bson.D, len(t))
			for i, e := range t {
				out[i] = bson.E{Key: e.Key, Value: runtimeValue(e.Value, e.Key, false)}
			}
			return out
		}
		m := make(map[string]interface{}, len(t))
		for _, e := range t {
			m[e.Key] = runtimeValue(e.Value, e.Key, config.IsSortPosition(owner, e.Key))
		}
		return m
	case bson.A:
		out := make([]interface{}, len(t))
		for i, x := range t {
			out[i] = runtimeValue(x, "", false)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, x := range t {
			out[i] = runtimeValue(x, "", false)
		}
		return out
	}
	return v
}

// executeStandard sends req and records its stats. start is when the worker
// began the operation; the latency window runs from start to the reply. A
// non-nil out receives the operation's outcome.
func executeStandard(ctx context.Context, wCfg workloadConfig, req *opRequest, start time.Time, out *workloadrec.Outcome) (bool, opIdentity) {
	switch req.opType {
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany", "aggregate":
		wCfg.collector.RecordTargeting(req.usedExisting)
	}
	coll := getCollectionHandle(wCfg.database, req.col)
	ns := collectionNamespace(wCfg.database, req.col)
	success := true
	// opErr is the outcome's error; it includes cursor errors, which do not
	// count as failures in the stats.
	var opErr error
	var rr *resultReader
	setUpdate := func(res *mongo.UpdateResult) {
		if out != nil {
			out.Matched, out.Modified, out.Upserted = workloadrec.Count(res.MatchedCount), workloadrec.Count(res.ModifiedCount), workloadrec.Count(res.UpsertedCount)
		}
	}
	switch req.opType {
	case "find":
		rr = newResultReader(out, wCfg.checksums)
		findOpts := []options.Lister[options.FindOptions]{
			options.Find().SetLimit(req.findLimit),
			options.Find().SetProjection(req.projection),
		}
		// 0 (imported finds without a batch size) is the server default.
		if req.findBatch > 0 {
			findOpts = append(findOpts, options.Find().SetBatchSize(req.findBatch))
		}
		if req.findSort != nil {
			findOpts = append(findOpts, options.Find().SetSort(req.findSort))
		}
		if req.findSkip > 0 {
			findOpts = append(findOpts, options.Find().SetSkip(req.findSkip))
		}
		if req.hint != nil {
			findOpts = append(findOpts, options.Find().SetHint(req.hint))
		}
		cursor, err := coll.Find(ctx, req.filter, findOpts...)
		if err == nil {
			returned, cerr := drainCursor(ctx, cursor, rr)
			wCfg.collector.RecordFindResult(returned)
			opErr = cerr
		} else {
			success, opErr = false, err
		}
	case "aggregate":
		rr = newResultReader(out, wCfg.checksums)
		var aggOpts []options.Lister[options.AggregateOptions]
		if req.hint != nil {
			aggOpts = append(aggOpts, options.Aggregate().SetHint(req.hint))
		}
		cursor, err := coll.Aggregate(ctx, req.pipeline, aggOpts...)
		if err == nil {
			_, opErr = drainCursor(ctx, cursor, rr)
		} else {
			success, opErr = false, err
		}
	case "updateOne":
		opts := options.UpdateOne().SetUpsert(req.upsert)
		if res, err := coll.UpdateOne(ctx, req.filter, req.update, opts); err != nil {
			success, opErr = false, err
		} else {
			wCfg.collector.RecordUpdateResult(res.MatchedCount, res.ModifiedCount)
			setUpdate(res)
		}
	case "updateMany":
		opts := options.UpdateMany().SetUpsert(req.upsert)
		if res, err := coll.UpdateMany(ctx, req.filter, req.update, opts); err != nil {
			success, opErr = false, err
		} else {
			wCfg.collector.RecordUpdateResult(res.MatchedCount, res.ModifiedCount)
			setUpdate(res)
		}
	case "deleteOne":
		if res, err := coll.DeleteOne(ctx, req.filter); err != nil {
			success, opErr = false, err
		} else {
			wCfg.recordPool.RemoveMatching(ns, req.filterMap)
			wCfg.collector.RecordDeleteResult(res.DeletedCount)
			if out != nil {
				out.Deleted = workloadrec.Count(res.DeletedCount)
			}
		}
	case "deleteMany":
		if res, err := coll.DeleteMany(ctx, req.filter); err != nil {
			success, opErr = false, err
		} else {
			wCfg.recordPool.RemoveMatching(ns, req.filterMap)
			wCfg.collector.RecordDeleteResult(res.DeletedCount)
			if out != nil {
				out.Deleted = workloadrec.Count(res.DeletedCount)
			}
		}
	case "insert":
		if _, err := coll.InsertOne(ctx, req.doc); err != nil {
			success, opErr = false, err
		} else {
			registerInsertedDocument(wCfg.recordPool, wCfg.database, req.col, req.poolDoc)
		}
		if out != nil {
			out.Inserted = workloadrec.Count(insertedCount(1, opErr))
		}
	case "insertMany":
		if _, err := coll.InsertMany(ctx, req.docs); err != nil {
			success, opErr = false, err
		} else {
			registerInsertedDocuments(wCfg.recordPool, wCfg.database, req.col, req.poolDocs)
		}
		if out != nil {
			out.Inserted = workloadrec.Count(insertedCount(len(req.docs), opErr))
		}
	}
	dur := time.Since(start)
	if out != nil {
		finishOutcome(out, dur, opErr)
		rr.finish(out)
	}
	wCfg.collector.Track(req.opType, dur)
	id := req.identity()
	var execQuery *stats.ExecutableQuery
	if wCfg.collector.CapturesExecutableQueries() {
		execQuery = req.executableQuery()
	}
	wCfg.collector.RecordOperationEvent(req.opType, req.col.DatabaseName, req.col.Name, id.shapeKey, id.shapeSummary, id.fields, dur, success, wCfg.collector.CurrentIteration, execQuery, id.queryLabel, id.querySource, id.workloadName, id.queryFile, id.queryDefID, id.querySummary, id.pipelineSummary, id.queryDefIndex)
	return success, id
}

// standardCommand is the recorded form of req: the driver call's arguments.
func standardCommand(req *opRequest) bson.D {
	switch req.opType {
	case "find":
		cmd := bson.D{{Key: "filter", Value: emptyDocIfNil(req.filter)}}
		if !isNilDoc(req.projection) {
			cmd = append(cmd, bson.E{Key: "projection", Value: req.projection})
		}
		if req.findSort != nil {
			cmd = append(cmd, bson.E{Key: "sort", Value: req.findSort})
		}
		if req.findSkip > 0 {
			cmd = append(cmd, bson.E{Key: "skip", Value: req.findSkip})
		}
		if req.hint != nil {
			cmd = append(cmd, bson.E{Key: "hint", Value: req.hint})
		}
		return append(cmd, bson.E{Key: "limit", Value: req.findLimit}, bson.E{Key: "batchSize", Value: req.findBatch})
	case "aggregate":
		pipeline := req.pipeline
		if pipeline == nil {
			pipeline = bson.A{}
		}
		cmd := bson.D{{Key: "pipeline", Value: pipeline}}
		if req.hint != nil {
			cmd = append(cmd, bson.E{Key: "hint", Value: req.hint})
		}
		return cmd
	case "updateOne", "updateMany":
		return bson.D{{Key: "filter", Value: emptyDocIfNil(req.filter)}, {Key: "update", Value: req.update}, {Key: "upsert", Value: req.upsert}}
	case "deleteOne", "deleteMany":
		return bson.D{{Key: "filter", Value: emptyDocIfNil(req.filter)}}
	case "insert":
		return bson.D{{Key: "document", Value: req.doc}}
	case "insertMany":
		return bson.D{{Key: "documents", Value: req.docs}}
	}
	return bson.D{}
}

func emptyDocIfNil(v interface{}) interface{} {
	if isNilDoc(v) {
		return bson.D{}
	}
	return v
}

func isNilDoc(v interface{}) bool {
	if v == nil {
		return true
	}
	m, ok := v.(map[string]interface{})
	return ok && m == nil
}

func recordQueryRef(q config.QueryDefinition, id opIdentity) *workloadrec.QueryRef {
	return &workloadrec.QueryRef{Name: q.Name, Index: id.queryDefIndex, Label: id.queryLabel, Source: id.querySource, Workload: id.workloadName, File: id.queryFile, ID: id.queryDefID}
}

func recordShapeRef(id opIdentity) *workloadrec.ShapeRef {
	return &workloadrec.ShapeRef{Key: id.shapeKey, Summary: id.shapeSummary, Fields: id.fields, QuerySummary: id.querySummary, PipelineSummary: id.pipelineSummary}
}

// withDocumentID returns doc with an _id, so a recorded insert carries the
// _id the server stored. The driver would generate the same kind of
// client-side ObjectId; doc itself is not changed.
func withDocumentID(doc map[string]interface{}) map[string]interface{} {
	if doc == nil {
		return nil
	}
	if _, ok := doc["_id"]; ok {
		return doc
	}
	out := make(map[string]interface{}, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	out["_id"] = bson.NewObjectID()
	return out
}

func withDocumentIDs(docs []interface{}) []interface{} {
	out := make([]interface{}, len(docs))
	for i, d := range docs {
		if m, ok := d.(map[string]interface{}); ok {
			out[i] = withDocumentID(m)
		} else {
			out[i] = d
		}
	}
	return out
}

// withBatchIDs returns a copy of a raw insert command whose documents all
// have an _id.
func withBatchIDs(cmdRaw []byte) ([]byte, error) {
	return withBatchIDsFrom(cmdRaw, func(int) bson.ObjectID { return bson.NewObjectID() })
}

// withBatchIDsFrom is withBatchIDs with newID(j) as the _id of document j.
func withBatchIDsFrom(cmdRaw []byte, newID func(j int) bson.ObjectID) ([]byte, error) {
	var cmd bson.D
	if err := bson.Unmarshal(cmdRaw, &cmd); err != nil {
		return nil, err
	}
	for i, e := range cmd {
		if e.Key != "documents" {
			continue
		}
		docs, ok := e.Value.(bson.A)
		if !ok {
			return nil, fmt.Errorf("insert command documents is %T", e.Value)
		}
		for j, raw := range docs {
			d, ok := raw.(bson.D)
			if !ok {
				return nil, fmt.Errorf("insert command document is %T", raw)
			}
			has := false
			for _, f := range d {
				if f.Key == "_id" {
					has = true
					break
				}
			}
			if !has {
				docs[j] = append(bson.D{{Key: "_id", Value: newID(j)}}, d...)
			}
		}
		cmd[i].Value = docs
	}
	return bson.Marshal(cmd)
}

// executeFastInsert sends a prebuilt insert command of count documents.
// onSuccess runs inside the latency window, before it closes. A non-nil out
// receives the outcome, including write errors the reply reports.
func executeFastInsert(ctx context.Context, wCfg workloadConfig, workerID int, col config.CollectionDefinition, cmdRaw []byte, count int, start time.Time, onSuccess func(), out *workloadrec.Outcome) bool {
	fastCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	res := wCfg.database.RunCommand(fastCtx, bson.Raw(cmdRaw))
	var reply fastInsertReply
	var err error
	if out != nil {
		err = res.Decode(&reply)
	} else {
		err = res.Err()
	}
	cancel()
	if count < 1 {
		count = 1
	}
	if err != nil {
		if wCfg.debug {
			log.Printf("[Worker %d] FastInsert error: %v", workerID, err)
		}
		dur := time.Since(start)
		if out != nil {
			fastInsertOutcome(out, dur, reply, err)
		}
		wCfg.collector.Add("insert", 1, dur)
		wCfg.collector.RecordOperationEvent("insert", col.DatabaseName, col.Name, fastInsertShapeKey, "fast insert batch", nil, dur, false, wCfg.collector.CurrentIteration, nil, "fast insert batch", "optimized_path", "generated_workload", "runtime", "fast_insert_batch", "", "", -1)
		return false
	}
	if onSuccess != nil {
		onSuccess()
	}
	dur := time.Since(start)
	if out != nil {
		fastInsertOutcome(out, dur, reply, nil)
	}
	wCfg.collector.Add("insert", int64(count), dur/time.Duration(count))
	wCfg.collector.RecordOperationEvent("insert", col.DatabaseName, col.Name, fastInsertShapeKey, "fast insert batch", nil, dur, true, wCfg.collector.CurrentIteration, nil, "fast insert batch", "optimized_path", "generated_workload", "runtime", "fast_insert_batch", "", "", -1)
	return true
}

// fastFindCommand is the find the optimized path sends for ids.
func fastFindCommand(col config.CollectionDefinition, patchKey string, ids []int64, batchSize int) bson.D {
	return bson.D{
		{Key: "find", Value: col.Name},
		{Key: "filter", Value: bson.D{{Key: patchKey, Value: bson.D{{Key: "$in", Value: bson.RawValue{Type: TypeArray, Value: buildRawInt64Array(ids)}}}}}},
		{Key: "limit", Value: batchSize},
	}
}

// fastFindRecord is the recorded form of a fast find: the same command with
// the ids as a plain array.
func fastFindRecord(col config.CollectionDefinition, patchKey string, ids []int64, batchSize int) bson.D {
	return bson.D{
		{Key: "find", Value: col.Name},
		{Key: "filter", Value: bson.D{{Key: patchKey, Value: bson.D{{Key: "$in", Value: ids}}}}},
		{Key: "limit", Value: batchSize},
	}
}

// executeFastFind sends the optimized $in find on patchKey. A non-nil out
// receives the outcome of the first batch, which is all the find reads.
func executeFastFind(ctx context.Context, wCfg workloadConfig, workerID int, col config.CollectionDefinition, patchKey string, ids []int64, batchSize int, pooled bool, start time.Time, out *workloadrec.Outcome) bool {
	wCfg.collector.RecordTargeting(pooled)
	cmd := fastFindCommand(col, patchKey, ids, batchSize)

	var fastFindRes struct {
		Cursor struct {
			FirstBatch []bson.Raw `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	fastCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := wCfg.database.RunCommand(fastCtx, cmd).Decode(&fastFindRes)
	cancel()
	if err == nil {
		wCfg.collector.RecordFindResult(int64(len(fastFindRes.Cursor.FirstBatch)))
	}

	fastShapeKey := fastFindShapeKey(batchSize)
	var fastQuery *stats.ExecutableQuery
	if wCfg.collector.CapturesExecutableQueries() {
		fastQuery = &stats.ExecutableQuery{
			Database: wCfg.database.Name(),
			Filter:   map[string]interface{}{patchKey: map[string]interface{}{"$in": append([]int64(nil), ids...)}},
			Limit:    int64(batchSize),
		}
	}
	if err != nil && wCfg.debug {
		log.Printf("[Worker %d] FastFind error: %v", workerID, err)
	}
	dur := time.Since(start)
	if out != nil {
		finishOutcome(out, dur, err)
		if err == nil {
			rr := newResultReader(out, wCfg.checksums)
			for _, d := range fastFindRes.Cursor.FirstBatch {
				rr.add(d)
			}
			rr.finish(out)
		}
	}
	wCfg.collector.Add("find", 1, dur)
	wCfg.collector.RecordOperationEvent("find", col.DatabaseName, col.Name, fastShapeKey, fmt.Sprintf("fast find on %s", patchKey), []string{patchKey}, dur, err == nil, wCfg.collector.CurrentIteration, fastQuery, fmt.Sprintf("fast find on %s", patchKey), "optimized_path", "generated_workload", "runtime", "fast_find_patch_key", fmt.Sprintf("{%s:{\"$in\":\"<batch>\"}}", patchKey), "", -1)
	return err == nil
}

// txInnerOp is one operation inside a transaction.
type txInnerOp struct {
	opType string
	col    config.CollectionDefinition
	filter interface{}
	update interface{}
	upsert bool
	doc    interface{}
	docs   []interface{}

	// Generated operations only.
	filterMap map[string]interface{}
	poolDoc   map[string]interface{}
	poolDocs  []interface{}
}

// executeTxInnerOp sends one operation inside a transaction.
func executeTxInnerOp(sessCtx context.Context, wCfg workloadConfig, op *txInnerOp) {
	coll := getCollectionHandle(wCfg.database, op.col)
	ns := collectionNamespace(wCfg.database, op.col)
	switch op.opType {
	case "find":
		cursor, err := coll.Find(sessCtx, op.filter, options.Find().SetLimit(1))
		if err == nil {
			for cursor.Next(sessCtx) {
			}
			_ = cursor.Close(sessCtx)
		}
	case "updateOne":
		opts := options.UpdateOne().SetUpsert(op.upsert)
		coll.UpdateOne(sessCtx, op.filter, op.update, opts)
	case "updateMany":
		opts := options.UpdateMany().SetUpsert(op.upsert)
		coll.UpdateMany(sessCtx, op.filter, op.update, opts)
	case "deleteOne":
		if _, err := coll.DeleteOne(sessCtx, op.filter); err == nil {
			wCfg.recordPool.RemoveMatching(ns, op.filterMap)
		}
	case "deleteMany":
		if _, err := coll.DeleteMany(sessCtx, op.filter); err == nil {
			wCfg.recordPool.RemoveMatching(ns, op.filterMap)
		}
	case "insert":
		if _, err := coll.InsertOne(sessCtx, op.doc); err == nil {
			registerInsertedDocument(wCfg.recordPool, wCfg.database, op.col, op.poolDoc)
		}
	case "insertMany":
		if _, err := coll.InsertMany(sessCtx, op.docs); err == nil {
			registerInsertedDocuments(wCfg.recordPool, wCfg.database, op.col, op.poolDocs)
		}
	}
}

// txInnerCommand is the recorded form of one transaction operation.
func txInnerCommand(db *mongo.Database, op *txInnerOp) bson.D {
	var args bson.D
	switch op.opType {
	case "find", "deleteOne", "deleteMany":
		args = bson.D{{Key: "filter", Value: emptyDocIfNil(op.filter)}}
	case "updateOne", "updateMany":
		args = bson.D{{Key: "filter", Value: emptyDocIfNil(op.filter)}, {Key: "update", Value: op.update}, {Key: "upsert", Value: op.upsert}}
	case "insert":
		args = bson.D{{Key: "document", Value: op.doc}}
	case "insertMany":
		args = bson.D{{Key: "documents", Value: op.docs}}
	}
	return bson.D{{Key: "op", Value: op.opType}, {Key: "ns", Value: collectionNamespace(db, op.col)}, {Key: "args", Value: args}}
}

// recordOp hands one sent operation to the recorder, if recording is on.
func recordOp(wCfg workloadConfig, e workloadrec.Entry, start time.Time) {
	if !wCfg.recorder.Active() {
		return
	}
	e.Offset = wCfg.recorder.Offset(start)
	wCfg.recorder.Record(e)
}
