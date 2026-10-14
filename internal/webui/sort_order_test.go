package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/definitions"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func loadedAggregateSample(t *testing.T, pipeline string) stats.RepresentativeSample {
	t.Helper()
	qf, err := config.ParseQueriesBytes([]byte(`[{"name":"q","collection":"orders","operation":"aggregate","pipeline":` + pipeline + `}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NormalizeAndValidateQueries(qf.Queries); err != nil {
		t.Fatal(err)
	}
	return stats.RepresentativeSample{Operation: "aggregate", Database: "shop", Collection: "orders",
		Query: &stats.ExecutableQuery{Database: "shop", Pipeline: qf.Queries[0].Pipeline}}
}

func TestExplainCommandFromLoadedDefinitionKeepsSortOrder(t *testing.T) {
	smp := loadedAggregateSample(t, `[{"$sort":{"b":-1,"a":1}},{"$setWindowFields":{"sortBy":{"t":1,"s":-1},"output":{"r":{"$rank":{}}}}}]`)
	spec, reason := buildExplainCommandSpec(smp, 1000, "executionStats")
	if reason != "" {
		t.Fatalf("loaded multi-key sort must be explainable, got %q", reason)
	}
	for i := 0; i < 50; i++ {
		b, err := bson.MarshalExtJSON(spec.cmd, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"$sort":{"b":-1,"a":1}`) || !strings.Contains(string(b), `"sortBy":{"t":1,"s":-1}`) {
			t.Fatalf("explain command must keep the executed sort order, got %s", b)
		}
	}
}

func TestExplainDedupeKeySeparatesSortPrecedence(t *testing.T) {
	key := func(p string) string { return explainDedupeKey(loadedAggregateSample(t, p), 1000, "executionStats") }
	ab := key(`[{"$sort":{"a":1,"b":-1}}]`)
	if ab != key(`[{"$sort":{"a":1,"b":-1}}]`) {
		t.Fatal("identical sorts must dedupe to one command")
	}
	if ab == key(`[{"$sort":{"b":-1,"a":1}}]`) {
		t.Fatal("different sort precedence must be separate explain commands")
	}
}

func TestHandleStartUploadedQueriesKeepSortOrder(t *testing.T) {
	runCalled := make(chan []config.QueryDefinition, 1)
	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			runCalled <- queries
			return nil
		},
	})
	s := newServerWithTempDefinitions(t)
	s.AppConfig.Duration = "1s"
	s.AppConfig.Iterations = 1
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "false"}, map[string]string{
		"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
		"queries_file":     `{"queries":[{"name":"by_b_a","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"b":-1,"a":1}}]}]}`,
	})
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	queries := <-runCalled
	got := queries[0].Pipeline[0].(map[string]interface{})["$sort"]
	if want := (bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("uploaded sort = %#v", got)
	}
}

func newLegacySortServer(t *testing.T) (*WebServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "definitions.json")
	legacy := map[string]interface{}{"definitions": []definitions.Definition{
		{ID: "cols", Type: definitions.KindCollection, Name: "cols", Format: "json", ContentVersion: definitions.ContentVersionOrderedSorts,
			Content: `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`},
		{ID: "legacy", Type: definitions.KindQuery, Name: "legacy sorts", Format: "json", CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-02T00:00:00Z",
			Content: `{"queries":[{"name":"by_a_b","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"a":1,"b":-1}}]}]}`},
		{ID: "current", Type: definitions.KindQuery, Name: "current sorts", Format: "json", ContentVersion: definitions.ContentVersionOrderedSorts,
			CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z",
			Content: `{"queries":[{"name":"by_b_a","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"b":-1,"a":1}}]}]}`},
	}}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := definitions.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newServerWithTempDefinitions(t)
	s.DefinitionStore = store
	s.AppConfig.Duration = "1s"
	s.AppConfig.Iterations = 1
	return s, path
}

func startLegacySelection(t *testing.T, s *WebServer) *httptest.ResponseRecorder {
	t.Helper()
	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":         "false",
		"collection_definition_id": "cols",
		"query_definition_id":      "legacy",
	}, nil)
	s.handleStart(rec, req)
	return rec
}

func putQueryDefinition(t *testing.T, s *WebServer, id string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/query-definitions/"+id, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	return rec
}

type sortReviewErrorBody struct {
	Status          string                      `json:"status"`
	Code            string                      `json:"code"`
	Message         string                      `json:"message"`
	SortOrderReview definitions.SortOrderReview `json:"sortOrderReview"`
}

func decodeReviewError(t *testing.T, rec *httptest.ResponseRecorder) sortReviewErrorBody {
	t.Helper()
	return decodeReviewErrorKeys(t, rec, "a", "b")
}

func decodeReviewErrorKeys(t *testing.T, rec *httptest.ResponseRecorder, keys ...string) sortReviewErrorBody {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", rec.Code, rec.Body.String())
	}
	var body sortReviewErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "error" || body.Code != definitions.SortOrderReviewErrorCode || len(body.SortOrderReview.Issues) != 1 ||
		body.SortOrderReview.Issues[0].Path != "queries[0].pipeline[0].$sort" || !reflect.DeepEqual(body.SortOrderReview.Issues[0].StoredKeys, keys) {
		t.Fatalf("structured review error = %+v", body)
	}
	return body
}

func TestHandleStartRejectsLegacyDefinitionWithMultiKeySort(t *testing.T) {
	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			t.Fatalf("an ambiguous definition must not connect")
			return nil, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			t.Fatalf("an ambiguous definition must not run")
			return nil
		},
	})
	s, _ := newLegacySortServer(t)
	body := decodeReviewError(t, startLegacySelection(t, s))
	if !strings.Contains(body.Message, "query 1 (by_a_b) at queries[0].pipeline[0].$sort") {
		t.Fatalf("start error must name the ambiguous sort: %s", body.Message)
	}
	if s.IsRunning {
		t.Fatal("a blocked start must not leave the server running")
	}
}

// TestSortReviewUIFixtureMatchesAPI records the definition API responses
// used by uitest/sort_review.test.mjs.
func TestSortReviewUIFixtureMatchesAPI(t *testing.T) {
	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			t.Fatalf("an ambiguous definition must not connect")
			return nil, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
	})
	s, _ := newLegacySortServer(t)
	decode := func(rec *httptest.ResponseRecorder) interface{} {
		var v interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	get := func(url string, h http.HandlerFunc) interface{} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return decode(rec)
	}
	legacy := get("/api/query-definitions/legacy", s.handleQueryDefinitionItem)
	content := legacy.(map[string]interface{})["content"].(string)
	src := getQueryDefinition(t, s, "legacy")
	copied := strings.Replace(strings.Replace(content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1), "by_a_b", "copy_b_a", 1)
	api := map[string]interface{}{
		"query_list":      get("/api/query-definitions", s.handleQueryDefinitions),
		"collection_list": get("/api/collection-definitions", s.handleCollectionDefinitions),
		"legacy":          legacy,
		"current":         get("/api/query-definitions/current", s.handleQueryDefinitionItem),
		"put_unreviewed":  decode(putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": "legacy sorts", "content": content})),
		"start_blocked":   decode(startLegacySelection(t, s)),
		"post_copy_stale": decode(postQueryDefinition(t, s, copyBody(src, copied,
			[]definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"a":1,"b":-1}`}}))),
		"post_copy_reviewed": decode(postQueryDefinition(t, s, copyBody(src, copied,
			[]definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"b":-1,"a":1}`}}))),
		"put_stale": decode(putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": "legacy sorts",
			"content": strings.Replace(content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1), "reviewedSorts": []definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"a":1,"b":-1}`}}})),
		"put_reviewed": decode(putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": "legacy sorts",
			"content": strings.Replace(content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1), "reviewedSorts": []definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"b":-1,"a":1}`}}})),
	}
	api["put_reviewed"].(map[string]interface{})["updatedAt"] = "normalized"
	for _, k := range []string{"id", "createdAt", "updatedAt"} {
		api["post_copy_reviewed"].(map[string]interface{})[k] = "normalized"
	}
	got, err := json.MarshalIndent(api, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "ui", "definitions_sort_review.json")
	if *updateUIFixtures {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test ./internal/webui -run TestSortReviewUIFixtureMatchesAPI -update-ui-fixtures): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with -update-ui-fixtures", path)
	}
}

func TestLegacySortReviewFlowThroughAPI(t *testing.T) {
	runCalled := make(chan []config.QueryDefinition, 1)
	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			runCalled <- queries
			return nil
		},
	})
	s, path := newLegacySortServer(t)

	// GET and list carry the preflight review.
	rec := httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, httptest.NewRequest(http.MethodGet, "/api/query-definitions/legacy", nil))
	var got definitions.Definition
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.SortOrderReview == nil || got.SortOrderReview.Issues[0].Path != "queries[0].pipeline[0].$sort" {
		t.Fatalf("GET must include the review: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleQueryDefinitions(rec, httptest.NewRequest(http.MethodGet, "/api/query-definitions", nil))
	if !strings.Contains(rec.Body.String(), `"sortOrderReview"`) {
		t.Fatalf("list must flag the definition: %s", rec.Body.String())
	}

	// Bypass attempts: a plain re-save, a forged path, and an object body.
	before, _ := os.ReadFile(path)
	decodeReviewError(t, putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": got.Name, "content": got.Content}))
	if rec := putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": got.Name, "content": got.Content, "reviewedSorts": []definitions.ReviewedSort{{Path: "queries[0].pipeline[1].$sort", SortSpec: `{"a":1,"b":-1}`}}}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "is not a multi-key sort") {
		t.Fatalf("forged path must be rejected, got %d %s", rec.Code, rec.Body.String())
	}
	decodeReviewError(t, putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": got.Name,
		"content": json.RawMessage(`{"queries":[{"name":"by_a_b","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"a":1,"b":-1}}]}]}`)}))
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("rejected saves must not change the store")
	}
	decodeReviewError(t, startLegacySelection(t, s))

	// Reviewed and corrected save, then the run uses the confirmed order.
	corrected := strings.Replace(got.Content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1)
	rec = putQueryDefinition(t, s, "legacy", map[string]interface{}{"name": got.Name, "content": corrected, "reviewedSorts": []definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"b":-1,"a":1}`}}})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sortOrderReview") {
		t.Fatalf("reviewed save must succeed, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := startLegacySelection(t, s); rec.Code != http.StatusOK {
		t.Fatalf("reviewed definition must start, got %d %s", rec.Code, rec.Body.String())
	}
	queries := <-runCalled
	sortSpec := queries[0].Pipeline[0].(map[string]interface{})["$sort"]
	if want := (bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}); !reflect.DeepEqual(sortSpec, want) {
		t.Fatalf("run sort = %#v", sortSpec)
	}
	s.runWG.Wait()
}

func postQueryDefinition(t *testing.T, s *WebServer, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	s.handleQueryDefinitions(rec, httptest.NewRequest(http.MethodPost, "/api/query-definitions", bytes.NewReader(b)))
	return rec
}

func getQueryDefinition(t *testing.T, s *WebServer, id string) definitions.Definition {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, httptest.NewRequest(http.MethodGet, "/api/query-definitions/"+id, nil))
	var def definitions.Definition
	if err := json.Unmarshal(rec.Body.Bytes(), &def); err != nil {
		t.Fatal(err)
	}
	return def
}

func copyBody(src definitions.Definition, content string, reviewed []definitions.ReviewedSort) map[string]interface{} {
	return map[string]interface{}{"name": "legacy copy", "content": content, "reviewedSorts": reviewed,
		"sourceDefinitionId": src.ID, "sourceUpdatedAt": src.UpdatedAt, "sourceContentVersion": src.ContentVersion}
}

// TestSaveAsNewOfLegacyDefinitionIsReviewedByBackend drives the Save As New
// request directly, as a client that skips the UI checks would.
func TestSaveAsNewOfLegacyDefinitionIsReviewedByBackend(t *testing.T) {
	runCalled := make(chan []config.QueryDefinition, 1)
	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			runCalled <- queries
			return nil
		},
	})
	s, path := newLegacySortServer(t)
	src := getQueryDefinition(t, s, "legacy")
	before, _ := os.ReadFile(path)
	stored := []definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"a":1,"b":-1}`}}
	corrected := strings.Replace(src.Content, `{"a":1,"b":-1}`, `{"b":-1,"a":1}`, 1)

	decodeReviewError(t, postQueryDefinition(t, s, copyBody(src, src.Content, nil)))
	body := decodeReviewErrorKeys(t, postQueryDefinition(t, s, copyBody(src, corrected, stored)), "b", "a")
	if !strings.Contains(body.Message, "stale") || body.SortOrderReview.Issues[0].SortSpec != `{"b":-1,"a":1}` {
		t.Fatalf("a confirmation of the loaded order must not confirm the edited copy: %+v", body)
	}
	if rec := postQueryDefinition(t, s, copyBody(src, src.Content, []definitions.ReviewedSort{{Path: "queries[0].pipeline[3].$sort", SortSpec: "{}"}})); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown path must be a 400, got %d %s", rec.Code, rec.Body.String())
	}
	staleSource := copyBody(src, corrected, nil)
	staleSource["sourceUpdatedAt"] = "2024-01-01T00:00:00Z"
	if rec := postQueryDefinition(t, s, staleSource); rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "sort_order_review_required") ||
		!strings.Contains(rec.Body.String(), "copy it again") {
		t.Fatalf("stale source version must be a 409, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := postQueryDefinition(t, s, map[string]interface{}{"name": "old client", "content": corrected, "reviewedSortPaths": []string{"queries[0].pipeline[0].$sort"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("path-only confirmations must be a 400, got %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("rejected copies must not change the store")
	}

	decodeReviewErrorKeys(t, postQueryDefinition(t, s, copyBody(src, corrected, []definitions.ReviewedSort{})), "b", "a")
	rec := postQueryDefinition(t, s, copyBody(src, corrected, []definitions.ReviewedSort{{Path: "queries[0].pipeline[0].$sort", SortSpec: `{"b":-1,"a":1}`}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reviewed copy must be created, got %d %s", rec.Code, rec.Body.String())
	}
	var created definitions.Definition
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if reloaded := getQueryDefinition(t, s, created.ID); reloaded.SortOrderReview != nil || reloaded.ContentVersion != definitions.ContentVersionOrderedSorts {
		t.Fatalf("reloaded copy = %+v", reloaded)
	}
	if getQueryDefinition(t, s, "legacy").SortOrderReview == nil {
		t.Fatal("the source stays unresolved")
	}

	req, startRec := newMultipartRequest(t, map[string]string{"default_workload": "false", "collection_definition_id": "cols", "query_definition_id": created.ID}, nil)
	s.handleStart(startRec, req)
	if startRec.Code != http.StatusOK {
		t.Fatalf("reviewed copy must start, got %d %s", startRec.Code, startRec.Body.String())
	}
	queries := <-runCalled
	if got := queries[0].Pipeline[0].(map[string]interface{})["$sort"]; !reflect.DeepEqual(got, bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}) {
		t.Fatalf("run sort = %#v", got)
	}
	s.runWG.Wait()
}

// TestNewDefinitionsWithoutKnownProvenanceKeepDeclaredOrder covers content
// whose origin is not declared: POST without a source, uploads, and copies of
// current or built-in definitions. None is asked for review.
func TestNewDefinitionsWithoutKnownProvenanceKeepDeclaredOrder(t *testing.T) {
	s, _ := newLegacySortServer(t)
	content := `{"queries":[{"name":"new_b_a","collection":"orders","operation":"aggregate","pipeline":[{"$sort":{"b":-1,"a":1}},{"$setWindowFields":{"sortBy":{"t":1,"s":-1},"output":{"r":{"$rank":{}}}}}]}]}`
	rec := postQueryDefinition(t, s, map[string]interface{}{"name": "new", "content": content})
	if rec.Code != http.StatusCreated {
		t.Fatalf("new definition: %d %s", rec.Code, rec.Body.String())
	}
	var created definitions.Definition
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	got := getQueryDefinition(t, s, created.ID)
	if got.SortOrderReview != nil || !strings.Contains(got.Content, `"$sort": {`+"\n") {
		t.Fatalf("new definition = %+v", got)
	}
	parsed, _ := config.ParseQueriesBytes([]byte(got.Content))
	if locs := config.MultiKeySortSpecs(parsed.Queries[0].Pipeline, ""); len(locs) != 2 || locs[0].Spec != `{"b":-1,"a":1}` || locs[1].Spec != `{"t":1,"s":-1}` {
		t.Fatalf("declared order must be kept: %+v", locs)
	}

	up, upRec := newDefinitionUploadRequest(t, "/api/query-definitions/upload", "file", "pasted.json", strings.Replace(content, "new_b_a", "uploaded", 1))
	s.handleQueryDefinitionItem(upRec, up)
	if upRec.Code != http.StatusCreated || strings.Contains(upRec.Body.String(), "sortOrderReview") {
		t.Fatalf("upload: %d %s", upRec.Code, upRec.Body.String())
	}

	current := getQueryDefinition(t, s, "current")
	if rec := postQueryDefinition(t, s, copyBody(current, strings.Replace(current.Content, "by_b_a", "copy_b_a", 1), nil)); rec.Code != http.StatusCreated {
		t.Fatalf("copy of a current definition: %d %s", rec.Code, rec.Body.String())
	}
	builtin := getQueryDefinition(t, s, builtinQueryDefinitionID)
	builtinCopy := copyBody(builtin, strings.Replace(content, "new_b_a", "builtin_copy", 1), nil)
	builtinCopy["name"] = "builtin copy"
	if rec := postQueryDefinition(t, s, builtinCopy); rec.Code != http.StatusCreated {
		t.Fatalf("copy of the built-in definition: %d %s", rec.Code, rec.Body.String())
	}
	staleBuiltin := copyBody(builtin, strings.Replace(content, "new_b_a", "b2", 1), nil)
	staleBuiltin["sourceUpdatedAt"] = "2000-01-01T00:00:00Z"
	if rec := postQueryDefinition(t, s, staleBuiltin); rec.Code != http.StatusConflict {
		t.Fatalf("stale built-in version must be a 409, got %d %s", rec.Code, rec.Body.String())
	}
}
