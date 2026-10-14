package stats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	indexListSource             = "listIndexes"
	indexInventoryMaxNamespaces = 32

	// scanWorkSignalNote explains a high scan efficiency score on a shape that
	// also sorted in memory or spilled. It does not change the score.
	scanWorkSignalNote = "Scan efficiency counts documents and index keys examined per result and filtered collection scans. This shape also sorted in memory (blocking_sort) or spilled to disk (spill); those are reported under Execution overhead, not in the score."

	indexValidationStep = "On a disposable copy, re-run explain with executionStats for this shape and compare the winning plan, docsExamined, keysExamined, and any SORT stage or spill count. Do not create or drop an index from this report."

	indexTradeoffBase = "An additional index is maintained on writes and uses storage. This report does not estimate bytes, selectivity, or a speedup percentage."
)

// IndexKeySpec is one key in an index or a candidate. Order in the containing
// slice is the key order.
type IndexKeySpec struct {
	Field     string `json:"field"`
	Direction int    `json:"direction,omitempty"`
	Kind      string `json:"kind"`
	Role      string `json:"role,omitempty"`
}

// PartialPredicate is a partial-filter constraint used only to decide whether
// a partial index was shown to include the query. It is not serialized.
type PartialPredicate struct {
	Field string
	Op    string
	Value string
}

// DeployedIndex is one index document returned by listIndexes. Key order is
// the order listIndexes returned.
type DeployedIndex struct {
	Name              string             `json:"name"`
	Keys              []IndexKeySpec     `json:"keys"`
	Type              string             `json:"type"`
	Unique            bool               `json:"unique,omitempty"`
	Sparse            bool               `json:"sparse,omitempty"`
	Hidden            bool               `json:"hidden,omitempty"`
	Partial           bool               `json:"partial,omitempty"`
	PartialSummary    string             `json:"partial_summary,omitempty"`
	Collation         string             `json:"collation,omitempty"`
	Multikey          *bool              `json:"multikey,omitempty"`
	PartialPredicates []PartialPredicate `json:"-"`
}

// NamespaceIndexInventory is the listIndexes result for one namespace.
// Status unknown means the definitions were not read. Indexes is nil in that
// case. An empty non-nil Indexes slice means the command succeeded and
// returned no indexes.
type NamespaceIndexInventory struct {
	Namespace string          `json:"namespace"`
	Status    string          `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	Source    string          `json:"source"`
	FetchedAt string          `json:"fetched_at,omitempty"`
	ElapsedMS int             `json:"elapsed_ms,omitempty"`
	Indexes   []DeployedIndex `json:"indexes,omitempty"`
}

// IndexInventoryView is the per-analysis index inventory. Each affected
// namespace is listed at most once.
type IndexInventoryView struct {
	Status              string                    `json:"status"`
	Reason              string                    `json:"reason,omitempty"`
	Source              string                    `json:"source"`
	FetchedAt           string                    `json:"fetched_at,omitempty"`
	NamespacesRequested int                       `json:"namespaces_requested"`
	NamespacesFetched   int                       `json:"namespaces_fetched"`
	Namespaces          []NamespaceIndexInventory `json:"namespaces,omitempty"`
	Limitations         []string                  `json:"limitations,omitempty"`
}

// PredicateProfile is a redacted description of one captured query. Equality
// and range fields are sets. SortKeys keeps the captured sort order. Literal
// values stay unexported.
type PredicateProfile struct {
	Source         string         `json:"source,omitempty"`
	EqualityFields []string       `json:"equality_fields,omitempty"`
	RangeFields    []string       `json:"range_fields,omitempty"`
	SortKeys       []IndexKeySpec `json:"sort_keys,omitempty"`
	SortOrderKnown bool           `json:"sort_order_known,omitempty"`
	Unsupported    []string       `json:"unsupported,omitempty"`
	HasLookup      bool           `json:"has_lookup,omitempty"`
	InPredicate    bool           `json:"in_predicate,omitempty"`

	equalityValue       map[string]string
	eqSet               map[string]struct{}
	rangeSet            map[string]struct{}
	filterDocumentOrder []string
}

// SelectivityEvidence is a measured per-field cardinality. A nil count or an
// empty source is not evidence and is ignored.
type SelectivityEvidence struct {
	Field         string
	DistinctCount *int64
	Source        string
}

type candidatePlan struct {
	Determined        bool
	Reason            string
	Keys              []IndexKeySpec
	EqualityFields    []string
	ResidualRanges    []string
	Unsupported       []string
	SelectivitySource string
}

var indexInventoryLimitations = []string{
	"Deployed indexes are read with listIndexes once per affected namespace during this analysis. Indexes declared only in the workload configuration are not treated as deployed.",
	"A listIndexes permission failure leaves that namespace unknown. Unknown is not evidence that the collection has no indexes, and this report does not recommend dropping an index.",
	"Query collation is not captured. An index with a non-simple collation is not treated as suitable.",
	"listIndexes does not establish multikey state unless the server document includes it. Unknown multikey status is not treated as proof that the index is not multikey.",
	"Sharded topologies, replica sets, and servers older than 8.0 were not validated for this inventory.",
}

// CanonicalPredicateValue maps a scalar to a comparable token. Non-scalars and
// non-integral floats are not comparable.
func CanonicalPredicateValue(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return "s:" + t, true
	case bool:
		if t {
			return "b:true", true
		}
		return "b:false", true
	case int:
		return fmt.Sprintf("i:%d", t), true
	case int32:
		return fmt.Sprintf("i:%d", t), true
	case int64:
		return fmt.Sprintf("i:%d", t), true
	case float64:
		if !math.IsInf(t, 0) && t == math.Trunc(t) {
			return fmt.Sprintf("i:%d", int64(t)), true
		}
		return "", false
	default:
		return "", false
	}
}

// ClassifyIndexListFailure maps a listIndexes error to an unknown-inventory
// reason. That reason is not an empty index list.
func ClassifyIndexListFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not authorized"), strings.Contains(msg, "unauthorized"):
		return "permission_denied"
	case strings.Contains(msg, "ns does not exist"), strings.Contains(msg, "namespace not found"), strings.Contains(msg, "namespace does not exist"):
		return "namespace_not_found"
	default:
		return "list_indexes_failed"
	}
}

func contextFailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "context_canceled"
}

// CollectIndexInventory lists each namespace once, in first-seen order, and
// stops at indexInventoryMaxNamespaces.
func CollectIndexInventory(ctx context.Context, namespaces []string, list func(context.Context, string) NamespaceIndexInventory) IndexInventoryView {
	return collectIndexInventory(ctx, namespaces, list, indexInventoryMaxNamespaces)
}

func collectIndexInventory(ctx context.Context, namespaces []string, list func(context.Context, string) NamespaceIndexInventory, maxNS int) IndexInventoryView {
	now := time.Now().UTC().Format(time.RFC3339)
	view := IndexInventoryView{
		Source:      indexListSource,
		FetchedAt:   now,
		Limitations: append([]string(nil), indexInventoryLimitations...),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	all := dedupeNamespaces(namespaces)
	view.NamespacesRequested = len(all)
	if len(all) == 0 {
		view.Status, view.Reason = "not_run", "no_explained_namespaces"
		return view
	}
	listed, omitted := all, []string(nil)
	if maxNS > 0 && len(all) > maxNS {
		omitted = all[maxNS:]
		listed = all[:maxNS]
	}
	for _, ns := range listed {
		if err := ctx.Err(); err != nil {
			view.Namespaces = append(view.Namespaces, unknownNamespace(ns, contextFailureReason(err), now))
			continue
		}
		inv := list(ctx, ns)
		inv.Namespace = ns
		if inv.Source == "" {
			inv.Source = indexListSource
		}
		if inv.FetchedAt == "" {
			inv.FetchedAt = now
		}
		if inv.Status != "fetched" {
			inv.Status = "unknown"
			if inv.Reason == "" {
				inv.Reason = "list_indexes_failed"
			}
			inv.Indexes = nil
		} else if inv.Indexes == nil {
			inv.Indexes = []DeployedIndex{}
		}
		if inv.Status == "fetched" {
			view.NamespacesFetched++
		}
		view.Namespaces = append(view.Namespaces, inv)
	}
	for _, ns := range omitted {
		view.Namespaces = append(view.Namespaces, unknownNamespace(ns, "inventory_namespace_budget", now))
	}
	switch {
	case view.NamespacesFetched == len(view.Namespaces) && len(view.Namespaces) > 0:
		view.Status = "ready"
	case view.NamespacesFetched == 0:
		view.Status = "unknown"
	default:
		view.Status = "partial"
	}
	return view
}

func unknownNamespace(ns, reason, at string) NamespaceIndexInventory {
	return NamespaceIndexInventory{Namespace: ns, Status: "unknown", Reason: reason, Source: indexListSource, FetchedAt: at}
}

func dedupeNamespaces(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, ns := range in {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// SplitNamespace splits a "database.collection" name. The collection may
// contain dots; the database is the first segment.
func SplitNamespace(namespace string) (database, collection string, ok bool) {
	ns := strings.TrimSpace(namespace)
	db, coll, found := strings.Cut(ns, ".")
	if !found || db == "" || coll == "" {
		return "", "", false
	}
	return db, coll, true
}

// UnknownIndexInventory records each namespace as unknown without calling a
// server.
func UnknownIndexInventory(namespaces []string, reason string) IndexInventoryView {
	if reason == "" {
		reason = "not_fetched"
	}
	view := CollectIndexInventory(context.Background(), namespaces, func(context.Context, string) NamespaceIndexInventory {
		return NamespaceIndexInventory{Status: "unknown", Reason: reason}
	})
	view.Reason = reason
	return view
}

// ProfileQuery classifies a captured query into equality fields, range fields,
// and an ordered sort when the sort has a defined key order. Map key order is
// not recorded and is not an index order.
func ProfileQuery(q *ExecutableQuery) PredicateProfile {
	p := PredicateProfile{Source: "captured_query"}
	if q == nil {
		p.Unsupported = []string{"query_not_captured"}
		return p
	}
	if len(q.Pipeline) > 0 {
		profilePipeline(q.Pipeline, &p)
	} else {
		classifyFilter(q.Filter, &p)
	}
	finishProfile(&p)
	return p
}

func finishProfile(p *PredicateProfile) {
	p.EqualityFields = sortedKeySet(p.eqSet)
	p.RangeFields = sortedKeySet(p.rangeSet)
	p.Unsupported = dedupeAndSortStrings(p.Unsupported)
	p.eqSet, p.rangeSet = nil, nil
}

func classifyFilter(v interface{}, p *PredicateProfile) {
	ensureSets(p)
	m, ok := asStringMap(v)
	if !ok {
		return
	}
	if raw, ok := m["$and"]; ok {
		items, isList := raw.([]interface{})
		if !isList {
			p.Unsupported = append(p.Unsupported, "$and")
		}
		for _, item := range items {
			classifyFilter(item, p)
		}
	}
	for k, val := range m {
		switch {
		case k == "$and" || k == "$comment":
			continue
		case strings.HasPrefix(k, "$"):
			p.Unsupported = append(p.Unsupported, k)
		default:
			classifyField(k, val, p)
		}
	}
}

func ensureSets(p *PredicateProfile) {
	if p.eqSet == nil {
		p.eqSet = map[string]struct{}{}
	}
	if p.rangeSet == nil {
		p.rangeSet = map[string]struct{}{}
	}
	if p.equalityValue == nil {
		p.equalityValue = map[string]string{}
	}
}

func classifyField(field string, val interface{}, p *PredicateProfile) {
	ensureSets(p)
	m, ok := asStringMap(val)
	if !ok {
		token, ok := CanonicalPredicateValue(val)
		if !ok {
			p.Unsupported = append(p.Unsupported, "unsupported_value")
			return
		}
		p.eqSet[field] = struct{}{}
		p.equalityValue[field] = token
		return
	}
	if !operatorDoc(m) {
		p.Unsupported = append(p.Unsupported, "embedded_document")
		return
	}
	for op, operand := range m {
		switch op {
		case "$comment":
			continue
		case "$eq":
			token, ok := CanonicalPredicateValue(operand)
			if !ok {
				p.Unsupported = append(p.Unsupported, "$eq")
				continue
			}
			p.eqSet[field] = struct{}{}
			p.equalityValue[field] = token
		case "$gt", "$gte", "$lt", "$lte":
			p.rangeSet[field] = struct{}{}
		case "$in":
			p.InPredicate = true
			p.eqSet[field] = struct{}{}
		default:
			p.Unsupported = append(p.Unsupported, op)
		}
	}
}

func operatorDoc(m map[string]interface{}) bool {
	if len(m) == 0 {
		return false
	}
	for k := range m {
		if !strings.HasPrefix(k, "$") {
			return false
		}
	}
	return true
}

func asStringMap(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

func profilePipeline(pipeline []interface{}, p *PredicateProfile) {
	ensureSets(p)
	reduced := false
	for _, st := range pipeline {
		m, ok := asStringMap(st)
		if !ok || len(m) != 1 {
			p.Unsupported = append(p.Unsupported, "unreadable_stage")
			continue
		}
		var name string
		var body interface{}
		for name, body = range m {
		}
		switch name {
		case "$match":
			if reduced {
				p.Unsupported = append(p.Unsupported, "match_after_reduction")
			} else {
				classifyFilter(body, p)
			}
		case "$sort":
			if reduced {
				p.Unsupported = append(p.Unsupported, "sort_after_reduction")
				continue
			}
			keys, known := sortKeysFromValue(body)
			p.SortKeys, p.SortOrderKnown = keys, known
			if !known {
				p.Unsupported = append(p.Unsupported, "sort_key_order_undefined")
			}
		case "$group", "$bucket", "$bucketAuto", "$sortByCount":
			reduced = true
		case "$facet":
			reduced = true
			p.Unsupported = append(p.Unsupported, "$facet")
		case "$lookup":
			p.HasLookup = true
		}
	}
}

func sortKeysFromValue(v interface{}) ([]IndexKeySpec, bool) {
	if d, ok := v.(bson.D); ok {
		keys := make([]IndexKeySpec, 0, len(d))
		for _, e := range d {
			k, ok := directionKey(e.Key, e.Value, "sort")
			if !ok {
				return nil, false
			}
			keys = append(keys, k)
		}
		return keys, true
	}
	m, ok := asStringMap(v)
	if !ok {
		return nil, false
	}
	switch len(m) {
	case 0:
		return nil, true
	case 1:
		for field, dir := range m {
			k, ok := directionKey(field, dir, "sort")
			if !ok {
				return nil, false
			}
			return []IndexKeySpec{k}, true
		}
	}
	return nil, false
}

func directionKey(field string, dir interface{}, role string) (IndexKeySpec, bool) {
	n, ok := scalarDirection(dir)
	if !ok {
		return IndexKeySpec{}, false
	}
	kind := "asc"
	if n < 0 {
		kind = "desc"
	}
	return IndexKeySpec{Field: field, Direction: n, Kind: kind, Role: role}, true
}

func scalarDirection(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		if n == 1 || n == -1 {
			return n, true
		}
	case int32:
		if n == 1 || n == -1 {
			return int(n), true
		}
	case int64:
		if n == 1 || n == -1 {
			return int(n), true
		}
	case float64:
		if n == 1 || n == -1 {
			return int(n), true
		}
	}
	return 0, false
}

func sortedKeySet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deriveCandidate orders keys by equality, then ordered sort, then one range
// field. filterDocumentOrder is intentionally not read. Multiple equality
// fields are ordered only when every one of them has selectivity evidence
// from the same source and the counts are unique.
func deriveCandidate(p PredicateProfile, sel []SelectivityEvidence) candidatePlan {
	plan := candidatePlan{
		EqualityFields: append([]string(nil), p.EqualityFields...),
		Unsupported:    append([]string(nil), p.Unsupported...),
	}
	sort.Strings(plan.EqualityFields)
	if len(plan.Unsupported) > 0 {
		plan.Reason = "unsupported_pattern"
		return plan
	}
	if len(p.SortKeys) > 0 && !p.SortOrderKnown {
		plan.Reason = "sort_key_order_undefined"
		plan.Unsupported = append(plan.Unsupported, "sort_key_order_undefined")
		return plan
	}

	ranges := append([]string(nil), p.RangeFields...)
	sort.Strings(ranges)
	var rangeField string
	sortFields := map[string]struct{}{}
	for _, k := range p.SortKeys {
		sortFields[k.Field] = struct{}{}
	}
	switch {
	case len(ranges) == 0:
	case len(ranges) == 1:
		rangeField = ranges[0]
	default:
		var sortedRange string
		for _, f := range ranges {
			if _, ok := sortFields[f]; ok {
				if sortedRange != "" {
					plan.Reason = "multiple_ranges"
					return plan
				}
				sortedRange = f
			}
		}
		if sortedRange == "" {
			plan.Reason = "multiple_ranges"
			plan.ResidualRanges = ranges
			return plan
		}
		rangeField = sortedRange
		for _, f := range ranges {
			if f != sortedRange {
				plan.ResidualRanges = append(plan.ResidualRanges, f)
			}
		}
	}

	eqOrder, eqReason, src := orderEquality(plan.EqualityFields, sel)
	if eqReason != "" {
		plan.Reason = eqReason
		return plan
	}
	plan.SelectivitySource = src

	seen := map[string]struct{}{}
	for _, f := range eqOrder {
		plan.Keys = append(plan.Keys, IndexKeySpec{Field: f, Direction: 1, Kind: "asc", Role: "equality"})
		seen[f] = struct{}{}
	}
	for _, k := range p.SortKeys {
		if _, ok := seen[k.Field]; ok {
			continue
		}
		k.Role = "sort"
		plan.Keys = append(plan.Keys, k)
		seen[k.Field] = struct{}{}
	}
	if rangeField != "" {
		if _, ok := seen[rangeField]; !ok {
			plan.Keys = append(plan.Keys, IndexKeySpec{Field: rangeField, Direction: 1, Kind: "asc", Role: "range"})
		}
	}
	plan.Determined = true
	if src != "" {
		plan.Reason = "selectivity"
	} else {
		plan.Reason = "equality_sort_range"
	}
	return plan
}

func orderEquality(fields []string, sel []SelectivityEvidence) ([]string, string, string) {
	if len(fields) <= 1 {
		return fields, "", ""
	}
	byField := map[string]SelectivityEvidence{}
	source := ""
	for _, s := range sel {
		if s.DistinctCount == nil || strings.TrimSpace(s.Source) == "" {
			continue
		}
		if source == "" {
			source = s.Source
		}
		if s.Source != source {
			return nil, "equality_order_undetermined", ""
		}
		byField[s.Field] = s
	}
	if len(byField) != len(fields) {
		return nil, "equality_order_undetermined", ""
	}
	counts := map[int64]struct{}{}
	for _, f := range fields {
		c := *byField[f].DistinctCount
		if _, ok := counts[c]; ok {
			return nil, "equality_order_undetermined", ""
		}
		counts[c] = struct{}{}
	}
	ordered := append([]string(nil), fields...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return *byField[ordered[i]].DistinctCount < *byField[ordered[j]].DistinctCount
	})
	return ordered, "", source
}

func formatKey(k IndexKeySpec) string {
	switch k.Kind {
	case "asc":
		return k.Field + ":1"
	case "desc":
		return k.Field + ":-1"
	case "":
		if k.Direction < 0 {
			return k.Field + ":-1"
		}
		return k.Field + ":1"
	default:
		return k.Field + ":" + k.Kind
	}
}

func formatKeys(keys []IndexKeySpec) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = formatKey(k)
	}
	return strings.Join(parts, ",")
}

func btreeKind(kind string) bool {
	return kind == "asc" || kind == "desc" || kind == ""
}

func flipKeys(keys []IndexKeySpec) []IndexKeySpec {
	out := make([]IndexKeySpec, len(keys))
	for i, k := range keys {
		out[i] = k
		if k.Direction > 0 {
			out[i].Direction = -1
			out[i].Kind = "desc"
		} else if k.Direction < 0 {
			out[i].Direction = 1
			out[i].Kind = "asc"
		}
	}
	return out
}

func sameFieldNames(keys []IndexKeySpec, fields []string) bool {
	if len(keys) != len(fields) {
		return false
	}
	got := make([]string, len(keys))
	for i, k := range keys {
		if !btreeKind(k.Kind) {
			return false
		}
		got[i] = k.Field
	}
	want := append([]string(nil), fields...)
	sort.Strings(got)
	sort.Strings(want)
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func keysServe(index, candidate []IndexKeySpec) bool {
	if len(candidate) == 0 || len(index) < len(candidate) {
		return false
	}
	return serveForward(index, candidate) || serveForward(flipKeys(index), candidate)
}

func serveForward(index, candidate []IndexKeySpec) bool {
	eq := 0
	for _, k := range candidate {
		if k.Role == "equality" {
			eq++
			continue
		}
		break
	}
	if eq > 0 && !sameFieldNames(index[:eq], fieldNames(candidate[:eq])) {
		return false
	}
	for i := eq; i < len(candidate); i++ {
		ik := index[i]
		ck := candidate[i]
		if ik.Field != ck.Field || ik.Direction != ck.Direction || !btreeKind(ik.Kind) || !btreeKind(ck.Kind) {
			return false
		}
	}
	return true
}

func fieldNames(keys []IndexKeySpec) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Field
	}
	return out
}

func planTail(p candidatePlan, pred PredicateProfile) []IndexKeySpec {
	if p.Determined {
		var tail []IndexKeySpec
		for _, k := range p.Keys {
			if k.Role != "equality" {
				tail = append(tail, k)
			}
		}
		return tail
	}
	tail := append([]IndexKeySpec(nil), pred.SortKeys...)
	for i := range tail {
		tail[i].Role = "sort"
	}
	return tail
}

func indexCouldServe(idx DeployedIndex, plan candidatePlan, pred PredicateProfile) bool {
	if plan.Determined && len(plan.Keys) == 1 && plan.Keys[0].Role == "equality" {
		if len(idx.Keys) == 1 && idx.Keys[0].Kind == "hashed" && idx.Keys[0].Field == plan.Keys[0].Field {
			return true
		}
	}
	if plan.Determined {
		return keysServe(idx.Keys, plan.Keys)
	}
	if plan.Reason != "equality_order_undetermined" || len(plan.EqualityFields) == 0 {
		return false
	}
	tail := planTail(plan, pred)
	return leadingSetAndTail(idx.Keys, plan.EqualityFields, tail) || leadingSetAndTail(flipKeys(idx.Keys), plan.EqualityFields, tail)
}

func leadingSetAndTail(index []IndexKeySpec, eq []string, tail []IndexKeySpec) bool {
	if len(index) < len(eq)+len(tail) {
		return false
	}
	if !sameFieldNames(index[:len(eq)], eq) {
		return false
	}
	for i := range tail {
		ik := index[len(eq)+i]
		if ik.Field != tail[i].Field || ik.Direction != tail[i].Direction || !btreeKind(ik.Kind) {
			return false
		}
	}
	return true
}

func sharesField(idx DeployedIndex, plan candidatePlan) bool {
	want := map[string]struct{}{}
	for _, f := range plan.EqualityFields {
		want[f] = struct{}{}
	}
	for _, k := range plan.Keys {
		want[k.Field] = struct{}{}
	}
	for _, k := range idx.Keys {
		if _, ok := want[k.Field]; ok {
			return true
		}
	}
	return false
}

func specialType(idx DeployedIndex) bool {
	for _, k := range idx.Keys {
		if k.Kind != "" && !btreeKind(k.Kind) && k.Kind != "hashed" {
			return true
		}
	}
	return idx.Type != "" && idx.Type != "btree" && idx.Type != "hashed"
}

func collationOK(locale string) bool {
	switch strings.ToLower(strings.TrimSpace(locale)) {
	case "", "simple":
		return true
	default:
		return false
	}
}

func partialProven(idx DeployedIndex, pred PredicateProfile) bool {
	if !idx.Partial {
		return true
	}
	if len(idx.PartialPredicates) == 0 {
		return false
	}
	for _, pp := range idx.PartialPredicates {
		if pp.Op != "eq" && pp.Op != "$eq" {
			return false
		}
		got, ok := pred.equalityValue[pp.Field]
		if !ok || got == "" || got != pp.Value {
			return false
		}
	}
	return true
}

func multikeyBlocks(idx DeployedIndex, plan candidatePlan) string {
	hasSort := false
	for _, k := range plan.Keys {
		if k.Role == "sort" || k.Role == "range" {
			hasSort = true
		}
	}
	if idx.Multikey != nil && *idx.Multikey {
		if hasSort || len(idx.Keys) > 1 {
			if hasSort {
				return "multikey_sort"
			}
			return "multikey_compound"
		}
	}
	return ""
}

func evaluateIndex(idx DeployedIndex, plan candidatePlan, pred PredicateProfile) (bool, string) {
	if !indexCouldServe(idx, plan, pred) {
		if specialType(idx) && sharesField(idx, plan) {
			return false, "unsupported_type"
		}
		if sharesField(idx, plan) {
			return false, "shared_field_only"
		}
		return false, "not_a_prefix"
	}
	if idx.Hidden {
		return false, "hidden"
	}
	if !collationOK(idx.Collation) {
		return false, "collation"
	}
	if !partialProven(idx, pred) {
		return false, "partial_not_proven"
	}
	if reason := multikeyBlocks(idx, plan); reason != "" {
		return false, reason
	}
	return true, "suitable"
}

func workSignalNote(obs []string) string {
	for _, o := range obs {
		if o == "blocking_sort" || o == "spill" {
			return scanWorkSignalNote
		}
	}
	return ""
}

func efficiencyObservations(rep InsightsReport, shapeID string) []string {
	for _, s := range rep.ExplainCoverage.Shapes {
		if s.ShapeID == shapeID && s.Efficiency != nil {
			return s.Efficiency.Observations
		}
	}
	return nil
}

func needsAdvice(s ExplainCoverageShape) bool {
	if s.Status != "explained" && s.SelectedBy == "" {
		return false
	}
	if s.Efficiency != nil {
		if workSignalNote(s.Efficiency.Observations) != "" {
			return true
		}
		if s.Efficiency.Status == EfficiencyPartial {
			return true
		}
		if s.Efficiency.Status == EfficiencyRated && s.Efficiency.Score != nil && *s.Efficiency.Score < efficiencyEfficientMin {
			return true
		}
	}
	if s.Finding != nil {
		switch s.Finding.Diagnosis {
		case DiagCollscanSelective, DiagIndexLowSelectivity, DiagSpillToDisk, DiagBlockingSort, DiagCollscanPlanOnly, DiagIndexPlanOnly:
			return true
		}
	}
	return false
}

type adviceDraft struct {
	kind      string
	namespace string
	plan      candidatePlan
	pred      PredicateProfile
	suitable  *DeployedIndex
	notes     []string
	shapes    []ExplainCoverageShape
	dedupe    string
}

func buildIndexAdvice(rep *InsightsReport) []InsightRecommendation {
	if rep == nil || rep.ExplainCoverage.Status != "ready" {
		return nil
	}
	order := make([]string, 0)
	groups := map[string]*adviceDraft{}
	for _, s := range rep.ExplainCoverage.Shapes {
		if !needsAdvice(s) {
			continue
		}
		d := adviseShape(rep, s)
		g, ok := groups[d.dedupe]
		if !ok {
			cp := d
			groups[d.dedupe] = &cp
			order = append(order, d.dedupe)
			g = groups[d.dedupe]
		}
		g.shapes = append(g.shapes, s)
		if confidenceScore(shapeAdviceConfidence(s, d)) < confidenceScore(shapeAdviceConfidence(s, *g)) {
			g.kind = d.kind
		}
	}
	recs := make([]InsightRecommendation, 0, len(order))
	for _, k := range order {
		recs = append(recs, renderAdvice(rep, groups[k]))
	}
	return recs
}

func adviseShape(rep *InsightsReport, s ExplainCoverageShape) adviceDraft {
	pred := PredicateProfile{}
	if s.Predicates != nil {
		pred = *s.Predicates
	} else {
		pred.Unsupported = []string{"predicates_not_captured"}
	}
	plan := deriveCandidate(pred, nil)
	plannerOnly := advicePlannerOnly(s)
	d := adviceDraft{namespace: s.Namespace, plan: plan, pred: pred, kind: "investigate"}
	if plannerOnly {
		d.kind = "measure"
	} else if plan.Determined || plan.Reason == "equality_order_undetermined" {
		inv, fetched := namespaceInventory(rep, s.Namespace)
		if fetched {
			suitable, notes := matchInventory(inv, plan, pred)
			d.notes = notes
			if suitable != nil {
				d.suitable = suitable
				d.kind = "existing_index"
			} else if plan.Determined {
				d.kind = "candidate"
			}
		} else if plan.Determined {
			d.kind = "candidate"
		}
	}
	d.dedupe = adviceDedupeKey(d)
	return d
}

func advicePlannerOnly(s ExplainCoverageShape) bool {
	if s.Evidence == "planner_only" {
		return true
	}
	if s.Efficiency != nil && s.Efficiency.Status == EfficiencyPartial {
		for _, sm := range s.Samples {
			if sm.Diagnostics != nil && sm.Diagnostics.ExecutionStatsAvailable {
				return false
			}
		}
		return true
	}
	for _, sm := range s.Samples {
		if sm.Status == "explained" && sm.Diagnostics != nil && sm.Diagnostics.ExecutionStatsAvailable {
			return false
		}
	}
	return true
}

func adviceDedupeKey(d adviceDraft) string {
	base := d.namespace + "|" + d.kind + "|" + d.plan.Reason
	if d.suitable != nil {
		return base + "|idx:" + d.suitable.Name
	}
	if d.plan.Determined {
		return base + "|" + formatKeys(d.plan.Keys)
	}
	fields := append([]string(nil), d.plan.EqualityFields...)
	sort.Strings(fields)
	return base + "|eq:" + strings.Join(fields, ",") + "|sort:" + formatKeys(d.pred.SortKeys)
}

func namespaceInventory(rep *InsightsReport, namespace string) (NamespaceIndexInventory, bool) {
	if rep.IndexInventory == nil {
		return NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: "not_fetched"}, false
	}
	for _, ns := range rep.IndexInventory.Namespaces {
		if ns.Namespace == namespace {
			return ns, ns.Status == "fetched"
		}
	}
	reason := rep.IndexInventory.Reason
	if reason == "" {
		reason = "not_fetched"
	}
	return NamespaceIndexInventory{Namespace: namespace, Status: "unknown", Reason: reason}, false
}

func matchInventory(inv NamespaceIndexInventory, plan candidatePlan, pred PredicateProfile) (*DeployedIndex, []string) {
	var suitable *DeployedIndex
	notes := make([]string, 0)
	for i := range inv.Indexes {
		idx := inv.Indexes[i]
		ok, reason := evaluateIndex(idx, plan, pred)
		if ok {
			suitable = &inv.Indexes[i]
			if idx.Sparse {
				notes = append(notes, fmt.Sprintf("Index %s is sparse. Documents missing the indexed field are omitted; this equality or range predicate requires the field to be present.", idx.Name))
			}
			if idx.Multikey == nil {
				notes = append(notes, fmt.Sprintf("listIndexes did not report whether %s is multikey. That is not a claim that the index is not multikey.", idx.Name))
			}
			if idx.Partial {
				notes = append(notes, fmt.Sprintf("Index %s is partial. The captured equality predicate matches its filter fields; confirm the filter still includes this query.", idx.Name))
			}
			break
		}
		notes = append(notes, indexRejectNote(idx, reason))
	}
	return suitable, compactNotes(notes)
}

func indexRejectNote(idx DeployedIndex, reason string) string {
	switch reason {
	case "hidden":
		return fmt.Sprintf("Index %s matches this predicate but is hidden, so the planner will not use it. This report does not suggest unhiding or dropping it.", idx.Name)
	case "shared_field_only":
		return fmt.Sprintf("Index %s shares a field with this predicate but its key prefix does not cover it. A shared field is not coverage.", idx.Name)
	case "collation":
		return fmt.Sprintf("Index %s has collation %q. Query collation was not captured, so the index is not treated as suitable.", idx.Name, idx.Collation)
	case "partial_not_proven":
		return fmt.Sprintf("Index %s is partial and this predicate was not shown to imply its filter, so it is not treated as coverage.", idx.Name)
	case "multikey_sort":
		return fmt.Sprintf("Index %s is multikey and is not treated as providing this sort.", idx.Name)
	case "multikey_compound":
		return fmt.Sprintf("Index %s is multikey across more than one key and is not treated as coverage.", idx.Name)
	case "unsupported_type":
		return fmt.Sprintf("Index %s is type %s and is not coverage for this predicate.", idx.Name, emptyFallback(idx.Type, indexKindLabel(idx)))
	default:
		return ""
	}
}

func indexKindLabel(idx DeployedIndex) string {
	for _, k := range idx.Keys {
		if !btreeKind(k.Kind) && k.Kind != "" {
			return k.Kind
		}
	}
	return "unknown"
}

func compactNotes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, n := range in {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func shapeAdviceConfidence(s ExplainCoverageShape, d adviceDraft) string {
	base := "medium"
	if s.Finding != nil && s.Finding.Confidence != "" {
		base = s.Finding.Confidence
	} else if s.Efficiency != nil && s.Efficiency.Confidence != "" {
		base = s.Efficiency.Confidence
	}
	capAt := "high"
	partial, lookup, sharded := shapeQualifiers(s)
	switch {
	case d.kind == "measure" || partial:
		capAt = "low"
	case !d.plan.Determined || lookup || sharded || explainedSampleCount(s) < 2 || d.pred.InPredicate:
		capAt = "medium"
	}
	if confidenceScore(base) > confidenceScore(capAt) {
		return capAt
	}
	return base
}

func explainedSampleCount(s ExplainCoverageShape) int {
	n := 0
	for _, sm := range s.Samples {
		if sm.Status == "explained" && sm.Diagnostics != nil {
			n++
		}
	}
	return n
}

func shapeQualifiers(s ExplainCoverageShape) (partial, lookup, sharded bool) {
	if s.Evidence == "planner_only" {
		partial = true
	}
	if s.Predicates != nil && s.Predicates.HasLookup {
		lookup = true
	}
	if s.Efficiency != nil {
		if s.Efficiency.Status == EfficiencyPartial {
			partial = true
		}
		for _, lim := range s.Efficiency.Limitations {
			if strings.Contains(lim, "$lookup") {
				lookup = true
			}
		}
		for _, sm := range s.Efficiency.Samples {
			if strings.HasPrefix(sm.Layout, "sharded") {
				sharded = true
			}
		}
	}
	for _, sm := range s.Samples {
		if sm.Diagnostics == nil || sm.Diagnostics.Metrics == nil {
			continue
		}
		m := sm.Diagnostics.Metrics
		if m.Shards > 0 || strings.HasPrefix(m.Layout, "sharded") {
			sharded = true
		}
		for _, n := range m.Notes {
			if strings.Contains(n, "lookup") {
				lookup = true
			}
		}
	}
	return partial, lookup, sharded
}

func planIgnoredIndex(shapes []ExplainCoverageShape, idx *DeployedIndex) bool {
	if idx == nil {
		return false
	}
	for _, s := range shapes {
		for _, sm := range s.Samples {
			d := sm.Diagnostics
			if sm.Status != "explained" || d == nil || !d.CollectionScanDetected {
				continue
			}
			used := false
			for _, name := range d.IndexesUsed {
				if name == idx.Name {
					used = true
				}
			}
			if !used {
				return true
			}
		}
	}
	return false
}

func renderAdvice(rep *InsightsReport, d *adviceDraft) InsightRecommendation {
	if d.kind == "existing_index" && planIgnoredIndex(d.shapes, d.suitable) {
		d.notes = append(d.notes, fmt.Sprintf("Index %s matches this predicate shape but an explained plan did not use it, so it is not treated as coverage.", d.suitable.Name))
		d.kind = "candidate"
		d.suitable = nil
	}
	inv, fetched := namespaceInventory(rep, d.namespace)
	if !fetched && d.kind == "candidate" {
		d.notes = append(d.notes, fmt.Sprintf("Deployed indexes on %s are unknown (%s). This is not evidence that an index is absent.", emptyFallback(d.namespace, "the namespace"), emptyFallback(inv.Reason, "not_fetched")))
	}
	partial, lookup, sharded := false, false, false
	var observations []string
	conf := "high"
	for _, s := range d.shapes {
		p, l, sh := shapeQualifiers(s)
		partial, lookup, sharded = partial || p, lookup || l, sharded || sh
		if s.Efficiency != nil {
			observations = append(observations, s.Efficiency.Observations...)
		}
		c := shapeAdviceConfidence(s, *d)
		if confidenceScore(c) < confidenceScore(conf) {
			conf = c
		}
	}
	if !fetched {
		conf = "low"
	}

	limits := append([]string(nil), d.notes...)
	if partial {
		limits = append(limits, "Explain returned only part of the counts, so selectivity was not fully measured and the scan efficiency score is an upper bound.")
	}
	if lookup {
		limits = append(limits, "Qualified: documents examined by $lookup in the joined collection are not part of scan efficiency, and this advice does not cover the joined collection.")
	}
	if sharded {
		limits = append(limits, "Qualified: this explain used a sharded layout. Live mongos, replica sets, and servers older than 8.0 were not validated.")
	}
	if d.pred.InPredicate && len(d.pred.SortKeys) > 0 {
		limits = append(limits, "A multi-value $in can prevent an index from providing the following sort. Confirm that with explain.")
	}
	if len(d.plan.ResidualRanges) > 0 {
		limits = append(limits, "Range fields not placed in the candidate stay as residual predicates: "+strings.Join(d.plan.ResidualRanges, ", ")+".")
	}
	if note := workSignalNote(observations); note != "" {
		limits = append(limits, note)
	}
	limits = compactNotes(limits)

	metrics := make([]string, 0)
	impacts := make([]string, 0)
	ids := make([]string, 0, len(d.shapes))
	var diagnosis string
	var score *int
	for _, s := range d.shapes {
		ids = append(ids, s.ShapeID)
		if diagnosis == "" && s.Finding != nil {
			diagnosis = s.Finding.Diagnosis
		}
		if s.Efficiency != nil && s.Efficiency.Score != nil && (score == nil || *s.Efficiency.Score < *score) {
			score = s.Efficiency.Score
		}
		metrics = append(metrics, metricLines(s)...)
		impacts = append(impacts, impactLine(rep, s))
	}

	var keys []string
	existing := ""
	if d.kind == "candidate" && d.plan.Determined {
		for _, k := range d.plan.Keys {
			keys = append(keys, formatKey(k))
		}
	}
	if d.suitable != nil {
		existing = d.suitable.Name + " (" + formatKeys(d.suitable.Keys) + ")"
	}
	rationale := candidateRationale(d.plan)
	if d.kind == "existing_index" && d.suitable != nil {
		rationale = fmt.Sprintf("Index %s (%s) already serves this predicate, so no additional index is suggested. Coverage required the key prefix and sort direction, not a shared field alone.", d.suitable.Name, formatKeys(d.suitable.Keys))
	}
	if d.kind == "measure" {
		rationale = "Execution statistics were not available, so no index key order is proposed. The smallest additional measurement is an explain with executionStats on this shape."
	}

	trade := indexTradeoffBase
	if mix, ok := writeMixText(rep, d.namespace); ok {
		trade += " " + mix
	} else {
		trade += " Write mix was not used because impact coverage does not support a measured read/write count for this namespace."
	}

	title := adviceTitle(d)
	details := strings.Join(compactNotes([]string{
		rationale,
		keySentence(d, keys),
		"Namespace: " + emptyFallback(d.namespace, "(unknown)") + ". Shapes: " + strings.Join(ids, ", ") + ".",
		"Metrics: " + emptyFallback(strings.Join(metrics, "; "), "no execution metrics."),
		"Workload impact: " + strings.Join(impacts, "; ") + ".",
		"Limitations: " + emptyFallback(strings.Join(limits, " "), "none."),
		"Tradeoffs: " + trade,
		"Validation: " + indexValidationStep,
	}), " ")

	return InsightRecommendation{
		Priority:        advicePriority(d.kind, score),
		Title:           title,
		Details:         details,
		Confidence:      conf,
		Source:          "index_advice",
		ShapeID:         firstString(ids),
		ShapeIDs:        ids,
		Diagnosis:       diagnosis,
		Namespace:       d.namespace,
		AdviceKind:      d.kind,
		CandidateKeys:   keys,
		ExistingIndex:   existing,
		InventoryStatus: inv.Status,
		Metrics:         metrics,
		WorkloadImpact:  strings.Join(impacts, "; "),
		Rationale:       rationale,
		Limitations:     limits,
		Tradeoffs:       trade,
		ValidationStep:  indexValidationStep,
		Evidence:        append(append([]string{}, metrics...), impacts...),
	}
}

func adviceTitle(d *adviceDraft) string {
	ns := emptyFallback(d.namespace, "the collection")
	switch d.kind {
	case "existing_index":
		return "Existing index already serves " + ns
	case "candidate":
		return "Candidate index for " + ns
	case "measure":
		return "Measure execution statistics before indexing " + ns
	default:
		return "Investigate indexing for " + ns
	}
}

func advicePriority(kind string, score *int) string {
	if kind == "existing_index" {
		return "low"
	}
	if score != nil && *score < efficiencyModerateMin {
		return "high"
	}
	return "medium"
}

func candidateRationale(plan candidatePlan) string {
	switch plan.Reason {
	case "selectivity":
		return "Key order is the equality fields from most selective measured distinct count, then ordered sort keys and their directions, then one range field. Filter-document order is not used. The distinct counts are not a speedup estimate. Source: " + plan.SelectivitySource + "."
	case "equality_sort_range":
		return "Key order is equality fields, then ordered sort keys and their directions, then one range field. Filter-document order is not used."
	case "equality_order_undetermined":
		return "Equality fields [" + strings.Join(plan.EqualityFields, ", ") + "] belong in the index prefix, before sort keys and a single range field. Their relative order was not determined because per-field selectivity was not measured. Filter-document order is not an index order."
	case "multiple_ranges":
		return "More than one range predicate is present and per-field selectivity was not measured, so this report does not choose which range field leads. Compare the alternatives with explain."
	case "sort_key_order_undefined":
		return "The sort has more than one key and that order was not preserved, so no index key order is proposed. A blocking sort or spill, if observed, remains an observation."
	case "unsupported_pattern":
		return "This predicate is not a supported equality, sort, and range pattern (" + strings.Join(plan.Unsupported, ", ") + "), so no index key order is proposed. Compare plans with explain instead of indexing fields in filter-document order."
	default:
		if len(plan.Unsupported) > 0 && plan.Unsupported[0] == "predicates_not_captured" {
			return "Predicate roles were not captured, so no index key order is proposed. Filter field names are not an index order."
		}
		return "No index key order was determined from the captured predicate."
	}
}

func keySentence(d *adviceDraft, keys []string) string {
	if d.kind == "existing_index" {
		return "No new index is suggested."
	}
	if len(keys) == 0 {
		return "No ordered candidate keys are proposed."
	}
	return "Candidate keys, in order: " + strings.Join(keys, ", ") + "."
}

func metricLines(s ExplainCoverageShape) []string {
	out := make([]string, 0, 3)
	if s.Efficiency != nil && s.Efficiency.Score != nil {
		bound := ""
		if s.Efficiency.ScoreIsUpperBound {
			bound = " upper bound"
		}
		out = append(out, fmt.Sprintf("scan efficiency %s score %d%s (%s), limiting %s", s.Efficiency.Status, *s.Efficiency.Score, bound, emptyFallback(s.Efficiency.Band, "unconfirmed"), emptyFallback(s.Efficiency.LimitingDimension, "none")))
	}
	for _, sm := range s.Samples {
		d := sm.Diagnostics
		if d == nil || !d.ExecutionStatsAvailable {
			continue
		}
		out = append(out, fmt.Sprintf("%s docsExamined=%d keysExamined=%d %s=%d", sm.Role, d.DocsExamined, d.KeysExamined, resultLabel(s.Operation), resultCount(d, s.Operation)))
	}
	return out
}

func impactLine(rep *InsightsReport, s ExplainCoverageShape) string {
	for _, g := range rep.Impact.Groups {
		if g.ShapeID == s.ShapeID {
			return fmt.Sprintf("%s %s on %s: %d operations, %.2f ms summed concurrent operation time, %.1f%% of observed operation time",
				s.ShapeID, s.Operation, s.Namespace, g.OperationCount, g.TotalOperationMs, g.ShareOfObservedTime*100)
		}
	}
	if s.OperationCount > 0 {
		return fmt.Sprintf("%s %s on %s: %d operations in the explain inventory; this shape was not in the listed impact groups", s.ShapeID, s.Operation, s.Namespace, s.OperationCount)
	}
	return fmt.Sprintf("%s on %s: workload impact was not listed", emptyFallback(s.ShapeID, s.Operation), s.Namespace)
}

func writeMixText(rep *InsightsReport, namespace string) (string, bool) {
	cov := rep.Impact.Coverage
	if rep.Impact.Status != "ready" && rep.Impact.Status != "" {
		return "", false
	}
	if !cov.GroupCoverageComplete || cov.NotShownGroups > 0 || cov.OverflowOperations > 0 {
		return "", false
	}
	var reads, writes uint64
	saw := false
	for _, g := range rep.Impact.Groups {
		if g.Namespace != namespace {
			continue
		}
		saw = true
		if isWriteOperation(g.Operation) {
			writes += g.OperationCount
		} else {
			reads += g.OperationCount
		}
	}
	if !saw {
		return "", false
	}
	return fmt.Sprintf("This run recorded %d reads and %d writes on %s (full-run counts; impact coverage is complete). That count is not an estimate of the cost of maintaining another index.", reads, writes, namespace), true
}

func isWriteOperation(op string) bool {
	switch op {
	case "insert", "insertOne", "insertMany", "updateOne", "updateMany", "deleteOne", "deleteMany", "replaceOne":
		return true
	default:
		return false
	}
}

func firstString(in []string) string {
	if len(in) == 0 {
		return ""
	}
	return in[0]
}
