package workloadrec

import (
	"strings"
	"testing"
)

func fp(mode string, cols ...CollectionFingerprint) *DataFingerprint {
	return &DataFingerprint{Mode: mode, Collections: cols}
}

func TestCompareDataFingerprints(t *testing.T) {
	idx := []IndexFingerprint{{Name: "_id_", Spec: `{"key":{"_id":1}}`}, {Name: "age_1", Spec: `{"key":{"age":1}}`}}
	users := CollectionFingerprint{NS: "app.users", Count: 300, Indexes: idx, DBHash: "aa"}
	orders := CollectionFingerprint{NS: "app.orders", Count: 10, Indexes: idx[:1], DBHash: "bb"}

	if c := CompareDataFingerprints(fp(FingerprintCounts, users, orders), fp(FingerprintCounts, orders, users)); !c.Match || len(c.Differences) != 0 {
		t.Fatalf("identical fingerprints: %+v", c)
	}

	changed := users
	changed.Count = 280
	changed.Indexes = []IndexFingerprint{idx[0], {Name: "age_1", Spec: `{"key":{"age":-1}}`}, {Name: "x_1", Spec: `{"key":{"x":1}}`}}
	c := CompareDataFingerprints(fp(FingerprintCounts, users, orders), fp(FingerprintCounts, changed))
	got := strings.Join(c.Differences, " | ")
	for _, want := range []string{"app.users: 300 documents recorded, 280 on this target", "only on this target: x_1", "different keys or options: age_1", "app.orders: not checked on this target"} {
		if !strings.Contains(got, want) {
			t.Fatalf("differences %q miss %q", got, want)
		}
	}
	if c.Match || strings.Contains(got, `"age"`) {
		t.Fatalf("differences must name indexes, not specs: %+v", c)
	}

	// Counts mode ignores dbHash; dbhash mode compares it, and a dbHash
	// that could not run is a note.
	other := users
	other.DBHash = "zz"
	if c := CompareDataFingerprints(fp(FingerprintCounts, users), fp(FingerprintCounts, other)); !c.Match {
		t.Fatalf("counts mode compared dbHash: %+v", c)
	}
	if c := CompareDataFingerprints(fp(FingerprintDBHash, users), fp(FingerprintDBHash, other)); c.Match || !strings.Contains(c.Differences[0], "dbHash differs") {
		t.Fatalf("dbhash mode: %+v", c)
	}
	denied := users
	denied.DBHash, denied.DBHashError = "", "Unauthorized"
	if c := CompareDataFingerprints(fp(FingerprintDBHash, users), fp(FingerprintDBHash, denied)); !c.Match || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "Unauthorized") {
		t.Fatalf("dbHash error must be a note: %+v", c)
	}
}
