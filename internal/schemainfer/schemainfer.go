// Package schemainfer infers a workload/schema model from MongoDB query logs.
//
// It accepts mongod structured slow-query logs (JSON lines with an "attr"
// section), system.profile-style documents (with "ns"/"op"/"command"), or raw
// command documents (e.g. {"find":"orders","filter":{...}}). From these it
// derives the collections touched, per-operation field/sort/update patterns, a
// suggested read/write operation mix, and candidate query definitions that can
// pre-fill a benchmark configuration.
//
// Parsing is intentionally tolerant: malformed or unrecognized lines are counted
// and reported as warnings rather than aborting the whole inference.
package schemainfer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

const maxSuggestedQueries = 100

// Collection summarizes activity observed against one namespace.
type Collection struct {
	Namespace       string         `json:"namespace"`
	Database        string         `json:"database"`
	Collection      string         `json:"collection"`
	OperationCounts map[string]int `json:"operationCounts"`
	Fields          []string       `json:"fields"`
}

// Operation summarizes one (namespace, operation) pattern.
type Operation struct {
	Namespace        string                 `json:"namespace"`
	Database         string                 `json:"database"`
	Collection       string                 `json:"collection"`
	Operation        string                 `json:"operation"`
	Count            int                    `json:"count"`
	FilterFields     []string               `json:"filterFields"`
	SortFields       []string               `json:"sortFields"`
	ProjectionFields []string               `json:"projectionFields"`
	UpdateFields     []string               `json:"updateFields"`
	SampleFilter     map[string]interface{} `json:"sampleFilter,omitempty"`
}

// OperationMix is a suggested benchmark distribution (percentages summing ~100).
type OperationMix struct {
	FindPercent      int `json:"findPercent"`
	InsertPercent    int `json:"insertPercent"`
	UpdatePercent    int `json:"updatePercent"`
	DeletePercent    int `json:"deletePercent"`
	AggregatePercent int `json:"aggregatePercent"`
}

// Result is the full inference output.
type Result struct {
	TotalLines       int                      `json:"totalLines"`
	ParsedEntries    int                      `json:"parsedEntries"`
	SkippedLines     int                      `json:"skippedLines"`
	Warnings         []string                 `json:"warnings"`
	Collections      []Collection             `json:"collections"`
	Operations       []Operation              `json:"operations"`
	SuggestedMix     OperationMix             `json:"suggestedMix"`
	SuggestedQueries []config.QueryDefinition `json:"suggestedQueries"`
}

// Infer parses the provided query-log text and returns an inference Result. It
// never returns an error for malformed content; problems are surfaced via
// Result.Warnings and the parsed/skipped counters.
func Infer(logText string) Result {
	res := Result{Warnings: []string{}, Collections: []Collection{}, Operations: []Operation{}, SuggestedQueries: []config.QueryDefinition{}}

	lines := splitEntries(logText)
	res.TotalLines = len(lines)

	collAgg := map[string]*collAccum{}
	opAgg := map[string]*opAccum{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		obj, err := parseJSONObject(line)
		if err != nil {
			res.SkippedLines++
			continue
		}
		ent, ok := extractEntry(obj)
		if !ok {
			res.SkippedLines++
			continue
		}
		res.ParsedEntries++
		accumulate(ent, collAgg, opAgg)
	}

	if res.ParsedEntries == 0 && res.TotalLines > 0 {
		res.Warnings = append(res.Warnings, "No recognizable MongoDB query-log entries were found. Provide mongod JSON slow-query logs, system.profile documents, or raw command documents.")
	}

	res.Collections = finalizeCollections(collAgg)
	res.Operations = finalizeOperations(opAgg)
	res.SuggestedMix = suggestMix(res.Operations)
	res.SuggestedQueries = suggestQueries(res.Operations)
	return res
}

// splitEntries returns one string per entry. It supports either newline-
// delimited JSON objects or a single top-level JSON array of objects.
func splitEntries(text string) []string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, raw := range arr {
				out = append(out, string(raw))
			}
			return out
		}
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func parseJSONObject(s string) (map[string]interface{}, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("empty object")
	}
	return obj, nil
}

// entry is a normalized single log observation.
type entry struct {
	namespace  string
	database   string
	collection string
	operation  string
	filter     map[string]interface{}
	sortFields []string
	projection map[string]interface{}
	update     map[string]interface{}
}

// extractEntry normalizes the supported log shapes into an entry.
func extractEntry(obj map[string]interface{}) (entry, bool) {
	// mongod structured slow-query log: {"attr":{"ns":..,"command":{...}}}
	if attr, ok := getMap(obj["attr"]); ok {
		ns := getString(attr["ns"])
		if cmd, ok := getMap(attr["command"]); ok {
			return entryFromCommand(ns, cmd)
		}
		// Some log shapes nest under originatingCommand.
		if cmd, ok := getMap(attr["originatingCommand"]); ok {
			return entryFromCommand(ns, cmd)
		}
	}

	// system.profile-style: {"ns":"db.coll","op":"query","command":{...}}
	if ns := getString(obj["ns"]); ns != "" {
		if cmd, ok := getMap(obj["command"]); ok {
			return entryFromCommand(ns, cmd)
		}
		if op := getString(obj["op"]); op != "" {
			e := entry{}
			e.setNamespace(ns)
			e.operation = normalizeOp(op)
			if e.operation == "" {
				return entry{}, false
			}
			if q, ok := getMap(obj["query"]); ok {
				e.filter = q
			}
			return e, e.operation != ""
		}
	}

	// Raw command document: {"find":"orders","filter":{...},"$db":"shop"}
	return entryFromCommand("", obj)
}

func entryFromCommand(ns string, cmd map[string]interface{}) (entry, bool) {
	op, coll := classifyCommand(cmd)
	if op == "" {
		return entry{}, false
	}
	e := entry{operation: op}
	if ns == "" {
		db := getString(cmd["$db"])
		if db != "" && coll != "" {
			ns = db + "." + coll
		}
	}
	if ns != "" {
		e.setNamespace(ns)
	} else if coll != "" {
		e.collection = coll
		e.namespace = coll
	}

	switch op {
	case "find":
		if f, ok := getMap(cmd["filter"]); ok {
			e.filter = f
		} else if f, ok := getMap(cmd["query"]); ok {
			e.filter = f
		}
		if s, ok := getMap(cmd["sort"]); ok {
			e.sortFields = mapKeys(s)
		}
		if p, ok := getMap(cmd["projection"]); ok {
			e.projection = p
		}
	case "aggregate":
		if pipe, ok := getArray(cmd["pipeline"]); ok {
			e.filter, e.sortFields = fieldsFromPipeline(pipe)
		}
	case "update":
		if ups, ok := getArray(cmd["updates"]); ok && len(ups) > 0 {
			if first, ok := getMap(ups[0]); ok {
				if q, ok := getMap(first["q"]); ok {
					e.filter = q
				}
				if u, ok := getMap(first["u"]); ok {
					e.update = u
				}
			}
		} else {
			// findAndModify shape
			if q, ok := getMap(cmd["query"]); ok {
				e.filter = q
			}
			if u, ok := getMap(cmd["update"]); ok {
				e.update = u
			}
		}
	case "delete":
		if dels, ok := getArray(cmd["deletes"]); ok && len(dels) > 0 {
			if first, ok := getMap(dels[0]); ok {
				if q, ok := getMap(first["q"]); ok {
					e.filter = q
				}
			}
		} else if q, ok := getMap(cmd["query"]); ok {
			e.filter = q
		}
	case "insert":
		if docs, ok := getArray(cmd["documents"]); ok && len(docs) > 0 {
			if first, ok := getMap(docs[0]); ok {
				e.update = first // reuse update slot to capture inserted fields
			}
		}
	}
	return e, true
}

func (e *entry) setNamespace(ns string) {
	e.namespace = ns
	if idx := strings.Index(ns, "."); idx >= 0 {
		e.database = ns[:idx]
		e.collection = ns[idx+1:]
	} else {
		e.collection = ns
	}
}

// classifyCommand maps a command document to a normalized operation and the
// target collection name (the value of the command key).
func classifyCommand(cmd map[string]interface{}) (op string, coll string) {
	// Order matters: check the most specific/known command verbs.
	for _, key := range []string{"find", "insert", "update", "delete", "aggregate", "count", "distinct", "findAndModify", "findandmodify", "getMore"} {
		if v, ok := cmd[key]; ok {
			if s, ok := v.(string); ok {
				coll = s
			}
			return normalizeOp(key), coll
		}
	}
	return "", ""
}

// normalizeOp maps raw command/op names into the workload's operation
// categories used by the runner and config.
func normalizeOp(raw string) string {
	switch strings.ToLower(raw) {
	case "find", "query", "getmore", "count", "distinct":
		return "find"
	case "insert":
		return "insert"
	case "update", "findandmodify":
		return "update"
	case "delete", "remove":
		return "delete"
	case "aggregate", "command": // some profiler entries label aggregate as command
		return "aggregate"
	default:
		return ""
	}
}

// fieldsFromPipeline pulls $match filter fields and $sort fields from an
// aggregation pipeline.
func fieldsFromPipeline(pipe []interface{}) (filter map[string]interface{}, sortFields []string) {
	for _, stage := range pipe {
		sm, ok := getMap(stage)
		if !ok {
			continue
		}
		if m, ok := getMap(sm["$match"]); ok && filter == nil {
			filter = m
		}
		if s, ok := getMap(sm["$sort"]); ok {
			sortFields = append(sortFields, mapKeys(s)...)
		}
	}
	return filter, sortFields
}

// --- aggregation accumulators ---

type collAccum struct {
	database   string
	collection string
	opCounts   map[string]int
	fields     map[string]struct{}
}

type opAccum struct {
	namespace    string
	database     string
	collection   string
	operation    string
	count        int
	filterFields map[string]struct{}
	sortFields   map[string]struct{}
	projFields   map[string]struct{}
	updateFields map[string]struct{}
	sampleFilter map[string]interface{}
}

func accumulate(e entry, collAgg map[string]*collAccum, opAgg map[string]*opAccum) {
	ca := collAgg[e.namespace]
	if ca == nil {
		ca = &collAccum{database: e.database, collection: e.collection, opCounts: map[string]int{}, fields: map[string]struct{}{}}
		collAgg[e.namespace] = ca
	}
	ca.opCounts[e.operation]++

	key := e.namespace + "|" + e.operation
	oa := opAgg[key]
	if oa == nil {
		oa = &opAccum{
			namespace:    e.namespace,
			database:     e.database,
			collection:   e.collection,
			operation:    e.operation,
			filterFields: map[string]struct{}{},
			sortFields:   map[string]struct{}{},
			projFields:   map[string]struct{}{},
			updateFields: map[string]struct{}{},
		}
		opAgg[key] = oa
	}
	oa.count++

	filterFields := map[string]struct{}{}
	collectFilterFields(e.filter, "", filterFields)
	for f := range filterFields {
		oa.filterFields[f] = struct{}{}
		ca.fields[f] = struct{}{}
	}
	for _, f := range e.sortFields {
		oa.sortFields[f] = struct{}{}
		ca.fields[f] = struct{}{}
	}
	for f := range e.projection {
		oa.projFields[f] = struct{}{}
		ca.fields[f] = struct{}{}
	}
	updateFields := map[string]struct{}{}
	collectUpdateFields(e.update, updateFields)
	for f := range updateFields {
		oa.updateFields[f] = struct{}{}
		ca.fields[f] = struct{}{}
	}
	if oa.sampleFilter == nil && len(e.filter) > 0 {
		oa.sampleFilter = e.filter
	}
}

// collectFilterFields records the field paths referenced by a filter document,
// descending into $and/$or/$nor logical operators.
func collectFilterFields(filter map[string]interface{}, prefix string, out map[string]struct{}) {
	for k, v := range filter {
		if strings.HasPrefix(k, "$") {
			switch k {
			case "$and", "$or", "$nor":
				if arr, ok := v.([]interface{}); ok {
					for _, sub := range arr {
						if m, ok := getMap(sub); ok {
							collectFilterFields(m, prefix, out)
						}
					}
				}
			}
			continue
		}
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		out[path] = struct{}{}
	}
}

// collectUpdateFields records fields touched by an update document, handling
// both operator-style ($set/$inc/...) and replacement-style updates, as well as
// inserted-document field capture.
func collectUpdateFields(update map[string]interface{}, out map[string]struct{}) {
	for k, v := range update {
		if strings.HasPrefix(k, "$") {
			if m, ok := getMap(v); ok {
				for f := range m {
					out[f] = struct{}{}
				}
			}
			continue
		}
		out[k] = struct{}{}
	}
}

func finalizeCollections(collAgg map[string]*collAccum) []Collection {
	out := make([]Collection, 0, len(collAgg))
	for ns, ca := range collAgg {
		out = append(out, Collection{
			Namespace:       ns,
			Database:        ca.database,
			Collection:      ca.collection,
			OperationCounts: ca.opCounts,
			Fields:          sortedKeys(ca.fields),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

func finalizeOperations(opAgg map[string]*opAccum) []Operation {
	out := make([]Operation, 0, len(opAgg))
	for _, oa := range opAgg {
		out = append(out, Operation{
			Namespace:        oa.namespace,
			Database:         oa.database,
			Collection:       oa.collection,
			Operation:        oa.operation,
			Count:            oa.count,
			FilterFields:     sortedKeys(oa.filterFields),
			SortFields:       sortedKeys(oa.sortFields),
			ProjectionFields: sortedKeys(oa.projFields),
			UpdateFields:     sortedKeys(oa.updateFields),
			SampleFilter:     oa.sampleFilter,
		})
	}
	// Sort by descending count, then namespace/op for stability.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Operation < out[j].Operation
	})
	return out
}

// suggestMix turns observed operation counts into rounded percentages that sum
// to 100 (when any operations were observed).
func suggestMix(ops []Operation) OperationMix {
	counts := map[string]int{}
	total := 0
	for _, o := range ops {
		counts[o.Operation] += o.Count
		total += o.Count
	}
	if total == 0 {
		return OperationMix{}
	}
	order := []string{"find", "insert", "update", "delete", "aggregate"}
	pct := map[string]int{}
	assigned := 0
	for _, k := range order {
		p := int(float64(counts[k]) / float64(total) * 100.0)
		pct[k] = p
		assigned += p
	}
	// Distribute rounding remainder to the largest category.
	if remainder := 100 - assigned; remainder != 0 {
		largest := "find"
		for _, k := range order {
			if counts[k] > counts[largest] {
				largest = k
			}
		}
		pct[largest] += remainder
		if pct[largest] < 0 {
			pct[largest] = 0
		}
	}
	return OperationMix{
		FindPercent:      pct["find"],
		InsertPercent:    pct["insert"],
		UpdatePercent:    pct["update"],
		DeletePercent:    pct["delete"],
		AggregatePercent: pct["aggregate"],
	}
}

// suggestQueries builds candidate QueryDefinitions (one per observed op pattern)
// to pre-fill a benchmark configuration.
func suggestQueries(ops []Operation) []config.QueryDefinition {
	out := make([]config.QueryDefinition, 0, len(ops))
	for _, o := range ops {
		if len(out) >= maxSuggestedQueries {
			break
		}
		if o.Collection == "" {
			continue
		}
		q := config.QueryDefinition{
			Database:    o.Database,
			Collection:  o.Collection,
			Operation:   o.Operation,
			Name:        fmt.Sprintf("%s_%s", o.Operation, o.Collection),
			Description: fmt.Sprintf("Inferred from %d log entries", o.Count),
		}
		if len(o.SampleFilter) > 0 {
			q.Filter = o.SampleFilter
		} else {
			q.Filter = map[string]interface{}{}
		}
		out = append(out, q)
	}
	return out
}

// --- small helpers ---

func getMap(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

func getArray(v interface{}) ([]interface{}, bool) {
	a, ok := v.([]interface{})
	return a, ok
}

func getString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func mapKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
