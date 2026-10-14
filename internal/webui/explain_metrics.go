package webui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Explain layouts recognized by normalizeExplainMetrics.
const (
	layoutPlan            = "plan"             // executionStats on a mongod (find, write, fully pushed-down aggregate)
	layoutPipeline        = "pipeline"         // aggregate "stages" array with a leading $cursor
	layoutShardedPlan     = "sharded_plan"     // mongos executionStats with executionStages.shards[]
	layoutShardedPipeline = "sharded_pipeline" // mongos aggregate with a top-level shards map
	layoutPlannerOnly     = "planner_only"
	layoutUnsupported     = "unsupported"
)

// pipelineReducingStages are pushed-down pipeline stages whose output is not
// the access stage's output; their input is the access-stage count.
var pipelineReducingStages = map[string]struct{}{"GROUP": {}}

// lookupPushdownStages make the access-stage count ambiguous when pushed down
// because the SBE execution tree does not name them.
var lookupPushdownStages = []string{"EQ_LOOKUP", "EQ_LOOKUP_UNWIND"}

var writeStages = map[string]struct{}{
	"UPDATE": {}, "DELETE": {}, "BATCHED_DELETE": {}, "EXPRESS_UPDATE": {}, "EXPRESS_DELETE": {},
}

func explainDoc(v interface{}) (map[string]interface{}, bool) {
	switch t := v.(type) {
	case bson.M:
		return map[string]interface{}(t), true
	case map[string]interface{}:
		return t, true
	case bson.D:
		m := make(map[string]interface{}, len(t))
		for _, e := range t {
			m[e.Key] = e.Value
		}
		return m, true
	}
	return nil, false
}

func explainArray(v interface{}) ([]interface{}, bool) {
	switch t := v.(type) {
	case bson.A:
		return []interface{}(t), true
	case []interface{}:
		return t, true
	}
	return nil, false
}

// explainInt reads a numeric field; ok is false when it is absent or not a number.
func explainInt(doc map[string]interface{}, key string) (int64, bool) {
	if doc == nil {
		return 0, false
	}
	switch t := doc[key].(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	}
	return 0, false
}

func explainCounter(doc map[string]interface{}, key, source string) *stats.ExplainCounter {
	v, ok := explainInt(doc, key)
	if !ok {
		return nil
	}
	return &stats.ExplainCounter{Value: v, Source: source}
}

func stageName(doc map[string]interface{}) string {
	s, _ := doc["stage"].(string)
	return strings.ToUpper(strings.TrimSpace(s))
}

// normalizeExplainMetrics extracts execution counters for op from an explain
// result, recording where each came from.
func normalizeExplainMetrics(out bson.M, op string) stats.ExplainMetrics {
	root := map[string]interface{}(out)
	var m stats.ExplainMetrics
	if shards, ok := explainDoc(root["shards"]); ok && len(shards) > 0 {
		m = normalizeShardedPipeline(root, shards, op)
	} else {
		m = normalizeNodeExplain(root, op, "")
	}
	m.SortSpill = collectSortSpill(root, m)
	return m
}

// normalizeNodeExplain handles one mongod's (or mongos plan-layout) explain.
func normalizeNodeExplain(doc map[string]interface{}, op, prefix string) stats.ExplainMetrics {
	if stages, ok := explainArray(doc["stages"]); ok {
		return normalizePipelineStages(stages, op, prefix)
	}
	es, ok := explainDoc(doc["executionStats"])
	if !ok {
		return stats.ExplainMetrics{Layout: layoutPlannerOnly}
	}
	planner, _ := explainDoc(doc["queryPlanner"])
	root, _ := explainDoc(es["executionStages"])
	if shards, ok := explainArray(root["shards"]); ok {
		return normalizeShardedPlan(es, root, shards, planner, op, prefix+"executionStats")
	}
	m := stats.ExplainMetrics{Layout: layoutPlan}
	fillPlanCounters(&m, es, root, planner, op, prefix+"executionStats")
	return m
}

// fillPlanCounters reads the executionStats totals, which already cover the
// whole plan tree; per-node counters are never added to them.
func fillPlanCounters(m *stats.ExplainMetrics, es, root, planner map[string]interface{}, op, base string) {
	m.DocsExamined = explainCounter(es, "totalDocsExamined", base+".totalDocsExamined")
	m.KeysExamined = explainCounter(es, "totalKeysExamined", base+".totalKeysExamined")
	switch {
	case isExplainWriteOp(op):
		m.Matched = writeMatchedCounter(root, op, base+".executionStages")
	case op == "aggregate":
		m.Returned = explainCounter(es, "nReturned", base+".nReturned")
		m.AccessReturned = accessStageCounter(m, root, planner, base+".executionStages")
	default:
		m.Returned = explainCounter(es, "nReturned", base+".nReturned")
	}
}

func isExplainWriteOp(op string) bool {
	switch op {
	case "updateOne", "updateMany", "deleteOne", "deleteMany":
		return true
	}
	return false
}

// accessStageCounter is the number of documents the access stage produced.
// Below the lowest pushed-down reducing stage (for example $group) that is the
// reducing stage's input; with no reducing stage it is the tree's output.
func accessStageCounter(m *stats.ExplainMetrics, root, planner map[string]interface{}, base string) *stats.ExplainCounter {
	if root == nil {
		m.Notes = appendUniqueString(m.Notes, "execution_stages_not_reported")
		return nil
	}
	if planHasStage(planner, lookupPushdownStages...) {
		m.Notes = appendUniqueString(m.Notes, "lookup_pushdown_access_count_ambiguous")
		return nil
	}
	path := base
	var input map[string]interface{}
	inputPath := ""
	node := root
	for node != nil {
		next, hasNext := explainDoc(node["inputStage"])
		if _, ok := pipelineReducingStages[stageName(node)]; ok {
			if !hasNext {
				m.Notes = appendUniqueString(m.Notes, "reducing_stage_input_not_reported")
				return nil
			}
			input, inputPath = next, path+"["+strings.ToLower(stageName(node))+"].inputStage"
		}
		if !hasNext {
			break
		}
		node, path = next, path+".inputStage"
	}
	if input == nil {
		return explainCounter(root, "nReturned", base+".nReturned")
	}
	c := explainCounter(input, "nReturned", inputPath+".nReturned")
	if c == nil {
		m.Notes = appendUniqueString(m.Notes, "reducing_stage_input_not_reported")
	}
	return c
}

// writeMatchedCounter reads the documents a write matched from its write
// stage. Updates report nMatched; the express update path reports only
// nWouldModify, which can only undercount matches. Deletes report nWouldDelete.
func writeMatchedCounter(root map[string]interface{}, op, base string) *stats.ExplainCounter {
	path := base
	for node := root; node != nil; {
		if _, ok := writeStages[stageName(node)]; ok {
			if strings.HasPrefix(op, "delete") {
				return explainCounter(node, "nWouldDelete", path+".nWouldDelete")
			}
			if c := explainCounter(node, "nMatched", path+".nMatched"); c != nil {
				return c
			}
			if c := explainCounter(node, "nWouldModify", path+".nWouldModify"); c != nil {
				c.LowerBound = true
				return c
			}
			return nil
		}
		next, ok := explainDoc(node["inputStage"])
		if !ok {
			break
		}
		node, path = next, path+".inputStage"
	}
	return nil
}

// planHasStage reports whether the winning plan (never rejected plans) has
// any of the named stages.
func planHasStage(planner map[string]interface{}, names ...string) bool {
	wp, ok := explainDoc(planner["winningPlan"])
	if !ok {
		return false
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	var walk func(v interface{}) bool
	walk = func(v interface{}) bool {
		if arr, ok := explainArray(v); ok {
			for _, x := range arr {
				if walk(x) {
					return true
				}
			}
			return false
		}
		doc, ok := explainDoc(v)
		if !ok {
			return false
		}
		if _, hit := want[stageName(doc)]; hit {
			return true
		}
		for k, child := range doc {
			if strings.EqualFold(k, "rejectedPlans") {
				continue
			}
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(wp)
}

// normalizePipelineStages handles an aggregate explain with a "stages" array.
// Counters come from the leading $cursor; the final output is the last stage's
// nReturned.
func normalizePipelineStages(stages []interface{}, op, prefix string) stats.ExplainMetrics {
	m := stats.ExplainMetrics{Layout: layoutPipeline}
	first, _ := explainDoc(firstOrNil(stages))
	cursor, ok := explainDoc(first["$cursor"])
	if !ok {
		m.Layout, m.UnsupportedReason = layoutUnsupported, "pipeline_without_cursor_stage"
		return m
	}
	var lookupStages []map[string]interface{}
	var lookupPaths []string
	for i, raw := range stages[1:] {
		if st, ok := explainDoc(raw); ok {
			if _, has := st["$lookup"]; has {
				m.Notes = appendUniqueString(m.Notes, "lookup_work_not_scored")
				lookupStages = append(lookupStages, st)
				lookupPaths = append(lookupPaths, fmt.Sprintf("%sstages[%d]", prefix, i+1))
			}
		}
	}
	if len(lookupStages) > 0 {
		m.Lookup = lookupMetrics(lookupStages, lookupPaths)
	}
	base := prefix + "stages[0].$cursor.executionStats"
	if es, ok := explainDoc(cursor["executionStats"]); ok {
		planner, _ := explainDoc(cursor["queryPlanner"])
		root, _ := explainDoc(es["executionStages"])
		fillPlanCounters(&m, es, root, planner, "aggregate", base)
	}
	last, _ := explainDoc(stages[len(stages)-1])
	m.Returned = explainCounter(last, "nReturned", fmt.Sprintf("%sstages[%d].nReturned", prefix, len(stages)-1))
	return m
}

// lookupMetrics sums the joined-collection counters that executionStats
// reports on each $lookup stage. A stage missing a counter makes that sum
// unknown.
func lookupMetrics(stages []map[string]interface{}, paths []string) *stats.LookupMetrics {
	out := &stats.LookupMetrics{Stages: len(stages)}
	sum := func(key string) *stats.ExplainCounter {
		total := &stats.ExplainCounter{}
		for i, st := range stages {
			c := explainCounter(st, key, paths[i]+"."+key)
			if c == nil {
				return nil
			}
			total.Value += c.Value
		}
		total.Source = paths[0] + "." + key
		if len(stages) > 1 {
			total.Source = fmt.Sprintf("sum($lookup stages.%s) over %d stages", key, len(stages))
		}
		return total
	}
	out.DocsExamined = sum("totalDocsExamined")
	out.KeysExamined = sum("totalKeysExamined")
	out.CollectionScans = sum("collectionScans")
	return out
}

func firstOrNil(a []interface{}) interface{} {
	if len(a) == 0 {
		return nil
	}
	return a[0]
}

// normalizeShardedPlan handles a mongos plan layout. Router-level totals are
// authoritative; per-shard counters are summed only when the router omits a
// total, and the two are never added together.
func normalizeShardedPlan(es, root map[string]interface{}, shards []interface{}, planner map[string]interface{}, op, base string) stats.ExplainMetrics {
	m := stats.ExplainMetrics{Layout: layoutShardedPlan, Shards: len(shards)}
	shardDocs := make([]map[string]interface{}, 0, len(shards))
	for _, raw := range shards {
		if d, ok := explainDoc(raw); ok {
			shardDocs = append(shardDocs, d)
		}
	}
	routerOrShards := func(key string) *stats.ExplainCounter {
		if c := explainCounter(es, key, base+"."+key); c != nil {
			return c
		}
		if c := explainCounter(root, key, base+".executionStages."+key); c != nil {
			return c
		}
		return sumShardCounters(shardDocs, len(shards), base+".executionStages.shards[*]", func(d map[string]interface{}, src string) *stats.ExplainCounter {
			return explainCounter(d, key, src+"."+key)
		})
	}
	m.DocsExamined = routerOrShards("totalDocsExamined")
	m.KeysExamined = routerOrShards("totalKeysExamined")
	switch {
	case isExplainWriteOp(op):
		m.Matched = sumShardCounters(shardDocs, len(shards), base+".executionStages.shards[*]", func(d map[string]interface{}, src string) *stats.ExplainCounter {
			stages, _ := explainDoc(d["executionStages"])
			return writeMatchedCounter(stages, op, src+".executionStages")
		})
	case op == "aggregate":
		m.Returned = explainCounter(es, "nReturned", base+".nReturned")
		m.AccessReturned = sumShardCounters(shardDocs, len(shards), base+".executionStages.shards[*]", func(d map[string]interface{}, src string) *stats.ExplainCounter {
			stages, _ := explainDoc(d["executionStages"])
			return accessStageCounter(&m, stages, shardPlanner(planner, d), src+".executionStages")
		})
	default:
		// The router's nReturned is after the merge (and any mongos limit);
		// shard nReturned values are not summed in its place.
		m.Returned = explainCounter(es, "nReturned", base+".nReturned")
	}
	return m
}

// shardPlanner finds the queryPlanner entry for a shard of a mongos plan.
func shardPlanner(planner, shard map[string]interface{}) map[string]interface{} {
	name, _ := shard["shardName"].(string)
	wp, _ := explainDoc(planner["winningPlan"])
	list, _ := explainArray(wp["shards"])
	for _, raw := range list {
		if d, ok := explainDoc(raw); ok {
			if n, _ := d["shardName"].(string); n == name {
				return d
			}
		}
	}
	return nil
}

// sumShardCounters adds one non-overlapping counter across shards. Any shard
// missing the counter makes the sum unknown rather than an undercount.
func sumShardCounters(shards []map[string]interface{}, want int, source string, get func(map[string]interface{}, string) *stats.ExplainCounter) *stats.ExplainCounter {
	if len(shards) == 0 || len(shards) != want {
		return nil
	}
	total := &stats.ExplainCounter{}
	var field string
	for i, d := range shards {
		c := get(d, source)
		if c == nil {
			return nil
		}
		total.Value += c.Value
		total.LowerBound = total.LowerBound || c.LowerBound
		if i == 0 {
			field = c.Source
		}
	}
	total.Source = fmt.Sprintf("sum(%s) over %d shards", field, len(shards))
	return total
}

// normalizeShardedPipeline handles a mongos aggregate with a per-shard map.
// Shard counters do not overlap, so they are summed; the merged final output
// is not reported unless the whole pipeline ran on a single shard.
func normalizeShardedPipeline(root, shards map[string]interface{}, op string) stats.ExplainMetrics {
	names := make([]string, 0, len(shards))
	for n := range shards {
		names = append(names, n)
	}
	sort.Strings(names)
	m := stats.ExplainMetrics{Layout: layoutShardedPipeline, Shards: len(names)}
	subs := make([]stats.ExplainMetrics, 0, len(names))
	for _, n := range names {
		d, ok := explainDoc(shards[n])
		if !ok {
			m.Layout, m.UnsupportedReason = layoutUnsupported, "shard_explain_not_a_document"
			return m
		}
		sub := normalizeNodeExplain(d, op, "shards."+n+".")
		if sub.Layout == layoutUnsupported {
			m.Layout, m.UnsupportedReason = layoutUnsupported, sub.UnsupportedReason
			return m
		}
		for _, note := range sub.Notes {
			m.Notes = appendUniqueString(m.Notes, note)
		}
		subs = append(subs, sub)
	}
	sum := func(get func(stats.ExplainMetrics) *stats.ExplainCounter) *stats.ExplainCounter {
		total := &stats.ExplainCounter{}
		for _, s := range subs {
			c := get(s)
			if c == nil {
				return nil
			}
			total.Value += c.Value
			total.LowerBound = total.LowerBound || c.LowerBound
		}
		if len(subs) == 0 {
			return nil
		}
		total.Source = fmt.Sprintf("sum(shards.*.%s) over %d shards", shardRelativeSource(get(subs[0]).Source), len(subs))
		return total
	}
	m.DocsExamined = sum(func(s stats.ExplainMetrics) *stats.ExplainCounter { return s.DocsExamined })
	m.KeysExamined = sum(func(s stats.ExplainMetrics) *stats.ExplainCounter { return s.KeysExamined })
	m.AccessReturned = sum(func(s stats.ExplainMetrics) *stats.ExplainCounter { return s.AccessReturned })
	if len(subs) == 1 && root["splitPipeline"] == nil && subs[0].Returned != nil {
		m.Returned = subs[0].Returned
	} else {
		m.Notes = appendUniqueString(m.Notes, "merged_output_not_reported")
	}
	return m
}

// shardRelativeSource drops the "shards.<name>." prefix so a sum can name the
// per-shard field it added.
func shardRelativeSource(src string) string {
	rest, ok := strings.CutPrefix(src, "shards.")
	if !ok {
		return src
	}
	if i := strings.Index(rest, "."); i >= 0 {
		return rest[i+1:]
	}
	return src
}

func appendUniqueString(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
