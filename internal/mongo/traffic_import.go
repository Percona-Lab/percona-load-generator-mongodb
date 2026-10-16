package mongo

import (
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ImportedOperation turns the arguments of an imported operation (the keys
// decodeStandard reads, for example filter/projection/sort/skip/hint/limit/
// batchSize for a find) into the command a replay sends and records, and the
// shape a replay reports for it. Importing through the same decode and
// encode as a replay keeps the recorded commands hash equal to the replay's.
func ImportedOperation(ns, op string, args bson.D) (bson.D, *workloadrec.ShapeRef, error) {
	dbName, colName, _ := strings.Cut(ns, ".")
	line := &workloadrec.OpLine{NS: ns, Op: op}
	req, err := decodeStandard(line, args, config.CollectionDefinition{DatabaseName: dbName, Name: colName})
	if err != nil {
		return nil, nil, err
	}
	cmd := standardCommand(req)
	// Shapes come from the decoded form, as during a replay.
	decoded, err := roundTrip(cmd)
	if err != nil {
		return nil, nil, err
	}
	if req, err = decodeStandard(line, decoded, req.col); err != nil {
		return nil, nil, err
	}
	return cmd, recordShapeRef(req.identity()), nil
}

func roundTrip(cmd bson.D) (bson.D, error) {
	b, err := workloadrec.EncodeCommand(cmd)
	if err != nil {
		return nil, err
	}
	return workloadrec.DecodeCommand(b)
}

// importedReplayWarnings explains what a replay of captured traffic cannot
// check or reproduce.
func importedReplayWarnings(imp *workloadrec.ImportInfo, cfg *config.AppConfig) []string {
	out := []string{"the recording was imported from captured traffic (" + strings.Join(imp.Sources, ", ") + "): collection and query definitions are not compared, and every operation runs on the namespace it was captured on"}
	if cfg.DropCollections || (!cfg.SkipSeed && cfg.DocumentsCount > 0) {
		out = append(out, "drop_collections or seeding was on: the configured collections were reset before the imported traffic ran; captured traffic expects each target to hold a copy of the captured data (turn drop_collections off and skip_seed on)")
	}
	if n := imp.Flagged[workloadrec.FlagServerGenerated]; n > 0 {
		out = append(out, "some imported operations use server-generated values (for example inserts without _id, $currentDate, $$NOW, $rand); their results can differ between targets")
	}
	return out
}
