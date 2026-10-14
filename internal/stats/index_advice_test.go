package stats

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDeriveCandidateOrder(t *testing.T) {
	t.Run("equality then range ignores filter document order", func(t *testing.T) {
		p := PredicateProfile{
			EqualityFields:      []string{"status"},
			RangeFields:         []string{"qty"},
			filterDocumentOrder: []string{"qty", "status"},
		}
		plan := deriveCandidate(p, nil)
		if !plan.Determined || formatKeys(plan.Keys) != "status:1,qty:1" {
			t.Fatalf("expected status then qty, got determined=%v keys=%s reason=%s", plan.Determined, formatKeys(plan.Keys), plan.Reason)
		}
	})

	t.Run("multiple equality fields are not ordered from the input slice", func(t *testing.T) {
		p := PredicateProfile{
			EqualityFields:      []string{"zzz", "aaa"},
			filterDocumentOrder: []string{"zzz", "aaa"},
		}
		plan := deriveCandidate(p, nil)
		if plan.Determined || len(plan.Keys) != 0 || plan.Reason != "equality_order_undetermined" {
			t.Fatalf("expected undetermined equality order, got %+v", plan)
		}
	})

	t.Run("selectivity orders equality fields", func(t *testing.T) {
		two, hundred := int64(2), int64(100)
		p := PredicateProfile{EqualityFields: []string{"zzz", "aaa"}}
		plan := deriveCandidate(p, []SelectivityEvidence{
			{Field: "aaa", DistinctCount: &hundred, Source: "measured"},
			{Field: "zzz", DistinctCount: &two, Source: "measured"},
		})
		if !plan.Determined || formatKeys(plan.Keys) != "zzz:1,aaa:1" || plan.SelectivitySource != "measured" {
			t.Fatalf("expected zzz then aaa from distinct counts, got %+v %s", plan, formatKeys(plan.Keys))
		}
	})

	t.Run("tied or partial selectivity does not invent an order", func(t *testing.T) {
		two := int64(2)
		p := PredicateProfile{EqualityFields: []string{"a", "b"}}
		plan := deriveCandidate(p, []SelectivityEvidence{{Field: "a", DistinctCount: &two, Source: "measured"}})
		if plan.Determined {
			t.Fatalf("missing selectivity must not determine order: %+v", plan)
		}
		same := int64(2)
		plan = deriveCandidate(p, []SelectivityEvidence{
			{Field: "a", DistinctCount: &same, Source: "measured"},
			{Field: "b", DistinctCount: &same, Source: "measured"},
		})
		if plan.Determined {
			t.Fatalf("tied selectivity must not determine order: %+v", plan)
		}
	})

	t.Run("sort direction and order are preserved", func(t *testing.T) {
		p := PredicateProfile{
			EqualityFields: []string{"status"},
			SortKeys: []IndexKeySpec{
				{Field: "created", Direction: -1, Kind: "desc"},
				{Field: "sku", Direction: 1, Kind: "asc"},
			},
			SortOrderKnown: true,
		}
		plan := deriveCandidate(p, nil)
		if !plan.Determined || formatKeys(plan.Keys) != "status:1,created:-1,sku:1" {
			t.Fatalf("expected equality then sort order, got %s", formatKeys(plan.Keys))
		}
	})

	t.Run("unordered multi-key sort is not a candidate", func(t *testing.T) {
		p := ProfileQuery(&ExecutableQuery{Pipeline: []interface{}{
			map[string]interface{}{"$match": map[string]interface{}{"status": "open"}},
			map[string]interface{}{"$sort": map[string]interface{}{"created": -1, "sku": 1}},
		}})
		plan := deriveCandidate(p, nil)
		if plan.Determined || plan.Reason != "unsupported_pattern" {
			t.Fatalf("expected undefined sort order to block a candidate, got %+v profile=%+v", plan, p)
		}
	})

	t.Run("single sort direction is kept", func(t *testing.T) {
		p := ProfileQuery(&ExecutableQuery{Pipeline: []interface{}{
			map[string]interface{}{"$sort": map[string]interface{}{"created": float64(-1)}},
		}})
		if !p.SortOrderKnown || len(p.SortKeys) != 1 || p.SortKeys[0].Direction != -1 {
			t.Fatalf("expected one descending sort key, got %+v", p)
		}
		plan := deriveCandidate(p, nil)
		if formatKeys(plan.Keys) != "created:-1" {
			t.Fatalf("expected created:-1, got %s", formatKeys(plan.Keys))
		}
	})

	t.Run("profile json omits literals", func(t *testing.T) {
		p := ProfileQuery(&ExecutableQuery{Filter: map[string]interface{}{"email": "secret-value", "qty": map[string]interface{}{"$gt": 5}}})
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "secret-value") || strings.Contains(string(b), "filterDocumentOrder") {
			t.Fatalf("profile leaked a literal or document order: %s", b)
		}
		plan := deriveCandidate(p, nil)
		if formatKeys(plan.Keys) != "email:1,qty:1" {
			t.Fatalf("expected email then qty, got %s", formatKeys(plan.Keys))
		}
	})
}

func TestIndexCoverageGates(t *testing.T) {
	pred := PredicateProfile{EqualityFields: []string{"status"}, RangeFields: []string{"qty"}, equalityValue: map[string]string{"status": "s:open"}}
	plan := deriveCandidate(pred, nil)
	if formatKeys(plan.Keys) != "status:1,qty:1" {
		t.Fatalf("setup keys %s", formatKeys(plan.Keys))
	}
	btree := func(name string, keys ...IndexKeySpec) DeployedIndex {
		return DeployedIndex{Name: name, Keys: keys, Type: "btree"}
	}
	asc := func(f string) IndexKeySpec { return IndexKeySpec{Field: f, Direction: 1, Kind: "asc"} }
	desc := func(f string) IndexKeySpec { return IndexKeySpec{Field: f, Direction: -1, Kind: "desc"} }

	cases := []struct {
		name     string
		idx      DeployedIndex
		plan     candidatePlan
		pred     PredicateProfile
		suitable bool
		reason   string
	}{
		{"prefix", btree("status_1_qty_1", asc("status"), asc("qty")), plan, pred, true, "suitable"},
		{"longer prefix", btree("status_1_qty_1_extra_1", asc("status"), asc("qty"), asc("extra")), plan, pred, true, "suitable"},
		{"reverse directions", btree("status_-1_qty_-1", desc("status"), desc("qty")), plan, pred, true, "suitable"},
		{"shared field only", btree("status_1_other_1", asc("status"), asc("other")), plan, pred, false, "shared_field_only"},
		{"sort before equality", btree("qty_1_status_1", asc("qty"), asc("status")), plan, pred, false, "shared_field_only"},
		{"hidden", DeployedIndex{Name: "hid", Keys: []IndexKeySpec{asc("status"), asc("qty")}, Type: "btree", Hidden: true}, plan, pred, false, "hidden"},
		{"collation", DeployedIndex{Name: "col", Keys: []IndexKeySpec{asc("status"), asc("qty")}, Type: "btree", Collation: "en"}, plan, pred, false, "collation"},
		{"partial not proven", DeployedIndex{Name: "part", Keys: []IndexKeySpec{asc("status"), asc("qty")}, Type: "btree", Partial: true, PartialPredicates: []PartialPredicate{{Field: "region", Op: "eq", Value: "s:us"}}}, plan, pred, false, "partial_not_proven"},
		{"partial proven", DeployedIndex{Name: "part_ok", Keys: []IndexKeySpec{asc("status"), asc("qty")}, Type: "btree", Partial: true, PartialPredicates: []PartialPredicate{{Field: "status", Op: "eq", Value: "s:open"}}}, plan, pred, true, "suitable"},
		{"text", DeployedIndex{Name: "txt", Keys: []IndexKeySpec{{Field: "status", Kind: "text"}}, Type: "text"}, plan, pred, false, "unsupported_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := evaluateIndex(tc.idx, tc.plan, tc.pred)
			if ok != tc.suitable || reason != tc.reason {
				t.Fatalf("got suitable=%v reason=%s", ok, reason)
			}
		})
	}

	t.Run("hashed equality is coverage and hashed range is not", func(t *testing.T) {
		eq := deriveCandidate(PredicateProfile{EqualityFields: []string{"email"}}, nil)
		hashed := DeployedIndex{Name: "email_hashed", Type: "hashed", Keys: []IndexKeySpec{{Field: "email", Kind: "hashed"}}}
		if ok, reason := evaluateIndex(hashed, eq, PredicateProfile{}); !ok || reason != "suitable" {
			t.Fatalf("hashed equality: suitable=%v reason=%s", ok, reason)
		}
		if ok, _ := evaluateIndex(hashed, plan, pred); ok {
			t.Fatalf("hashed index must not cover a range candidate")
		}
	})

	t.Run("multikey blocks a sort and unknown multikey does not", func(t *testing.T) {
		sortPlan := deriveCandidate(PredicateProfile{
			EqualityFields: []string{"status"},
			SortKeys:       []IndexKeySpec{{Field: "created", Direction: -1, Kind: "desc"}},
			SortOrderKnown: true,
		}, nil)
		yes := true
		multi := DeployedIndex{Name: "m", Type: "btree", Multikey: &yes, Keys: []IndexKeySpec{asc("status"), desc("created")}}
		if ok, reason := evaluateIndex(multi, sortPlan, PredicateProfile{}); ok || reason != "multikey_sort" {
			t.Fatalf("multikey sort: suitable=%v reason=%s", ok, reason)
		}
		unknown := DeployedIndex{Name: "u", Type: "btree", Keys: []IndexKeySpec{asc("status"), desc("created")}}
		if ok, reason := evaluateIndex(unknown, sortPlan, PredicateProfile{}); !ok || reason != "suitable" {
			t.Fatalf("unknown multikey must not reject a matching sort prefix: suitable=%v reason=%s", ok, reason)
		}
	})

	t.Run("equality permutation is coverage", func(t *testing.T) {
		two, hundred := int64(2), int64(100)
		plan := deriveCandidate(PredicateProfile{EqualityFields: []string{"sku", "status"}}, []SelectivityEvidence{
			{Field: "sku", DistinctCount: &two, Source: "measured"},
			{Field: "status", DistinctCount: &hundred, Source: "measured"},
		})
		idx := btree("status_1_sku_1", asc("status"), asc("sku"))
		if ok, reason := evaluateIndex(idx, plan, PredicateProfile{}); !ok || reason != "suitable" {
			t.Fatalf("permutation: suitable=%v reason=%s keys=%s", ok, reason, formatKeys(plan.Keys))
		}
	})
}

func TestCollectIndexInventoryFailures(t *testing.T) {
	if got := ClassifyIndexListFailure(errors.New("not authorized on shop.orders")); got != "permission_denied" {
		t.Fatalf("reason %s", got)
	}
	calls := map[string]int{}
	view := collectIndexInventory(context.Background(), []string{"shop.orders", "shop.orders", "shop.audit", "shop.extra"}, func(_ context.Context, ns string) NamespaceIndexInventory {
		calls[ns]++
		if ns == "shop.orders" {
			return NamespaceIndexInventory{Status: "unknown", Reason: "permission_denied", Indexes: []DeployedIndex{{Name: "should_be_dropped"}}}
		}
		if ns == "shop.audit" {
			return NamespaceIndexInventory{Status: "fetched"}
		}
		return NamespaceIndexInventory{Status: "fetched", Indexes: []DeployedIndex{{Name: "_id_"}}}
	}, 2)
	if calls["shop.orders"] != 1 || calls["shop.audit"] != 1 || calls["shop.extra"] != 0 {
		t.Fatalf("list calls = %+v", calls)
	}
	if view.Status != "partial" || view.NamespacesRequested != 3 || view.NamespacesFetched != 1 {
		t.Fatalf("view %+v", view)
	}
	orders := view.Namespaces[0]
	if orders.Status != "unknown" || orders.Reason != "permission_denied" || orders.Indexes != nil {
		t.Fatalf("permission failure must be unknown with nil indexes, got %+v", orders)
	}
	if view.Namespaces[1].Status != "fetched" || view.Namespaces[1].Indexes == nil || len(view.Namespaces[1].Indexes) != 0 {
		t.Fatalf("successful empty list must be a non-nil empty slice, got %+v", view.Namespaces[1])
	}
	if view.Namespaces[2].Reason != "inventory_namespace_budget" || view.Namespaces[2].Status != "unknown" {
		t.Fatalf("budget %+v", view.Namespaces[2])
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := 0
	cancelled := CollectIndexInventory(ctx, []string{"shop.orders"}, func(context.Context, string) NamespaceIndexInventory {
		called++
		return NamespaceIndexInventory{Status: "fetched"}
	})
	if called != 0 || cancelled.Namespaces[0].Reason != "context_canceled" || cancelled.Status != "unknown" {
		t.Fatalf("cancelled inventory called=%d %+v", called, cancelled)
	}
}

func TestIndexAdviceDedupConfidenceAndFastShapes(t *testing.T) {
	fast := coverageShape("fast", "shop.orders", "latency_tier", true, emailProfile(), poorFindDiag(), poorFindDiag())
	other := coverageShape("other", "shop.orders", "impact_rank", false, emailProfile(), poorFindDiag(), poorFindDiag())
	rep := adviceReport(fast, other)
	rep.IndexInventory = fetchedInventory("shop.orders", DeployedIndex{Name: "_id_", Type: "btree", Keys: []IndexKeySpec{{Field: "_id", Direction: 1, Kind: "asc"}}})
	rep.Impact = readyImpact(
		WorkloadImpact{ShapeID: "fast", Namespace: "shop.orders", Operation: "find", OperationCount: 1000, TotalOperationMs: 50, ShareOfObservedTime: 0.2},
		WorkloadImpact{ShapeID: "other", Namespace: "shop.orders", Operation: "updateOne", OperationCount: 4, TotalOperationMs: 10, ShareOfObservedTime: 0.05},
	)
	ApplyExplainFindings(rep)

	var advice []InsightRecommendation
	for _, r := range rep.Recommendations {
		if r.Source == "index_advice" {
			advice = append(advice, r)
		}
	}
	if len(advice) != 1 {
		t.Fatalf("expected one deduped candidate, got %+v", rep.Recommendations)
	}
	rec := advice[0]
	if rec.AdviceKind != "candidate" || strings.Join(rec.CandidateKeys, ",") != "email:1" {
		t.Fatalf("candidate %+v", rec)
	}
	if !containsAll(rec.ShapeIDs, "fast", "other") {
		t.Fatalf("shape ids %+v", rec.ShapeIDs)
	}
	if rec.Confidence != "high" || rec.Namespace != "shop.orders" || rec.ValidationStep == "" || rec.Rationale == "" {
		t.Fatalf("expected high-confidence advice with rationale and validation, got %+v", rec)
	}
	if !strings.Contains(rec.WorkloadImpact, "fast") || !strings.Contains(rec.WorkloadImpact, "of observed operation time") {
		t.Fatalf("impact %s", rec.WorkloadImpact)
	}
	if !strings.Contains(rec.Tradeoffs, "1000 reads") || !strings.Contains(rec.Tradeoffs, "4 writes") {
		t.Fatalf("write mix %s", rec.Tradeoffs)
	}
	if !strings.Contains(rec.Tradeoffs, "does not estimate") || strings.Contains(rec.Details, "missing index") || strings.Contains(rec.Details, "% faster") {
		t.Fatalf("advice invented a savings claim or a missing index: %s", rec.Details)
	}
	for _, s := range rep.ExplainCoverage.Shapes {
		if s.ShapeID == "fast" && (s.SelectedBy != "latency_tier" || !s.BelowSlowThreshold) {
			t.Fatalf("fast shape selection changed: %+v", s)
		}
	}
	if len(rep.SlowQueries) != 0 {
		t.Fatalf("fast-shape advice must not require a severity row")
	}
}

func TestEquivalentIndexSuppressesCandidate(t *testing.T) {
	pred := &PredicateProfile{EqualityFields: []string{"status"}, RangeFields: []string{"qty"}, equalityValue: map[string]string{"status": "s:open"}, Source: "captured_query"}
	shape := coverageShape("a", "shop.orders", "severity", false, pred, lowSelectivityDiag("status_1_qty_1"), lowSelectivityDiag("status_1_qty_1"))
	rep := adviceReport(shape)
	rep.IndexInventory = fetchedInventory("shop.orders", DeployedIndex{
		Name: "status_1_qty_1", Type: "btree",
		Keys: []IndexKeySpec{{Field: "status", Direction: 1, Kind: "asc"}, {Field: "qty", Direction: 1, Kind: "asc"}},
	})
	ApplyExplainFindings(rep)
	rec := adviceRec(t, rep)
	if rec.AdviceKind != "existing_index" || len(rec.CandidateKeys) != 0 || !strings.Contains(rec.ExistingIndex, "status_1_qty_1") {
		t.Fatalf("expected the deployed index to suppress a new candidate, got %+v", rec)
	}
	if strings.Contains(rec.Details, "Candidate keys") {
		t.Fatalf("suppressed advice still proposed keys: %s", rec.Details)
	}
}

func TestUnknownInventoryIsNotMissingIndexes(t *testing.T) {
	shape := coverageShape("a", "shop.orders", "severity", false, emailProfile(), poorFindDiag(), poorFindDiag())
	rep := adviceReport(shape)
	inv := UnknownIndexInventory([]string{"shop.orders"}, "permission_denied")
	rep.IndexInventory = &inv
	ApplyExplainFindings(rep)
	rec := adviceRec(t, rep)
	if rec.Confidence != "low" || rec.InventoryStatus != "unknown" {
		t.Fatalf("confidence/status %+v", rec)
	}
	blob := rec.Details + " " + strings.Join(rec.Limitations, " ")
	if !strings.Contains(blob, "permission_denied") || !strings.Contains(blob, "not evidence") {
		t.Fatalf("expected an unknown-inventory explanation, got %s", blob)
	}
	if strings.Contains(blob, "missing") || strings.Contains(blob, "no indexes") {
		t.Fatalf("unknown inventory was treated as no indexes: %s", blob)
	}
}

func TestAdviceConfidenceAndQualifiers(t *testing.T) {
	t.Run("one sample is not high", func(t *testing.T) {
		shape := coverageShape("a", "shop.orders", "latency_tier", true, emailProfile(), poorFindDiag())
		rep := adviceReport(shape)
		rep.IndexInventory = fetchedInventory("shop.orders")
		ApplyExplainFindings(rep)
		if got := adviceRec(t, rep).Confidence; got == "high" {
			t.Fatalf("one sample must not be high confidence")
		}
	})
	t.Run("shared field is not coverage", func(t *testing.T) {
		pred := &PredicateProfile{EqualityFields: []string{"status"}, RangeFields: []string{"qty"}, Source: "captured_query"}
		shape := coverageShape("a", "shop.orders", "severity", false, pred, poorFindDiag(), poorFindDiag())
		rep := adviceReport(shape)
		rep.IndexInventory = fetchedInventory("shop.orders", DeployedIndex{
			Name: "status_1_other_1", Type: "btree",
			Keys: []IndexKeySpec{{Field: "status", Direction: 1, Kind: "asc"}, {Field: "other", Direction: 1, Kind: "asc"}},
		})
		ApplyExplainFindings(rep)
		rec := adviceRec(t, rep)
		if rec.AdviceKind != "candidate" || !strings.Contains(strings.Join(rec.Limitations, " "), "shared field") {
			t.Fatalf("expected a candidate that rejects shared-field coverage, got %+v", rec)
		}
	})
	t.Run("sharded and lookup evidence is qualified", func(t *testing.T) {
		d := poorFindDiag()
		d.Metrics.Layout = "sharded_plan"
		d.Metrics.Shards = 2
		d.Metrics.Notes = []string{"lookup_work_not_scored"}
		pred := emailProfile()
		pred.HasLookup = true
		shape := coverageShape("a", "shop.orders", "latency_tier", true, pred, d, poorFindDiag())
		shape.Samples[1].Diagnostics.Metrics.Notes = []string{"lookup_work_not_scored"}
		rep := adviceReport(shape)
		rep.IndexInventory = fetchedInventory("shop.orders")
		ApplyExplainFindings(rep)
		rec := adviceRec(t, rep)
		blob := rec.Details + strings.Join(rec.Limitations, " ")
		if rec.Confidence == "high" || !strings.Contains(blob, "$lookup") || !strings.Contains(blob, "not validated") {
			t.Fatalf("expected qualified lookup and topology advice, got confidence=%s %s", rec.Confidence, blob)
		}
	})
	t.Run("planner-only evidence asks for execution stats", func(t *testing.T) {
		d := poorFindDiag()
		d.ExecutionStatsAvailable = false
		d.Metrics = &ExplainMetrics{Layout: "planner_only"}
		d.DocsExamined, d.NReturned = 0, 0
		DescribeExplainSample(d, "find")
		shape := coverageShape("a", "shop.orders", "severity", false, emailProfile(), d, d)
		shape.Evidence = "planner_only"
		rep := adviceReport(shape)
		rep.IndexInventory = fetchedInventory("shop.orders")
		ApplyExplainFindings(rep)
		rec := adviceRec(t, rep)
		if rec.AdviceKind != "measure" || len(rec.CandidateKeys) != 0 || !strings.Contains(rec.Details, "executionStats") {
			t.Fatalf("expected a measurement step, got %+v", rec)
		}
	})
}

func TestSortAndSpillStayObservationsWhenScoreIs100(t *testing.T) {
	d := &ExplainDiagnostics{
		ExecutionStatsAvailable: true,
		IndexScanDetected:       true,
		BlockingSortDetected:    true,
		UsedDisk:                true,
		Spills:                  2,
		DocsExamined:            10,
		KeysExamined:            10,
		NReturned:               10,
		Metrics: &ExplainMetrics{
			Layout:       "plan",
			DocsExamined: &ExplainCounter{Value: 10, Source: "executionStats.totalDocsExamined"},
			KeysExamined: &ExplainCounter{Value: 10, Source: "executionStats.totalKeysExamined"},
			Returned:     &ExplainCounter{Value: 10, Source: "executionStats.nReturned"},
		},
	}
	DescribeExplainSample(d, "find")
	pred := &PredicateProfile{
		EqualityFields: []string{"status"},
		SortKeys:       []IndexKeySpec{{Field: "created", Direction: -1, Kind: "desc"}},
		SortOrderKnown: true,
		Source:         "captured_query",
	}
	shape := coverageShape("sorted", "shop.orders", "latency_tier", true, pred, d, cloneDiag(d))
	rep := adviceReport(shape)
	rep.IndexInventory = fetchedInventory("shop.orders")
	ApplyExplainFindings(rep)
	eff := rep.ExplainCoverage.Shapes[0].Efficiency
	if eff == nil || eff.Version != EfficiencyVersion || eff.Score == nil || *eff.Score != 100 || !hasString(eff.Observations, "blocking_sort") || !hasString(eff.Observations, "spill") {
		t.Fatalf("scan efficiency must stay 100 with sort and spill observations, got %+v", eff)
	}
	rec := adviceRec(t, rep)
	if !strings.Contains(rec.Details, "reported under Execution overhead") || !strings.Contains(rec.Details, "blocking_sort") || !strings.Contains(rec.Details, "created:-1") {
		t.Fatalf("advice dropped the sort observation or the ordered key: %s", rec.Details)
	}
}

func TestWriteMixRequiresCompleteCoverage(t *testing.T) {
	shape := coverageShape("a", "shop.orders", "severity", false, emailProfile(), poorFindDiag(), poorFindDiag())
	rep := adviceReport(shape)
	rep.IndexInventory = fetchedInventory("shop.orders")
	rep.Impact = ImpactView{
		Status: "ready",
		Coverage: ImpactCoverage{
			GroupCoverageComplete: false,
			OverflowOperations:    9,
		},
		Groups: []WorkloadImpact{{ShapeID: "a", Namespace: "shop.orders", Operation: "updateMany", OperationCount: 40}},
	}
	ApplyExplainFindings(rep)
	rec := adviceRec(t, rep)
	if strings.Contains(rec.Tradeoffs, "recorded") || strings.Contains(rec.Tradeoffs, "40") {
		t.Fatalf("incomplete coverage must not cite a write mix: %s", rec.Tradeoffs)
	}
	if !strings.Contains(rec.Tradeoffs, "was not used") {
		t.Fatalf("expected the coverage disclosure, got %s", rec.Tradeoffs)
	}
}

func adviceReport(shapes ...ExplainCoverageShape) *InsightsReport {
	rep := &InsightsReport{ExplainCoverage: NewExplainCoverage("ready", "")}
	rep.ExplainCoverage.Shapes = shapes
	return rep
}

func coverageShape(id, namespace, selectedBy string, below bool, pred *PredicateProfile, diags ...*ExplainDiagnostics) ExplainCoverageShape {
	samples := make([]ExplainSample, len(diags))
	roles := []string{"median", "p95"}
	for i, d := range diags {
		samples[i] = ExplainSample{Role: roles[i%2], Status: "explained", Diagnostics: d}
	}
	return ExplainCoverageShape{
		ShapeID: id, Namespace: namespace, Operation: "find", ShapeSummary: "find",
		SelectedBy: selectedBy, BelowSlowThreshold: below, Status: "explained", Evidence: "execution_stats",
		Predicates: pred, Samples: samples, OperationCount: 10,
	}
}

func emailProfile() *PredicateProfile {
	return &PredicateProfile{EqualityFields: []string{"email"}, Source: "captured_query", equalityValue: map[string]string{"email": "s:a"}}
}

func lowSelectivityDiag(index string) *ExplainDiagnostics {
	d := &ExplainDiagnostics{
		ExecutionStatsAvailable: true,
		IndexScanDetected:       true,
		IndexesUsed:             []string{index},
		DocsExamined:            30000,
		KeysExamined:            30000,
		NReturned:               10,
		WinningPlanSummary:      "IXSCAN",
		Metrics: &ExplainMetrics{
			Layout:       "plan",
			DocsExamined: &ExplainCounter{Value: 30000, Source: "executionStats.totalDocsExamined"},
			KeysExamined: &ExplainCounter{Value: 30000, Source: "executionStats.totalKeysExamined"},
			Returned:     &ExplainCounter{Value: 10, Source: "executionStats.nReturned"},
		},
	}
	DescribeExplainSample(d, "find")
	return d
}

func poorFindDiag() *ExplainDiagnostics {
	d := &ExplainDiagnostics{
		ExecutionStatsAvailable: true,
		CollectionScanDetected:  true,
		DocsExamined:            50000,
		NReturned:               2,
		WinningPlanSummary:      "COLLSCAN",
		Metrics: &ExplainMetrics{
			Layout:       "plan",
			DocsExamined: &ExplainCounter{Value: 50000, Source: "executionStats.totalDocsExamined"},
			KeysExamined: &ExplainCounter{Value: 0, Source: "executionStats.totalKeysExamined"},
			Returned:     &ExplainCounter{Value: 2, Source: "executionStats.nReturned"},
		},
	}
	DescribeExplainSample(d, "find")
	return d
}

func cloneDiag(d *ExplainDiagnostics) *ExplainDiagnostics {
	cp := *d
	if d.Metrics != nil {
		m := *d.Metrics
		cp.Metrics = &m
	}
	return &cp
}

func fetchedInventory(namespace string, indexes ...DeployedIndex) *IndexInventoryView {
	if indexes == nil {
		indexes = []DeployedIndex{}
	}
	return &IndexInventoryView{
		Status: "ready", Source: indexListSource,
		Namespaces: []NamespaceIndexInventory{{
			Namespace: namespace, Status: "fetched", Source: indexListSource, Indexes: indexes,
		}},
	}
}

func readyImpact(groups ...WorkloadImpact) ImpactView {
	return ImpactView{Status: "ready", Coverage: ImpactCoverage{GroupCoverageComplete: true}, Groups: groups}
}

func adviceRec(t *testing.T, rep *InsightsReport) InsightRecommendation {
	t.Helper()
	for _, r := range rep.Recommendations {
		if r.Source == "index_advice" {
			return r
		}
	}
	t.Fatalf("no index advice in %+v", rep.Recommendations)
	return InsightRecommendation{}
}

func containsAll(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
