package definitions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

const orderedSortQueries = `{"queries":[{"name":"by_b_then_a","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"b":-1,"a":1}}]}]}`

func sortSnippet(t *testing.T, content string) string {
	t.Helper()
	parsed, err := config.ParseQueriesBytes([]byte(content))
	if err != nil {
		t.Fatalf("parse stored content: %v", err)
	}
	b, err := json.Marshal(parsed.Queries[0].Pipeline)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStoreKeepsSortOrderThroughSaveUpdateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "definitions.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"$sort":{"b":-1,"a":1}}]`

	created, err := store.Create(KindQuery, Input{Name: "sorted", Content: orderedSortQueries})
	if err != nil {
		t.Fatal(err)
	}
	if got := sortSnippet(t, created.Content); got != want {
		t.Fatalf("create stored %s, want %s", got, want)
	}
	if created.ContentVersion != ContentVersionOrderedSorts {
		t.Fatalf("content version = %d", created.ContentVersion)
	}

	// The editor sends content as a string; API clients may send an object.
	var in Input
	body := `{"name":"sorted","content":` + strings.Replace(orderedSortQueries, `"b":-1,"a":1`, `"a":1,"b":-1`, 1) + `}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update(KindQuery, created.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortSnippet(t, updated.Content); got != `[{"$sort":{"a":1,"b":-1}}]` {
		t.Fatalf("object content update stored %s", got)
	}

	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(KindQuery, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sortSnippet(t, got.Content) != `[{"$sort":{"a":1,"b":-1}}]` || got.ContentVersion != ContentVersionOrderedSorts {
		t.Fatalf("reloaded definition lost order or version: %+v", got)
	}
	parsed, _ := config.ParseQueriesBytes([]byte(got.Content))
	if err := got.CheckSortOrder(parsed.Queries); err != nil {
		t.Fatalf("current definitions are unambiguous: %v", err)
	}
}

func TestStoreRejectsAmbiguousSortContent(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Create(KindQuery, Input{Name: "dup", Content: `[{"collection":"c","operation":"aggregate","pipeline":[{"$sort":{"a":1,"a":-1}}]}]`})
	if err == nil || !strings.Contains(err.Error(), "declared more than once") {
		t.Fatalf("duplicate sort key must be rejected, got %v", err)
	}
}

const legacyMultiContent = `{"queries":[{"name":"q1","collection":"c","operation":"aggregate","pipeline":[{"$match":{"x":1}},{"$sort":{"a":1,"b":-1}}]}]}`

// legacyNestedContent has multi-key sorts in a $facet sub-pipeline, a
// $lookup sub-pipeline, $setWindowFields sortBy, and an update $push $sort,
// plus a single-key sort that needs no review.
const legacyNestedContent = `{"queries":[
 {"name":"agg","collection":"c","operation":"aggregate","pipeline":[
  {"$facet":{"top":[{"$sort":{"p":-1,"q":1}}],"one":[{"$sort":{"z":1}}]}},
  {"$lookup":{"from":"d","as":"j","pipeline":[{"$sort":{"m":1,"n":1}}]}},
  {"$setWindowFields":{"sortBy":{"t":1,"u":-1},"output":{"r":{"$rank":{}}}}}]},
 {"name":"push","collection":"c","operation":"updateOne","filter":{"_id":1},"update":{"$push":{"items":{"$each":[{"k":1}],"$sort":{"k":1,"w":-1}}}}}]}`

func writeStore(t *testing.T, defs ...Definition) (string, *FileStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "definitions.json")
	b, _ := json.Marshal(persistedData{Definitions: defs})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, store
}

func legacyQueryDef(id, name, content string) Definition {
	return Definition{ID: id, Type: KindQuery, Name: name, Format: "json", Content: content, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z"}
}

func confirmAll(issues []SortOrderIssue) []ReviewedSort {
	out := make([]ReviewedSort, len(issues))
	for i, is := range issues {
		out[i] = ReviewedSort{Path: is.Path, SortSpec: is.SortSpec}
	}
	return out
}

func checkStored(t *testing.T, def Definition) error {
	t.Helper()
	parsed, err := config.ParseQueriesBytes([]byte(def.Content))
	if err != nil {
		t.Fatal(err)
	}
	return def.CheckSortOrder(parsed.Queries)
}

func TestLegacyDefinitionWithMultiKeySortNeedsReview(t *testing.T) {
	current := legacyQueryDef("current", "current multi", strings.Replace(legacyMultiContent, `"q1"`, `"q2"`, 1))
	current.ContentVersion = ContentVersionOrderedSorts
	_, store := writeStore(t,
		legacyQueryDef("multi", "legacy multi", legacyMultiContent),
		legacyQueryDef("single", "legacy single", `{"queries":[{"name":"q1","collection":"c","operation":"aggregate","pipeline":[{"$sort":{"a":1}}]}]}`),
		current,
		Definition{ID: "cols", Type: KindCollection, Name: "cols", Format: "json", Content: `{"collections":[{"database":"d","collection":"c","fields":{}}]}`},
	)

	multi, _ := store.Get(KindQuery, "multi")
	want := []SortOrderIssue{{QueryIndex: 0, QueryName: "q1", Path: "queries[0].pipeline[1].$sort", StoredKeys: []string{"a", "b"}, SortSpec: `{"a":1,"b":-1}`}}
	if multi.SortOrderReview == nil || multi.SortOrderReview.Status != SortOrderReviewRequired || !reflect.DeepEqual(multi.SortOrderReview.Issues, want) {
		t.Fatalf("legacy multi-key sort review = %+v", multi.SortOrderReview)
	}
	if !strings.Contains(multi.SortOrderReview.Message, "may have stored multi-key sort keys alphabetically") {
		t.Fatalf("review must explain the alphabetization: %q", multi.SortOrderReview.Message)
	}
	err := checkStored(t, multi)
	var reviewErr *SortOrderReviewError
	if !errors.As(err, &reviewErr) || !reflect.DeepEqual(reviewErr.Review, *multi.SortOrderReview) {
		t.Fatalf("run-start check must return the same structured review, got %v", err)
	}

	for _, id := range []string{"single", "current"} {
		def, _ := store.Get(KindQuery, id)
		if def.SortOrderReview != nil || checkStored(t, def) != nil {
			t.Fatalf("%s must not need review: %+v", id, def.SortOrderReview)
		}
	}
	if cols, _ := store.Get(KindCollection, "cols"); cols.SortOrderReview != nil {
		t.Fatal("collection definitions never need sort review")
	}

	listed, _ := store.List(KindQuery)
	for _, d := range listed {
		if d.Content != "" {
			t.Fatal("list must not return content")
		}
		if (d.ID == "multi") != (d.SortOrderReview != nil) {
			t.Fatalf("list review flag for %s = %+v", d.ID, d.SortOrderReview)
		}
	}
}

func TestLegacyNestedSortsAreAllReported(t *testing.T) {
	_, store := writeStore(t, legacyQueryDef("nested", "nested", legacyNestedContent))
	def, _ := store.Get(KindQuery, "nested")
	if def.SortOrderReview == nil {
		t.Fatal("nested multi-key sorts must need review")
	}
	var got []string
	for _, is := range def.SortOrderReview.Issues {
		got = append(got, is.Path+"="+strings.Join(is.StoredKeys, ","))
	}
	want := []string{
		"queries[0].pipeline[0].$facet.top[0].$sort=p,q",
		"queries[0].pipeline[1].$lookup.pipeline[0].$sort=m,n",
		"queries[0].pipeline[2].$setWindowFields.sortBy=t,u",
		"queries[1].update.$push.items.$sort=k,w",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested issues = %v, want %v", got, want)
	}
}

func TestLegacyUpdateRequiresExplicitReview(t *testing.T) {
	path, store := writeStore(t, legacyQueryDef("multi", "legacy multi", legacyMultiContent))
	before, _ := os.ReadFile(path)
	def, _ := store.Get(KindQuery, "multi")
	sortPath := def.SortOrderReview.Issues[0].Path

	for name, reviewed := range map[string][]ReviewedSort{"plain re-save": nil, "empty list": {}} {
		_, err := store.Update(KindQuery, "multi", Input{Name: def.Name, Description: "edited", Content: def.Content, ReviewedSorts: reviewed})
		var reviewErr *SortOrderReviewError
		if !errors.As(err, &reviewErr) || len(reviewErr.Review.Issues) != 1 || reviewErr.Review.Issues[0].Path != sortPath ||
			!strings.Contains(reviewErr.Message, "saving does not confirm the order by itself") {
			t.Fatalf("%s: want a structured review error, got %v", name, err)
		}
	}
	spec := def.SortOrderReview.Issues[0].SortSpec
	for name, reviewed := range map[string][]ReviewedSort{
		"unknown path":    {{sortPath, spec}, {"queries[0].pipeline[0].$sort", spec}},
		"misspelled path": {{"queries[0].pipeline[1].$Sort", spec}},
	} {
		_, err := store.Update(KindQuery, "multi", Input{Name: def.Name, Description: "edited", Content: def.Content, ReviewedSorts: reviewed})
		if err == nil || !strings.Contains(err.Error(), "is not a multi-key sort in the submitted content") {
			t.Fatalf("%s: want a stale-review error, got %v", name, err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejected saves must not change the store")
	}
	if got, _ := store.Get(KindQuery, "multi"); got.SortOrderReview == nil || got.Description != "" {
		t.Fatalf("definition must stay unresolved and unchanged: %+v", got)
	}
}

func TestLegacyPartialReviewIsRejectedWithFullChecklist(t *testing.T) {
	_, store := writeStore(t, legacyQueryDef("nested", "nested", legacyNestedContent))
	def, _ := store.Get(KindQuery, "nested")
	_, err := store.Update(KindQuery, "nested", Input{Name: def.Name, Content: def.Content, ReviewedSorts: confirmAll(def.SortOrderReview.Issues[:1])})
	var reviewErr *SortOrderReviewError
	if !errors.As(err, &reviewErr) {
		t.Fatalf("partial review must be rejected, got %v", err)
	}
	if len(reviewErr.Review.Issues) != 4 || strings.Contains(reviewErr.Message, def.SortOrderReview.Issues[0].Path) {
		t.Fatalf("error must list the full checklist and name only unconfirmed sorts: %+v", reviewErr)
	}
}

func TestLegacyReviewedSaveCorrectsAndReloadsInOrder(t *testing.T) {
	path, store := writeStore(t,
		legacyQueryDef("multi", "legacy multi", legacyMultiContent),
		legacyQueryDef("keep", "legacy keep", strings.Replace(legacyMultiContent, `"q1"`, `"q3"`, 1)),
	)
	def, _ := store.Get(KindQuery, "multi")
	corrected := strings.Replace(def.Content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1)
	saved, err := store.Update(KindQuery, "multi", Input{Name: def.Name, Description: "reviewed", Content: corrected, ReviewedSorts: []ReviewedSort{{"queries[0].pipeline[1].$sort", `{"b":-1,"a":1}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ContentVersion != ContentVersionOrderedSorts || saved.SortOrderReview != nil || saved.CreatedAt != def.CreatedAt {
		t.Fatalf("reviewed save = %+v", saved)
	}

	// The explicitly confirmed stored order is also accepted as-is.
	keep, _ := store.Get(KindQuery, "keep")
	if _, err := store.Update(KindQuery, "keep", Input{Name: keep.Name, Content: keep.Content, ReviewedSorts: confirmAll(keep.SortOrderReview.Issues)}); err != nil {
		t.Fatalf("confirming the stored order must save: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sortOrderReview") {
		t.Fatal("the computed review must never be persisted")
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.Get(KindQuery, "multi")
	if sortSnippet(t, got.Content) != `[{"$match":{"x":1}},{"$sort":{"b":-1,"a":1}}]` || got.Description != "reviewed" {
		t.Fatalf("reloaded reviewed definition = %s %+v", sortSnippet(t, got.Content), got)
	}
	if got.SortOrderReview != nil || checkStored(t, got) != nil {
		t.Fatal("a reviewed definition must run")
	}
	if k, _ := reopened.Get(KindQuery, "keep"); sortSnippet(t, k.Content) != `[{"$match":{"x":1}},{"$sort":{"a":1,"b":-1}}]` || checkStored(t, k) != nil {
		t.Fatalf("confirmed stored order must reload unchanged: %s", sortSnippet(t, k.Content))
	}
}

func TestCurrentDefinitionUpdateNeedsNoReview(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(KindQuery, Input{Name: "sorted", Content: orderedSortQueries})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(KindQuery, created.ID, Input{Name: "sorted", Description: "d", Content: orderedSortQueries}); err != nil {
		t.Fatalf("current definitions update without review: %v", err)
	}
}

func copyOf(src Definition, name, content string, reviewed []ReviewedSort) Input {
	return Input{Name: name, Content: content, ReviewedSorts: reviewed,
		SourceDefinitionID: src.ID, SourceUpdatedAt: src.UpdatedAt, SourceContentVersion: src.ContentVersion}
}

func TestCopyOfLegacyDefinitionRequiresBoundReview(t *testing.T) {
	path, store := writeStore(t, legacyQueryDef("nested", "nested", legacyNestedContent))
	src, _ := store.Get(KindQuery, "nested")
	issues := src.SortOrderReview.Issues
	before, _ := os.ReadFile(path)

	for name, reviewed := range map[string][]ReviewedSort{"none": nil, "incomplete": confirmAll(issues[:3])} {
		_, err := store.Create(KindQuery, copyOf(src, "copy", src.Content, reviewed))
		var reviewErr *SortOrderReviewError
		if !errors.As(err, &reviewErr) || len(reviewErr.Review.Issues) != 4 || !strings.Contains(reviewErr.Message, "before a copy of it can be saved") {
			t.Fatalf("%s: want the full checklist, got %v", name, err)
		}
	}
	unknown := append(confirmAll(issues), ReviewedSort{Path: "queries[0].pipeline[9].$sort", SortSpec: `{"a":1}`})
	if _, err := store.Create(KindQuery, copyOf(src, "copy", src.Content, unknown)); err == nil || !strings.Contains(err.Error(), "is not a multi-key sort in the submitted content") {
		t.Fatalf("unknown path: got %v", err)
	}

	// Confirmations given for the loaded order are stale once the order is edited.
	edited := strings.Replace(src.Content, `{"p":-1,"q":1}`, `{"q":1,"p":-1}`, 1)
	_, err := store.Create(KindQuery, copyOf(src, "copy", edited, confirmAll(issues)))
	var reviewErr *SortOrderReviewError
	if !errors.As(err, &reviewErr) || !strings.Contains(reviewErr.Message, `queries[0].pipeline[0].$facet.top[0].$sort was confirmed as {"p":-1,"q":1} but is {"q":1,"p":-1}`) ||
		reviewErr.Review.Issues[0].SortSpec != `{"q":1,"p":-1}` {
		t.Fatalf("stale confirmation must be rejected with the submitted order, got %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("rejected copies must not change the store")
	}

	// Confirming the submitted (edited) order saves a current-version copy in that order.
	created, err := store.Create(KindQuery, copyOf(src, "copy", edited, confirmAll(reviewErr.Review.Issues)))
	if err != nil {
		t.Fatalf("reviewed copy must save: %v", err)
	}
	got, _ := store.Get(KindQuery, created.ID)
	if got.ContentVersion != ContentVersionOrderedSorts || got.SortOrderReview != nil || !strings.Contains(sortSnippet(t, got.Content), `{"$sort":{"q":1,"p":-1}}`) {
		t.Fatalf("reviewed copy = %+v", got)
	}
	if src2, _ := store.Get(KindQuery, "nested"); src2.SortOrderReview == nil {
		t.Fatal("copying must not resolve the source")
	}
}

func TestCopySourceIdentityAndVersionAreValidated(t *testing.T) {
	_, store := writeStore(t, legacyQueryDef("multi", "legacy multi", legacyMultiContent))
	src, _ := store.Get(KindQuery, "multi")
	content := strings.Replace(legacyMultiContent, `"q1"`, `"q9"`, 1)

	stale := copyOf(src, "copy", content, nil)
	stale.SourceUpdatedAt = "2024-12-31T00:00:00Z"
	upgraded := copyOf(src, "copy", content, nil)
	upgraded.SourceContentVersion = ContentVersionOrderedSorts
	gone := copyOf(src, "copy", content, nil)
	gone.SourceDefinitionID = "deleted"
	for name, in := range map[string]Input{"stale updatedAt": stale, "other content version": upgraded, "deleted source": gone} {
		if _, err := store.Create(KindQuery, in); !errors.Is(err, ErrSourceDefinitionChanged) {
			t.Fatalf("%s: want ErrSourceDefinitionChanged, got %v", name, err)
		}
	}
	noVersion := copyOf(src, "copy", content, nil)
	noVersion.SourceUpdatedAt = ""
	if _, err := store.Create(KindQuery, noVersion); err == nil || !strings.Contains(err.Error(), "sourceUpdatedAt is required") {
		t.Fatalf("missing version: got %v", err)
	}
	if _, err := store.Create(KindQuery, Input{Name: "copy", Content: content, SourceUpdatedAt: src.UpdatedAt}); err == nil || !strings.Contains(err.Error(), "require sourceDefinitionId") {
		t.Fatalf("version without id: got %v", err)
	}
	if listed, _ := store.List(KindQuery); len(listed) != 1 {
		t.Fatalf("rejected copies must not be stored: %d definitions", len(listed))
	}
}

func TestNewDefinitionsAndCopiesOfCurrentDefinitionsNeedNoReview(t *testing.T) {
	current := legacyQueryDef("current", "current", legacyMultiContent)
	current.ContentVersion = ContentVersionOrderedSorts
	_, store := writeStore(t, current)

	// Content without a declared source is new; its declared compound order is kept.
	created, err := store.Create(KindQuery, Input{Name: "new", Content: orderedSortQueries})
	if err != nil || created.SortOrderReview != nil || sortSnippet(t, created.Content) != `[{"$sort":{"b":-1,"a":1}}]` {
		t.Fatalf("new definition = %+v, %v", created, err)
	}
	src, _ := store.Get(KindQuery, "current")
	if _, err := store.Create(KindQuery, copyOf(src, "copy", strings.Replace(legacyMultiContent, `"q1"`, `"q2"`, 1), nil)); err != nil {
		t.Fatalf("copy of a current definition must save without review: %v", err)
	}
}

func TestInputRejectsUnboundPathConfirmations(t *testing.T) {
	var in Input
	err := json.Unmarshal([]byte(`{"name":"x","content":"{}","reviewedSortPaths":["queries[0].pipeline[0].$sort"]}`), &in)
	if err == nil || !strings.Contains(err.Error(), "send reviewedSorts") {
		t.Fatalf("path-only confirmations must be rejected, got %v", err)
	}
	if err := json.Unmarshal([]byte(`{"name":"x","content":"{}","reviewedSortPaths":null}`), &in); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyUpdateRejectsStaleConfirmation(t *testing.T) {
	_, store := writeStore(t, legacyQueryDef("multi", "legacy multi", legacyMultiContent))
	def, _ := store.Get(KindQuery, "multi")
	corrected := strings.Replace(def.Content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1)
	_, err := store.Update(KindQuery, "multi", Input{Name: def.Name, Content: corrected, ReviewedSorts: confirmAll(def.SortOrderReview.Issues)})
	var reviewErr *SortOrderReviewError
	if !errors.As(err, &reviewErr) || !strings.Contains(reviewErr.Message, "stale") {
		t.Fatalf("a confirmation of the stored order must not confirm the edited order, got %v", err)
	}
	flipped := strings.Replace(def.Content, `{"a":1,"b":-1}`, `{"a":-1,"b":-1}`, 1)
	if _, err := store.Update(KindQuery, "multi", Input{Name: def.Name, Content: flipped, ReviewedSorts: confirmAll(def.SortOrderReview.Issues)}); !errors.As(err, &reviewErr) {
		t.Fatalf("a direction change must invalidate the confirmation, got %v", err)
	}
}
