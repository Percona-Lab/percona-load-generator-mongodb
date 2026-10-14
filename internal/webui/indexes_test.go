package webui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestDecodeListedIndexPreservesOrderAndDoesNotLeakPartialLiterals(t *testing.T) {
	raw, err := bson.Marshal(bson.D{
		{Key: "v", Value: int32(2)},
		{Key: "key", Value: bson.D{{Key: "status", Value: int32(1)}, {Key: "qty", Value: int32(-1)}}},
		{Key: "name", Value: "status_1_qty_-1"},
		{Key: "hidden", Value: true},
		{Key: "sparse", Value: true},
		{Key: "partialFilterExpression", Value: bson.D{{Key: "status", Value: "open-secret"}}},
		{Key: "collation", Value: bson.D{{Key: "locale", Value: "en"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := decodeListedIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Name != "status_1_qty_-1" || !idx.Hidden || !idx.Sparse || idx.Collation != "en" || idx.Type != "btree" {
		t.Fatalf("flags %+v", idx)
	}
	if len(idx.Keys) != 2 || idx.Keys[0].Field != "status" || idx.Keys[0].Direction != 1 || idx.Keys[1].Field != "qty" || idx.Keys[1].Direction != -1 {
		t.Fatalf("key order %+v", idx.Keys)
	}
	if idx.Multikey != nil {
		t.Fatalf("absent multikey must stay unknown, got %+v", idx.Multikey)
	}
	if idx.PartialSummary != "status:$eq" || len(idx.PartialPredicates) != 1 || idx.PartialPredicates[0].Value != "s:open-secret" {
		t.Fatalf("partial %+v summary %s", idx.PartialPredicates, idx.PartialSummary)
	}
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "open-secret") {
		t.Fatalf("serialized index leaked a partial-filter literal: %s", b)
	}
}

func TestIndexAdviceConsistentAcrossAPIReportAndExport(t *testing.T) {
	origRun, origList := runExplainJobsFn, listNamespaceIndexesFn
	defer func() {
		runExplainJobsFn, listNamespaceIndexesFn = origRun, origList
	}()
	runExplainJobsFn = func(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, verbosity string, retries int, backoffMs int) []explainEvidence {
		out := make([]explainEvidence, len(jobs))
		for i := range jobs {
			d := &stats.ExplainDiagnostics{
				ReplayDB: jobs[i].sample.Query.Database, ReplayCollection: jobs[i].sample.Collection, Verbosity: verbosity,
				ExecutionStatsAvailable: true, CollectionScanDetected: true, WinningPlanSummary: "COLLSCAN",
				DocsExamined: 50000, NReturned: 2, ExecutionTimeMillis: 4,
				Metrics: &stats.ExplainMetrics{
					Layout:       "plan",
					DocsExamined: &stats.ExplainCounter{Value: 50000, Source: "executionStats.totalDocsExamined"},
					KeysExamined: &stats.ExplainCounter{Value: 0, Source: "executionStats.totalKeysExamined"},
					Returned:     &stats.ExplainCounter{Value: 2, Source: "executionStats.nReturned"},
				},
			}
			stats.DescribeExplainSample(d, jobs[i].sample.Operation)
			out[i] = explainEvidence{status: "explained", reason: d.Diagnosis, attempts: 1, diag: d}
		}
		return out
	}
	var listed []string
	listNamespaceIndexesFn = func(ctx context.Context, cfg *config.AppConfig, namespaces []string) stats.IndexInventoryView {
		listed = append([]string(nil), namespaces...)
		view := stats.CollectIndexInventory(ctx, namespaces, func(_ context.Context, ns string) stats.NamespaceIndexInventory {
			return stats.NamespaceIndexInventory{
				Status: "fetched",
				Indexes: []stats.DeployedIndex{{
					Name: "_id_", Type: "btree",
					Keys: []stats.IndexKeySpec{{Field: "_id", Direction: 1, Kind: "asc"}},
				}},
			}
		})
		return view
	}

	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled: true, InsightsSamplingRate: 1, InsightsSlowThresholdMs: 200, InsightsMaxEvents: 200, InsightsMaxGroups: 50,
		InsightsExplainEnabled: true, InsightsExplainTopN: 1, InsightsExplainSeverityMode: "high_and_low",
	})
	for i := 0; i < 12; i++ {
		slow := &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"email": "secret-value-123"}}
		c.RecordOperationEvent("find", "shop", "users", "find|email", "find email", []string{"email"}, 900*1e6, true, 1, slow, "by email", "unit_test", "w", "q.json", "q1", "{email:<str>}", "", 0)
		fast := &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"sku": "sku-secret"}}
		c.RecordOperationEvent("find", "shop", "users", "find|sku", "find sku", []string{"sku"}, 1e6, true, 1, fast, "by sku", "unit_test", "w", "q.json", "q2", "{sku:<str>}", "", 1)
	}
	s := NewServer(&config.AppConfig{Duration: "1s"})
	s.CurrentStats = c

	api := getInsightsJSON(t, s)
	if !reflect.DeepEqual(listed, []string{"shop.users"}) {
		t.Fatalf("expected one listIndexes namespace, got %v", listed)
	}
	recs, _ := api["recommendations"].([]interface{})
	var fast, slow map[string]interface{}
	for _, raw := range recs {
		rec := raw.(map[string]interface{})
		if rec["source"] != "index_advice" {
			continue
		}
		keys, _ := rec["candidate_keys"].([]interface{})
		joined := make([]string, len(keys))
		for i, k := range keys {
			joined[i] = k.(string)
		}
		switch strings.Join(joined, ",") {
		case "email:1":
			slow = rec
		case "sku:1":
			fast = rec
		}
	}
	if slow == nil || fast == nil {
		t.Fatalf("expected advice for the slow and fast shapes, got %+v", recs)
	}
	if fast["namespace"] != "shop.users" || fast["validation_step"] == "" || fast["confidence"] == "" {
		t.Fatalf("fast advice %+v", fast)
	}
	slowRows, _ := api["slow_queries"].([]interface{})
	for _, raw := range slowRows {
		row := raw.(map[string]interface{})
		if row["shape_summary"] == "find sku" {
			t.Fatalf("fast shape must not be required in the severity view: %+v", row)
		}
	}
	body, _ := json.Marshal(api)
	if strings.Contains(string(body), "secret-value-123") || strings.Contains(string(body), "sku-secret") {
		t.Fatalf("API leaked a query literal")
	}

	exported, err := json.Marshal(map[string]interface{}{"insights": api})
	if err != nil {
		t.Fatal(err)
	}
	var rt map[string]interface{}
	if err := json.Unmarshal(exported, &rt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt["insights"].(map[string]interface{})["recommendations"], recs) {
		t.Fatalf("export recommendations differ from /api/insights")
	}
	html := getReportHTML(t, s)
	for _, want := range []string{"email:1", "sku:1", fast["validation_step"].(string), "Candidate index for shop.users"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
	if strings.Contains(html, "secret-value-123") || strings.Contains(html, "sku-secret") {
		t.Fatalf("HTML report leaked a query literal")
	}
}

func TestEmptyURIDoesNotDialForIndexInventory(t *testing.T) {
	view := listNamespaceIndexes(context.Background(), &config.AppConfig{}, []string{"shop.users"})
	if view.Status != "unknown" || view.Reason != "connection_not_configured" || view.NamespacesFetched != 0 {
		t.Fatalf("empty URI must not look like a fetched empty index list: %+v", view)
	}
	if len(view.Namespaces) != 1 || view.Namespaces[0].Indexes != nil {
		t.Fatalf("unconfigured namespace must stay unknown: %+v", view.Namespaces)
	}
}
