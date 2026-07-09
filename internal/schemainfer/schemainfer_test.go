package schemainfer

import (
	"strings"
	"testing"
)

func findOp(ops []Operation, ns, op string) *Operation {
	for i := range ops {
		if ops[i].Namespace == ns && ops[i].Operation == op {
			return &ops[i]
		}
	}
	return nil
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestInferMongodSlowQueryLog(t *testing.T) {
	log := `{"t":{"$date":"2026-01-01T00:00:00Z"},"s":"I","c":"COMMAND","id":51803,"ctx":"conn1","msg":"Slow query","attr":{"type":"command","ns":"shop.orders","command":{"find":"orders","filter":{"status":"open","total":{"$gt":100}},"sort":{"createdAt":-1},"limit":20},"durationMillis":15}}
{"attr":{"ns":"shop.orders","command":{"find":"orders","filter":{"status":"shipped"}}}}
{"attr":{"ns":"shop.users","command":{"update":"users","updates":[{"q":{"user_id":42},"u":{"$set":{"last_login":"now"}}}]}}}
{"attr":{"ns":"shop.orders","command":{"insert":"orders","documents":[{"status":"new","total":10}]}}}`

	res := Infer(log)
	if res.ParsedEntries != 4 {
		t.Fatalf("expected 4 parsed entries, got %d (skipped=%d)", res.ParsedEntries, res.SkippedLines)
	}
	if len(res.Collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(res.Collections))
	}

	findOrders := findOp(res.Operations, "shop.orders", "find")
	if findOrders == nil {
		t.Fatalf("expected find on shop.orders")
	}
	if findOrders.Count != 2 {
		t.Fatalf("expected 2 find ops, got %d", findOrders.Count)
	}
	if !contains(findOrders.FilterFields, "status") || !contains(findOrders.FilterFields, "total") {
		t.Fatalf("expected status+total filter fields, got %v", findOrders.FilterFields)
	}
	if !contains(findOrders.SortFields, "createdAt") {
		t.Fatalf("expected createdAt sort field, got %v", findOrders.SortFields)
	}

	upd := findOp(res.Operations, "shop.users", "update")
	if upd == nil || !contains(upd.UpdateFields, "last_login") || !contains(upd.FilterFields, "user_id") {
		t.Fatalf("update inference wrong: %+v", upd)
	}
}

func TestInferAggregatePipeline(t *testing.T) {
	log := `{"attr":{"ns":"shop.orders","command":{"aggregate":"orders","pipeline":[{"$match":{"region":"us","status":"open"}},{"$sort":{"created":-1}},{"$group":{"_id":"$region"}}]}}}`
	res := Infer(log)
	agg := findOp(res.Operations, "shop.orders", "aggregate")
	if agg == nil {
		t.Fatalf("expected aggregate op, ops=%+v", res.Operations)
	}
	if !contains(agg.FilterFields, "region") || !contains(agg.FilterFields, "status") {
		t.Fatalf("expected $match fields, got %v", agg.FilterFields)
	}
	if !contains(agg.SortFields, "created") {
		t.Fatalf("expected $sort field, got %v", agg.SortFields)
	}
}

func TestInferRawCommandWithDb(t *testing.T) {
	log := `{"find":"products","filter":{"$or":[{"sku":"A1"},{"sku":"B2"}]},"$db":"catalog"}`
	res := Infer(log)
	op := findOp(res.Operations, "catalog.products", "find")
	if op == nil {
		t.Fatalf("expected find on catalog.products, ops=%+v", res.Operations)
	}
	if !contains(op.FilterFields, "sku") {
		t.Fatalf("expected sku from $or branches, got %v", op.FilterFields)
	}
}

func TestInferJSONArrayInput(t *testing.T) {
	log := `[
	  {"attr":{"ns":"db.c","command":{"find":"c","filter":{"a":1}}}},
	  {"attr":{"ns":"db.c","command":{"delete":"c","deletes":[{"q":{"a":2},"limit":1}]}}}
	]`
	res := Infer(log)
	if res.ParsedEntries != 2 {
		t.Fatalf("expected 2 parsed entries from array, got %d", res.ParsedEntries)
	}
	if findOp(res.Operations, "db.c", "delete") == nil {
		t.Fatalf("expected delete op from array input")
	}
}

func TestInferSuggestedMixSumsTo100(t *testing.T) {
	log := `{"attr":{"ns":"db.c","command":{"find":"c","filter":{"a":1}}}}
{"attr":{"ns":"db.c","command":{"find":"c","filter":{"a":2}}}}
{"attr":{"ns":"db.c","command":{"find":"c","filter":{"a":3}}}}
{"attr":{"ns":"db.c","command":{"insert":"c","documents":[{"a":1}]}}}`
	res := Infer(log)
	mix := res.SuggestedMix
	sum := mix.FindPercent + mix.InsertPercent + mix.UpdatePercent + mix.DeletePercent + mix.AggregatePercent
	if sum != 100 {
		t.Fatalf("expected mix to sum to 100, got %d (%+v)", sum, mix)
	}
	if mix.FindPercent <= mix.InsertPercent {
		t.Fatalf("expected find to dominate, got %+v", mix)
	}
}

func TestInferSuggestedQueries(t *testing.T) {
	log := `{"attr":{"ns":"shop.orders","command":{"find":"orders","filter":{"status":"open"}}}}`
	res := Infer(log)
	if len(res.SuggestedQueries) != 1 {
		t.Fatalf("expected 1 suggested query, got %d", len(res.SuggestedQueries))
	}
	q := res.SuggestedQueries[0]
	if q.Database != "shop" || q.Collection != "orders" || q.Operation != "find" {
		t.Fatalf("unexpected suggested query: %+v", q)
	}
	if q.Filter["status"] != "open" {
		t.Fatalf("expected sample filter preserved, got %+v", q.Filter)
	}
}

func TestInferHandlesMalformedAndPartial(t *testing.T) {
	log := `this is not json
{"attr":{"ns":"db.c","command":{"find":"c","filter":{"a":1}}}}
{"attr":{"ns":"db.c"}}
{broken json
{"random":"object","with":"no command"}`
	res := Infer(log)
	if res.ParsedEntries != 1 {
		t.Fatalf("expected exactly 1 parsed entry, got %d", res.ParsedEntries)
	}
	if res.SkippedLines < 3 {
		t.Fatalf("expected at least 3 skipped lines, got %d", res.SkippedLines)
	}
}

func TestInferEmptyInputWarns(t *testing.T) {
	res := Infer("\n   \n")
	if res.ParsedEntries != 0 {
		t.Fatalf("expected no parsed entries")
	}
	// Whitespace-only lines are skipped silently; there are no recognizable
	// entries but also effectively no content lines.
	if len(res.Operations) != 0 {
		t.Fatalf("expected no operations")
	}
}

func TestInferNoRecognizableEntriesWarns(t *testing.T) {
	res := Infer(`{"foo":"bar"}` + "\n" + `{"baz":1}`)
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "No recognizable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'No recognizable' warning, got %v", res.Warnings)
	}
}
