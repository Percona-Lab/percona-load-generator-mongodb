package webui

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

const sortSpillSource = "explain executionStats of the representative command, re-run after the workload"

// planChildKeys are the plan-tree links between execution stages.
var planChildKeys = []string{"inputStage", "thenStage", "elseStage", "outerStage", "innerStage"}

// peakMemoryFields are per-stage peak memory fields; memUsage is the pre-4.4
// SORT field.
var peakMemoryFields = []string{"maxMemoryUsageBytes", "peakTrackedMemBytes", "memUsage"}

type sortSpillCollector struct {
	out         *stats.SortSpillMetrics
	partial     []string
	plannerOnly bool
	// read counts executed stages examined; partial evidence with none read
	// is unavailable.
	read int
	// version is the serverInfo.version of the explain being read (the
	// shard's own when it reports one, otherwise the top-level one).
	version string
}

// collectSortSpill reads the per-stage sort, spill, memory, and timing fields
// of the executed plan. Only executionStats trees and executed pipeline
// stages are read, so rejected plans, queryPlanner trees, and the echoed
// command never contribute. Router nodes above shard plans are skipped and
// each stage is reported on its own, so no counter is counted twice.
func collectSortSpill(root map[string]interface{}, m stats.ExplainMetrics) *stats.SortSpillMetrics {
	s := &stats.SortSpillMetrics{Source: sortSpillSource}
	switch m.Layout {
	case layoutPlannerOnly:
		s.Status, s.Reason = stats.SortSpillUnavailable, "planner_only_no_execution_stats"
		return s
	case layoutUnsupported:
		s.Status, s.Reason = stats.SortSpillUnavailable, "unsupported_explain_layout"
		if m.UnsupportedReason != "" {
			s.Reason += ": " + m.UnsupportedReason
		}
		return s
	}
	rootVersion := explainServerVersion(root)
	c := &sortSpillCollector{out: s, version: rootVersion}
	if shards, ok := explainDoc(root["shards"]); ok && len(shards) > 0 {
		names := make([]string, 0, len(shards))
		for n := range shards {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			d, ok := explainDoc(shards[n])
			if !ok {
				c.markPartial("shard_explain_not_a_document")
				continue
			}
			c.version = rootVersion
			if info, ok := explainDoc(d["serverInfo"]); ok {
				if v, ok := info["version"].(string); ok && v != "" {
					c.version = v
				}
			}
			c.node(d, "shards."+n+".", n)
		}
		if root["splitPipeline"] != nil {
			c.markPartial("merge_stages_not_measured")
		}
	} else {
		c.node(root, "", "")
	}
	switch {
	case c.plannerOnly:
		s.Status, s.Reason, s.Stages = stats.SortSpillUnavailable, "planner_only_no_execution_stats", nil
	case len(c.partial) > 0 && c.read == 0:
		s.Status, s.Reason = stats.SortSpillUnavailable, strings.Join(c.partial, ", ")
	case len(c.partial) > 0:
		s.Status, s.Reason = stats.SortSpillPartial, strings.Join(c.partial, ", ")
	default:
		s.Status = stats.SortSpillMeasured
	}
	return s
}

func (c *sortSpillCollector) markPartial(reason string) {
	c.partial = appendUniqueString(c.partial, reason)
}

// node reads one mongod-level explain (or a mongos plan-layout explain).
func (c *sortSpillCollector) node(doc map[string]interface{}, prefix, shard string) {
	if stages, ok := explainArray(doc["stages"]); ok {
		c.pipeline(stages, prefix+"stages", "", shard)
		return
	}
	es, ok := explainDoc(doc["executionStats"])
	if !ok {
		if shard != "" {
			c.markPartial("shard_execution_stats_missing")
		} else {
			c.plannerOnly = true
		}
		return
	}
	planner, _ := explainDoc(doc["queryPlanner"])
	ns, _ := planner["namespace"].(string)
	root, ok := explainDoc(es["executionStages"])
	if !ok {
		c.markPartial("execution_stages_not_reported")
		return
	}
	c.plan(root, prefix+"executionStats.executionStages", ns, shard)
}

// pipeline reads an aggregate "stages" array: the $cursor's executed plan
// and each later stage's own fields. A stage's specification (for example the
// $sort sort key or a $lookup sub-pipeline) is never read as a measurement.
func (c *sortSpillCollector) pipeline(stages []interface{}, base, ns, shard string) {
	for i, raw := range stages {
		st, ok := explainDoc(raw)
		if !ok {
			continue
		}
		path := fmt.Sprintf("%s[%d]", base, i)
		if cur, ok := explainDoc(st["$cursor"]); ok {
			planner, _ := explainDoc(cur["queryPlanner"])
			ns, _ = planner["namespace"].(string)
			es, ok := explainDoc(cur["executionStats"])
			if !ok {
				c.plannerOnly = true
				return
			}
			root, ok := explainDoc(es["executionStages"])
			if !ok {
				c.markPartial("execution_stages_not_reported")
				continue
			}
			c.plan(root, path+".$cursor.executionStats.executionStages", ns, shard)
			continue
		}
		name := pipelineStageKey(st)
		if name == "$facet" {
			if facets, ok := explainDoc(st["$facet"]); ok {
				keys := make([]string, 0, len(facets))
				for k := range facets {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if sub, ok := explainArray(facets[k]); ok {
						c.pipeline(sub, path+".$facet."+k, ns, shard)
					}
				}
			}
		}
		if name == "$unionWith" {
			c.markPartial("union_with_subpipeline_not_measured")
		}
		c.add(st, name, path, ns, shard, "pipeline")
	}
}

func pipelineStageKey(st map[string]interface{}) string {
	keys := make([]string, 0, 1)
	for k := range st {
		if strings.HasPrefix(k, "$") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// plan walks an executionStages tree. A mongos node with shards[] is a
// router merge node: its own fields are skipped and each shard plan is read.
func (c *sortSpillCollector) plan(node map[string]interface{}, path, ns, shard string) {
	if shards, ok := explainArray(node["shards"]); ok {
		for i, raw := range shards {
			d, ok := explainDoc(raw)
			if !ok {
				c.markPartial("shard_explain_not_a_document")
				continue
			}
			name, _ := d["shardName"].(string)
			st, ok := explainDoc(d["executionStages"])
			if !ok {
				c.markPartial("shard_execution_stages_missing")
				continue
			}
			c.plan(st, fmt.Sprintf("%s.shards[%d].executionStages", path, i), ns, name)
		}
		return
	}
	stage, _ := node["stage"].(string)
	engine := "classic"
	if stage != "" && stage == strings.ToLower(stage) {
		engine = "sbe"
	}
	c.add(node, stage, path, ns, shard, engine)
	for _, k := range planChildKeys {
		if child, ok := explainDoc(node[k]); ok {
			c.plan(child, path+"."+k, ns, shard)
		}
	}
	if children, ok := explainArray(node["inputStages"]); ok {
		for i, raw := range children {
			if child, ok := explainDoc(raw); ok {
				c.plan(child, fmt.Sprintf("%s.inputStages[%d]", path, i), ns, shard)
			}
		}
	}
}

// add records a stage that is a sort or reports any spill or memory field.
func (c *sortSpillCollector) add(doc map[string]interface{}, stage, path, ns, shard, engine string) {
	c.read++
	isSort := strings.EqualFold(stage, "SORT") || stage == "$sort"
	counter := func(keys ...string) *stats.ExplainCounter {
		for _, k := range keys {
			if v, ok := explainNumber(doc[k]); ok {
				return &stats.ExplainCounter{Value: v, Source: path + "." + k}
			}
		}
		return nil
	}
	r := stats.StageResourceMetric{Path: path, Stage: stage, Sort: isSort, Engine: engine, Namespace: ns, Shard: shard, ServerVersion: c.version}
	if isSort {
		r.SortBytesProcessed = counter("totalDataSizeSorted", "totalDataSizeSortedBytesEstimate")
	}
	if b, ok := doc["usedDisk"].(bool); ok {
		r.UsedDisk = &b
	}
	r.Spills = counter("spills")
	r.SpilledRecords = counter("spilledRecords")
	r.SpilledBytes = counter("spilledBytes", "numBytesSpilledEstimate")
	r.SpillStorageBytes = counter("spilledDataStorageSize")
	r.MemoryLimitBytes = counter("memLimit")
	for _, k := range peakMemoryFields {
		if v := counter(k); v != nil {
			r.PeakMemory = append(r.PeakMemory, stats.MemoryCounter{Scope: "stage", ExplainCounter: *v})
		}
	}
	if acc, ok := explainDoc(doc["maxAccumulatorMemoryUsageBytes"]); ok {
		names := make([]string, 0, len(acc))
		for k := range acc {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if v, ok := explainNumber(acc[k]); ok {
				r.PeakMemory = append(r.PeakMemory, stats.MemoryCounter{Scope: "accumulator " + k,
					ExplainCounter: stats.ExplainCounter{Value: v, Source: path + ".maxAccumulatorMemoryUsageBytes." + k}})
			}
		}
	}
	reported := r.SortBytesProcessed != nil || r.UsedDisk != nil || r.Spills != nil || r.SpilledRecords != nil ||
		r.SpilledBytes != nil || r.SpillStorageBytes != nil || r.MemoryLimitBytes != nil || len(r.PeakMemory) > 0
	if !isSort && !reported {
		return
	}
	r.TimeMsEstimate = counter("executionTimeMillisEstimate")
	if isSort {
		if r.SortBytesProcessed == nil {
			r.Missing = append(r.Missing, "sort_bytes_processed")
		}
		if r.UsedDisk == nil && r.Spills == nil {
			r.Missing = append(r.Missing, "spill_status")
		}
	}
	spilled := (r.UsedDisk != nil && *r.UsedDisk) || (r.Spills != nil && r.Spills.Value > 0)
	if spilled && r.Spills == nil && r.SpilledBytes == nil && r.SpillStorageBytes == nil {
		r.Missing = append(r.Missing, "spill_amount")
	}
	// On 7.0 and 8.0 the slot-based sort stage adds the sorter's bytesSorted to
	// totalDataSizeSorted before calling done(), and the sorter counts bytes
	// only when it sorts a run (on spill or in done()). Without a spill the
	// field is therefore 0 whatever was sorted. Only an empty sort
	// (nReturned 0) makes that 0 true; other versions keep the reported value.
	if isSort && engine == "sbe" && !spilled && r.SortBytesProcessed != nil && r.SortBytesProcessed.Value == 0 &&
		r.SortBytesProcessed.Source == path+".totalDataSizeSorted" && sbeSortBytesReadBeforeDone(c.version) {
		if n, ok := explainNumber(doc["nReturned"]); !ok || n > 0 {
			r.Caveats = append(r.Caveats, stats.CaveatSBESortBytesWithoutSpill)
			c.markPartial(stats.CaveatSBESortBytesWithoutSpill)
		}
	}
	if len(r.Missing) > 0 {
		c.markPartial("stage_fields_not_reported")
	}
	c.out.Stages = append(c.out.Stages, r)
}

// sbeSortBytesReadBeforeDone reports whether the server's release branch is
// one whose slot-based sort reads totalDataSizeSorted before the in-memory
// run is sorted: 7.0 and 8.0 (checked in the r7.0.0, r7.0.14, r8.0.0, r8.0.4,
// and r8.0.12 sources; observed on 8.0.4). The 6.0 and 8.2+ code differs and
// was not checked, so their reported values are kept as measurements.
func sbeSortBytesReadBeforeDone(version string) bool {
	return strings.HasPrefix(version, "7.0.") || strings.HasPrefix(version, "8.0.")
}

// explainNumber reads any BSON or JSON numeric value; fractional values are
// truncated. ok is false for absent, non-numeric, or non-finite values.
func explainNumber(v interface{}) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float32:
		return explainNumber(float64(t))
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int64(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return explainNumber(f)
	}
	return 0, false
}
