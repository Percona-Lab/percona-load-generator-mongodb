package workloadrec

import (
	"fmt"
	"sort"
	"strings"
)

// Data fingerprint modes (workload_data_fingerprint).
const (
	FingerprintCounts = "counts"
	FingerprintDBHash = "dbhash"
)

// DataFingerprint describes the starting data of a run: per collection,
// the document count and the index list, plus the dbHash hash in dbhash
// mode. It holds no document values.
type DataFingerprint struct {
	Mode        string                  `json:"mode"`
	Collections []CollectionFingerprint `json:"collections"`
}

// CollectionFingerprint is one namespace of a DataFingerprint.
type CollectionFingerprint struct {
	NS      string             `json:"ns"`
	Count   int64              `json:"count"`
	Indexes []IndexFingerprint `json:"indexes"`
	// DBHash is the collection hash dbHash reported; DBHashError is why it
	// could not run (an error code name, never the message).
	DBHash      string `json:"dbhash,omitempty"`
	DBHashError string `json:"dbhash_error,omitempty"`
}

// IndexFingerprint is one index: its name and a canonical spec of its keys
// and options.
type IndexFingerprint struct {
	Name string `json:"name"`
	Spec string `json:"spec"`
}

// FingerprintCheck is the result of comparing a target's starting data with
// a recorded fingerprint. Differences and Notes name namespaces, counts, and
// index names only.
type FingerprintCheck struct {
	Mode        string   `json:"mode"`
	Match       bool     `json:"match"`
	Differences []string `json:"differences,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// CompareDataFingerprints compares a target's fingerprint (got) with the
// recorded one (want). A dbHash that either side could not compute is a
// note, not a difference.
func CompareDataFingerprints(want, got *DataFingerprint) FingerprintCheck {
	out := FingerprintCheck{Mode: want.Mode}
	gotByNS := map[string]CollectionFingerprint{}
	if got != nil {
		for _, c := range got.Collections {
			gotByNS[c.NS] = c
		}
	}
	for _, w := range want.Collections {
		g, ok := gotByNS[w.NS]
		if !ok {
			out.Differences = append(out.Differences, fmt.Sprintf("%s: not checked on this target", w.NS))
			continue
		}
		if w.Count != g.Count {
			out.Differences = append(out.Differences, fmt.Sprintf("%s: %d documents recorded, %d on this target", w.NS, w.Count, g.Count))
		}
		if d := indexDifference(w.Indexes, g.Indexes); d != "" {
			out.Differences = append(out.Differences, fmt.Sprintf("%s: indexes differ (%s)", w.NS, d))
		}
		if want.Mode != FingerprintDBHash {
			continue
		}
		switch {
		case w.DBHashError != "":
			out.Notes = append(out.Notes, fmt.Sprintf("%s: dbHash not compared: it could not run when recording (%s)", w.NS, w.DBHashError))
		case g.DBHashError != "":
			out.Notes = append(out.Notes, fmt.Sprintf("%s: dbHash not compared: it could not run on this target (%s)", w.NS, g.DBHashError))
		case w.DBHash != g.DBHash:
			out.Differences = append(out.Differences, fmt.Sprintf("%s: dbHash differs (the stored documents are not byte-identical)", w.NS))
		}
	}
	out.Match = len(out.Differences) == 0
	return out
}

func indexDifference(want, got []IndexFingerprint) string {
	w, g := map[string]string{}, map[string]string{}
	for _, i := range want {
		w[i.Name] = i.Spec
	}
	for _, i := range got {
		g[i.Name] = i.Spec
	}
	var onlyW, onlyG, changed []string
	for n, s := range w {
		gs, ok := g[n]
		switch {
		case !ok:
			onlyW = append(onlyW, n)
		case gs != s:
			changed = append(changed, n)
		}
	}
	for n := range g {
		if _, ok := w[n]; !ok {
			onlyG = append(onlyG, n)
		}
	}
	var parts []string
	for _, p := range []struct {
		label string
		names []string
	}{{"missing on this target", onlyW}, {"only on this target", onlyG}, {"different keys or options", changed}} {
		if len(p.names) > 0 {
			sort.Strings(p.names)
			parts = append(parts, p.label+": "+strings.Join(p.names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}
