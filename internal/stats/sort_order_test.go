package stats

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func orderedSortQuery() *ExecutableQuery {
	return &ExecutableQuery{Database: "db", Pipeline: []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{"status": "open"}},
		map[string]interface{}{"$sort": bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}},
		map[string]interface{}{"$setWindowFields": map[string]interface{}{
			"sortBy": bson.D{{Key: "t", Value: int32(1)}, {Key: "s", Value: int32(-1)}},
			"output": map[string]interface{}{"r": map[string]interface{}{"$rank": map[string]interface{}{}}},
		}},
	}}
}

func TestDescriptorKeepsDeclaredSortOrder(t *testing.T) {
	d, _ := DescribeExecutableQuery("aggregate", orderedSortQuery())
	for _, want := range []string{"$sort:{b:-1,a:1}", "sortBy:{t:1,s:-1}", "status:<string>"} {
		if !strings.Contains(d, want) {
			t.Fatalf("descriptor %q must contain %q", d, want)
		}
	}
}

func TestOrderedSortIsNotFlaggedUnordered(t *testing.T) {
	if HasUnorderedSortSpec(orderedSortQuery().Pipeline) {
		t.Fatal("an ordered bson.D sort must not be flagged")
	}
	unordered := []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": 1}}}
	if !HasUnorderedSortSpec(unordered) {
		t.Fatal("a multi-key map sort must still be flagged")
	}
}

// canonicalOrdered sorts map keys (their order is never sent deterministically)
// and keeps bson.D order, so equal results mean equal sort specifications.
func canonicalOrdered(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := sortedKeys(t)
		out := make(bson.D, 0, len(keys))
		for _, k := range keys {
			out = append(out, bson.E{Key: k, Value: canonicalOrdered(t[k])})
		}
		return out
	case bson.D:
		out := make(bson.D, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: canonicalOrdered(e.Value)}
		}
		return out
	case []interface{}:
		out := make(bson.A, len(t))
		for i, x := range t {
			out[i] = canonicalOrdered(x)
		}
		return out
	}
	return v
}

func canonicalBytes(t *testing.T, pipeline []interface{}) []byte {
	t.Helper()
	b, err := bson.Marshal(bson.D{{Key: "p", Value: canonicalOrdered(pipeline)}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCloneExecutableQueryCopiesSortSpec(t *testing.T) {
	q := orderedSortQuery()
	c := cloneExecutableQuery(q)
	before, cloned := canonicalBytes(t, q.Pipeline), canonicalBytes(t, c.Pipeline)
	if !bytes.Equal(before, cloned) {
		t.Fatalf("clone changed the command:\n%s\n%s", bson.Raw(before), bson.Raw(cloned))
	}
	c.Pipeline[1].(map[string]interface{})["$sort"].(bson.D)[0].Key = "z"
	if !bytes.Equal(before, canonicalBytes(t, q.Pipeline)) {
		t.Fatal("clone must not share sort specifications")
	}
}

func TestProfileQueryUsesOrderedMultiKeySort(t *testing.T) {
	p := ProfileQuery(orderedSortQuery())
	if !p.SortOrderKnown || len(p.SortKeys) != 2 || p.SortKeys[0].Field != "b" || p.SortKeys[0].Direction != -1 || p.SortKeys[1].Field != "a" || p.SortKeys[1].Direction != 1 {
		t.Fatalf("ordered sort keys = %+v known=%t", p.SortKeys, p.SortOrderKnown)
	}
	unordered := ProfileQuery(&ExecutableQuery{Pipeline: []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": 1}}}})
	if unordered.SortOrderKnown {
		t.Fatal("map sort order must stay unknown")
	}
}

func TestSI2BaselinesAreNotComparableWithSI3(t *testing.T) {
	if ShapeIdentityVersion != "si-3" {
		t.Fatalf("ShapeIdentityVersion = %s", ShapeIdentityVersion)
	}
	old, cur := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	old.ShapeIdentityVersion = "si-2"
	row := compareOne(t, shape(effSnap(old, 67, 2), wlSnap(old, 100, 5, 0.1)), shape(effSnap(cur, 100, 2), wlSnap(cur, 100, 5, 0.1)), old, cur)
	if row.Comparability != TrendIncompatible || row.Efficiency.State != TrendNotComparable || row.Latency.State != TrendNotComparable || row.Impact.State != TrendNotComparable {
		t.Fatalf("si-2 baselines must not be compared: %+v", row)
	}
	if len(row.Efficiency.Reasons) == 0 || !strings.Contains(row.Efficiency.Reasons[0], "baseline si-2, current si-3") {
		t.Fatalf("reason must name both identity versions: %v", row.Efficiency.Reasons)
	}
}
