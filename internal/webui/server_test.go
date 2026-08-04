package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/definitions"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestParseInt(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		defaultVal int
		want       int
	}{
		{name: "empty_uses_default", in: "", defaultVal: 10, want: 10},
		{name: "invalid_uses_default", in: "abc", defaultVal: 7, want: 7},
		{name: "valid_number", in: "42", defaultVal: 1, want: 42},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseInt(tc.in, tc.defaultVal); got != tc.want {
				t.Fatalf("parseInt(%q, %d) = %d, want %d", tc.in, tc.defaultVal, got, tc.want)
			}
		})
	}
}

func TestHandleReportRendersHTML(t *testing.T) {
	s := NewServer(&config.AppConfig{
		URI:         "mongodb://user:secret@localhost:27017",
		Duration:    "30s",
		Concurrency: 8,
		FindPercent: 100,
	})
	s.CurrentStats = stats.NewCollector()
	s.CurrentStats.Track("find", 3*time.Millisecond)
	s.CurrentStats.Track("find", 9*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/api/report", nil)
	rec := httptest.NewRecorder()
	s.handleReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "MongoDB Benchmark Report") {
		t.Fatalf("expected report title in HTML")
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("expected password to be masked in report URI")
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected HTML content type, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestHandleReportDownloadSetsAttachment(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "10s"})
	req := httptest.NewRequest(http.MethodGet, "/api/report?download=1", nil)
	rec := httptest.NewRecorder()
	s.handleReport(rec, req)
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("expected attachment disposition, got %q", cd)
	}
}

func TestHandleInferSchemaJSON(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	body := `{"log":"{\"attr\":{\"ns\":\"shop.orders\",\"command\":{\"find\":\"orders\",\"filter\":{\"status\":\"open\"}}}}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/infer-schema", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleInferSchema(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ParsedEntries int `json:"parsedEntries"`
		Operations    []struct {
			Namespace string `json:"namespace"`
			Operation string `json:"operation"`
		} `json:"operations"`
		SuggestedMix struct {
			FindPercent int `json:"findPercent"`
		} `json:"suggestedMix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ParsedEntries != 1 {
		t.Fatalf("expected 1 parsed entry, got %d", resp.ParsedEntries)
	}
	if len(resp.Operations) == 0 || resp.Operations[0].Namespace != "shop.orders" {
		t.Fatalf("unexpected operations: %+v", resp.Operations)
	}
	if resp.SuggestedMix.FindPercent != 100 {
		t.Fatalf("expected find mix 100, got %d", resp.SuggestedMix.FindPercent)
	}
}

func TestHandleInferSchemaEmptyBodyRejected(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	req := httptest.NewRequest(http.MethodPost, "/api/infer-schema", strings.NewReader("   "))
	rec := httptest.NewRecorder()
	s.handleInferSchema(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d", rec.Code)
	}
}

func TestHandleInferSchemaRejectsGet(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/infer-schema", nil)
	rec := httptest.NewRecorder()
	s.handleInferSchema(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleGetConfigMasksPassword(t *testing.T) {
	s := NewServer(&config.AppConfig{
		URI: "mongodb://localhost:27017",
		ConnectionParams: config.ConnectionParams{
			Username: "user",
			Password: "secret",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	s.handleGetConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var got config.AppConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ConnectionParams.Password != "" {
		t.Fatalf("expected masked password, got %q", got.ConnectionParams.Password)
	}
	if got.ConnectionParams.Username != "user" {
		t.Fatalf("expected username preserved, got %q", got.ConnectionParams.Username)
	}
}

func TestDefinitionAPIListCreateGetUpdateDelete(t *testing.T) {
	s := newServerWithTempDefinitions(t)
	body := strings.NewReader(`{"name":"orders queries","description":"demo","content":"{\"queries\":[{\"name\":\"find_orders\",\"database\":\"shop\",\"collection\":\"orders\",\"operation\":\"find\",\"filter\":{}}]}"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query-definitions", body)
	rec := httptest.NewRecorder()

	s.handleQueryDefinitions(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created definitions.Definition
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created definition: %v", err)
	}
	if created.ID == "" || created.Type != definitions.KindQuery || !strings.Contains(created.Content, `"queries"`) {
		t.Fatalf("unexpected created definition: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/query-definitions", nil)
	rec = httptest.NewRecorder()
	s.handleQueryDefinitions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	listPayload := decodeJSONMap(t, rec.Body.Bytes())
	defs, ok := listPayload["definitions"].([]interface{})
	if !ok || len(defs) != 2 {
		t.Fatalf("expected built-in plus one saved definition, got %+v", listPayload)
	}
	if _, hasContent := defs[0].(map[string]interface{})["content"]; hasContent {
		t.Fatalf("list payload should omit definition content")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/query-definitions/"+created.ID, nil)
	rec = httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/query-definitions/"+created.ID, strings.NewReader(`{"name":"orders queries edited","content":"{\"queries\":[{\"name\":\"find_orders\",\"database\":\"shop\",\"collection\":\"orders\",\"operation\":\"find\",\"filter\":{\"status\":\"open\"}}]}"}`))
	rec = httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on update, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/query-definitions/"+created.ID, nil)
	rec = httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDefinitionAPIIncludesBuiltInDefaults(t *testing.T) {
	s := newServerWithTempDefinitions(t)

	req := httptest.NewRequest(http.MethodGet, "/api/query-definitions", nil)
	rec := httptest.NewRecorder()
	s.handleQueryDefinitions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeJSONMap(t, rec.Body.Bytes())
	defs := payload["definitions"].([]interface{})
	first := defs[0].(map[string]interface{})
	if first["id"] != builtinQueryDefinitionID || first["name"] != "Built-in Default Queries" {
		t.Fatalf("expected built-in query definition first, got %+v", first)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/query-definitions/"+builtinQueryDefinitionID, nil)
	rec = httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected built-in get 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSONMap(t, rec.Body.Bytes())
	content, ok := got["content"].(string)
	if !ok || !strings.Contains(content, `"queries"`) {
		t.Fatalf("expected built-in content to include queries, got %+v", got)
	}
	for _, placeholder := range []string{`"<string>"`, `"<int>"`} {
		if !strings.Contains(content, placeholder) {
			t.Fatalf("expected readable datatype placeholder %s in built-in content, got:\n%s", placeholder, content)
		}
	}
	if strings.Contains(content, `\u003c`) {
		t.Fatalf("expected no HTML-escaped datatype placeholders in built-in content, got:\n%s", content)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/query-definitions/"+builtinQueryDefinitionID, nil)
	rec = httptest.NewRecorder()
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot be deleted") {
		t.Fatalf("expected built-in delete rejection, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDefinitionAPIUploadAndValidationFailures(t *testing.T) {
	s := newServerWithTempDefinitions(t)

	req, rec := newDefinitionUploadRequest(t, "/api/collection-definitions/upload", "file", "collections.json", `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`)
	s.handleCollectionDefinitionItem(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected upload 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	req, rec = newDefinitionUploadRequest(t, "/api/collection-definitions/upload", "file", "collections-copy.json", `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`)
	s.handleCollectionDefinitionItem(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "identical content") {
		t.Fatalf("expected duplicate content failure, got %d body=%s", rec.Code, rec.Body.String())
	}

	req, rec = newDefinitionUploadRequest(t, "/api/query-definitions/upload", "file", "bad.json", `{"queries":[{"name":"bad","collection":"orders","filter":{}}]}`)
	s.handleQueryDefinitionItem(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid query operation") {
		t.Fatalf("expected semantic validation failure, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleStartGuardrails(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	t.Run("method_not_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/start", nil)
		rec := httptest.NewRecorder()
		s.handleStart(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", rec.Code)
		}
	})

	t.Run("already_running_conflict", func(t *testing.T) {
		s.IsRunning = true
		defer func() { s.IsRunning = false }()

		req := httptest.NewRequest(http.MethodPost, "/api/start", strings.NewReader(""))
		rec := httptest.NewRecorder()
		s.handleStart(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rec.Code)
		}
	})
}

func TestHandleStartInvalidMultipartPayload(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	req := httptest.NewRequest(http.MethodPost, "/api/start", strings.NewReader("not-a-valid-multipart-body"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=bad")
	rec := httptest.NewRecorder()

	s.handleStart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid multipart payload, got %d", rec.Code)
	}
	if s.IsRunning {
		t.Fatalf("expected run to be aborted and IsRunning reset")
	}
}

func TestHandleStartInvalidCustomCollectionsFile(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("default_workload", "false"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := w.CreateFormFile("collections_file", "collections.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`{"collections":[{"database":"","collection":"","fields":{}}]}`)); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/start", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()

	s.handleStart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid collections file, got %d body=%s", rec.Code, rec.Body.String())
	}
	if s.IsRunning {
		t.Fatalf("expected aborted run to reset IsRunning")
	}
}

func TestHandleStartUploadsTLSClientFilesToTempPaths(t *testing.T) {
	var seenCA string
	var seenCert string

	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			seenCA = cfg.ConnectionParams.TLSCAFile
			seenCert = cfg.ConnectionParams.TLSCertificateKeyFile
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
			return nil
		},
	})

	s := NewServer(&config.AppConfig{URI: "mongodb://localhost:27017", Duration: "1s", Concurrency: 1})
	req, rec := newMultipartRequest(t,
		map[string]string{"default_workload": "false"},
		map[string]string{
			"collections_file":      `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
			"queries_file":          `{"queries":[{"name":"find_orders","database":"shop","collection":"orders","operation":"find","filter":{}}]}`,
			"tls_ca_file":           "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
			"tlsCertificateKeyFile": "-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----\n",
		},
	)

	s.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if seenCA == "" || seenCert == "" {
		t.Fatalf("expected TLS uploads to be persisted to temp files, got ca=%q cert=%q", seenCA, seenCert)
	}
	caBytes, err := os.ReadFile(seenCA)
	if err != nil {
		t.Fatalf("read persisted CA file: %v", err)
	}
	certBytes, err := os.ReadFile(seenCert)
	if err != nil {
		t.Fatalf("read persisted cert file: %v", err)
	}
	if !strings.Contains(string(caBytes), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("expected CA upload contents to be preserved, got %q", string(caBytes))
	}
	if !strings.Contains(string(certBytes), "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("expected cert upload contents to be preserved, got %q", string(certBytes))
	}
}

func TestHandleStartCustomCollectionsWrongKeysReturnsActionableError(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("default_workload", "false"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := w.CreateFormFile("collections_file", "collections.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`[{"databaseName":"shop","collectionName":"orders","fields":{}}]`)); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/start", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()

	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "databaseName")
	msg := decodeJSONMap(t, rec.Body.Bytes())["message"].(string)
	if !strings.Contains(msg, "databaseName") || !strings.Contains(msg, "database") {
		t.Fatalf("expected actionable key-mismatch message, got %q", msg)
	}
}

func TestHandleStartCustomCollectionsInvalidRangeReturnsActionableError(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("default_workload", "false"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := w.CreateFormFile("collections_file", "collections.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`{"collections":[{"database":"shop","collection":"orders","fields":{"amount":{"type":"int","min":100,"max":1}}}]}`)); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/start", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()

	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "invalid min/max")
	msg := decodeJSONMap(t, rec.Body.Bytes())["message"].(string)
	if !strings.Contains(msg, "invalid min/max") {
		t.Fatalf("expected actionable min/max validation message, got %q", msg)
	}
}

func TestHandleStartMalformedUploadsReturnUICompatibleErrorPayload(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	tests := []struct {
		name            string
		files           map[string]string
		wantStatusCode  int
		wantMessageLike string
	}{
		{
			name: "malformed_collections_json",
			files: map[string]string{
				"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}`,
				"queries_file":     `[{"name":"find_orders","collection":"orders","operation":"find","filter":{}}]`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "invalid collections format",
		},
		{
			name: "malformed_queries_json",
			files: map[string]string{
				"collections_file": `[{"database":"shop","collection":"orders","fields":{}}]`,
				"queries_file":     `{"queries":[{"name":"find_orders","collection":"orders","operation":"find","filter":{}}`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "invalid queries format",
		},
		{
			name: "queries_wrong_wrapped_key",
			files: map[string]string{
				"collections_file": `[{"database":"shop","collection":"orders","fields":{}}]`,
				"queries_file":     `{"query":[{"name":"find_orders","collection":"orders","operation":"find","filter":{}}]}`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "queries format",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, rec := newMultipartRequest(t, map[string]string{
				"default_workload": "false",
			}, tc.files)

			s.handleStart(rec, req)
			assertStartErrorResponse(t, rec, tc.wantStatusCode, tc.wantMessageLike)
		})
	}
}

func TestHandleStartSemanticUploadValidationErrorsReturnUICompatiblePayload(t *testing.T) {
	s := NewServer(&config.AppConfig{})

	tests := []struct {
		name            string
		files           map[string]string
		wantStatusCode  int
		wantMessageLike string
	}{
		{
			name: "query_missing_operation",
			files: map[string]string{
				"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
				"queries_file":     `{"queries":[{"name":"bad_query","collection":"orders","filter":{}}]}`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "invalid query operation",
		},
		{
			name: "query_references_unknown_collection",
			files: map[string]string{
				"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
				"queries_file":     `{"queries":[{"name":"find_missing","collection":"customers","operation":"find","filter":{}}]}`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "unknown collection",
		},
		{
			name: "ambiguous_collection_without_database",
			files: map[string]string{
				"collections_file": `{"collections":[{"database":"shop_a","collection":"orders","fields":{}},{"database":"shop_b","collection":"orders","fields":{}}]}`,
				"queries_file":     `{"queries":[{"name":"ambiguous_orders","collection":"orders","operation":"find","filter":{}}]}`,
			},
			wantStatusCode:  http.StatusBadRequest,
			wantMessageLike: "exists in multiple databases",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, rec := newMultipartRequest(t, map[string]string{
				"default_workload": "false",
			}, tc.files)

			s.handleStart(rec, req)
			assertStartErrorResponse(t, rec, tc.wantStatusCode, tc.wantMessageLike)
		})
	}
}

func TestHandleStartRejectsConflictingShardingSettings(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":                     "false",
		"sharding_mode":                        "force_on",
		"sharding_skip_generic_without_config": "true",
	}, map[string]string{
		"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
		"queries_file":     `{"queries":[{"name":"find_orders","collection":"orders","operation":"find","filter":{}}]}`,
	})

	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "sharding_mode=force_on conflicts")
}

func TestHandleStartSuccessWithDuplicateCollectionNamesAcrossDatabasesAndExplicitQueryDatabase(t *testing.T) {
	connectedDBName := ""
	runWorkloadCalled := make(chan []config.QueryDefinition, 1)

	withWebUISeams(t, webuiSeams{
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			connectedDBName = dbName
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
			runWorkloadCalled <- append([]config.QueryDefinition(nil), queries...)
			return nil
		},
	})

	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})
	req, rec := newMultipartRequest(t,
		map[string]string{"default_workload": "false"},
		map[string]string{
			"collections_file": `{"collections":[{"database":"shop_a","collection":"orders","fields":{}},{"database":"shop_b","collection":"orders","fields":{}}]}`,
			"queries_file":     `{"queries":[{"name":"find_orders_a","database":"shop_a","collection":"orders","operation":"find","filter":{}},{"name":"find_orders_b","database":"shop_b","collection":"orders","operation":"find","filter":{}}]}`,
		},
	)
	s.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case queries := <-runWorkloadCalled:
		if len(queries) != 2 {
			t.Fatalf("expected 2 bound queries, got %+v", queries)
		}
		if queries[0].Database == queries[1].Database {
			t.Fatalf("expected queries to remain bound to different databases, got %+v", queries)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}

	if connectedDBName != "shop_a" {
		t.Fatalf("expected initial DB connection from first collection, got %q", connectedDBName)
	}
}

func TestHandleStop(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "5s"})

	t.Run("stop_method_not_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/stop", nil)
		rec := httptest.NewRecorder()
		s.handleStop(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", rec.Code)
		}
	})

	t.Run("stop_cancels_active_run", func(t *testing.T) {
		cancelCalled := false
		s.IsRunning = true
		s.ActiveCancel = func() { cancelCalled = true }

		req := httptest.NewRequest(http.MethodPost, "/api/stop", nil)
		rec := httptest.NewRecorder()
		s.handleStop(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if !cancelCalled {
			t.Fatalf("expected ActiveCancel to be called")
		}
		if s.IsRunning {
			t.Fatalf("expected IsRunning=false after stop")
		}
	})
}

func TestHandleStats(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "5s"})

	t.Run("without_collector", func(t *testing.T) {
		s.CurrentStats = nil
		s.IsRunning = true
		s.LifecyclePhase = "initializing"
		s.LifecycleMessage = "Preparing workload resources"
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		s.handleStats(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		if payload["isRunning"] != true {
			t.Fatalf("expected isRunning=true during initialization without collector, got %+v", payload)
		}
		if payload["lifecyclePhase"] != "initializing" {
			t.Fatalf("expected lifecyclePhase=initializing, got %+v", payload["lifecyclePhase"])
		}
		lifecycle, ok := payload["lifecycle"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected structured lifecycle payload, got %+v", payload["lifecycle"])
		}
		if lifecycle["phase"] != "initializing" {
			t.Fatalf("expected lifecycle.phase=initializing, got %+v", lifecycle["phase"])
		}
	})

	t.Run("with_collector", func(t *testing.T) {
		c := stats.NewCollector()
		c.Track("find", 10*time.Millisecond)
		s.CurrentStats = c
		s.IsRunning = true
		s.LastError = "none"
		s.CurrentIteration = 2
		s.TotalIterations = 5
		s.IsWaiting = true
		s.IntervalStr = "1s"
		s.AppConfig = &config.AppConfig{Duration: "7s"}
		s.LifecyclePhase = "initializing"
		s.InitStartedAt = time.Now().Add(-3 * time.Second)

		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		s.handleStats(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode stats payload: %v", err)
		}
		if payload["isRunning"] != true {
			t.Fatalf("expected running=true")
		}
		if payload["duration"] != "7s" {
			t.Fatalf("expected duration from app config, got %v", payload["duration"])
		}
		if payload["findOps"].(float64) != 1 {
			t.Fatalf("expected findOps=1, got %v", payload["findOps"])
		}
		if payload["lifecyclePhase"] != "running" {
			t.Fatalf("expected lifecycle to transition to running after first observed operations, got %v", payload["lifecyclePhase"])
		}
		if payload["executionElapsedSec"].(float64) < 0 {
			t.Fatalf("expected non-negative executionElapsedSec, got %v", payload["executionElapsedSec"])
		}
		if _, ok := payload["lifecycleRecentEvents"]; !ok {
			t.Fatalf("expected lifecycleRecentEvents in stats payload")
		}
	})

	t.Run("exposes_last_error_for_runtime_failures", func(t *testing.T) {
		c := stats.NewCollector()
		s.CurrentStats = c
		s.IsRunning = false
		s.LastError = "Workload crashed: panic in worker 1"
		s.AppConfig = &config.AppConfig{Duration: "7s"}

		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		s.handleStats(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode stats payload: %v", err)
		}
		if payload["lastError"] != "Workload crashed: panic in worker 1" {
			t.Fatalf("expected lastError to propagate, got %#v", payload["lastError"])
		}
	})
}

type webuiSeams struct {
	loadCollections   func(path string, loadDefault bool) (*config.CollectionsFile, error)
	loadQueries       func(path string, loadDefault bool) (*config.QueriesFile, error)
	connect           func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error)
	disconnect        func(c *db.Connection, ctx context.Context)
	createCollections func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error
	createIndexes     func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error
	insertRandomDocs  func(ctx context.Context, database *driverMongo.Database, col config.CollectionDefinition, count int, cfg *config.AppConfig) error
	runWorkload       func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error
	runRawInjector    func(ctx context.Context, database *driverMongo.Database, cfg *config.AppConfig, uiCollector ...*stats.Collector) error
}

func newMultipartRequest(t *testing.T, fields map[string]string, files map[string]string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	for formField, contents := range files {
		part, err := w.CreateFormFile(formField, formField+".json")
		if err != nil {
			t.Fatalf("create file field %q: %v", formField, err)
		}
		if _, err := part.Write([]byte(contents)); err != nil {
			t.Fatalf("write file content for %q: %v", formField, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/start", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, httptest.NewRecorder()
}

func newDefinitionUploadRequest(t *testing.T, path, formField, filename, contents string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile(formField, filename)
	if err != nil {
		t.Fatalf("create upload part: %v", err)
	}
	if _, err := part.Write([]byte(contents)); err != nil {
		t.Fatalf("write upload content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close upload writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, httptest.NewRecorder()
}

func newServerWithTempDefinitions(t *testing.T) *WebServer {
	t.Helper()
	store, err := definitions.NewFileStore(t.TempDir() + "/definitions.json")
	if err != nil {
		t.Fatalf("create temp definition store: %v", err)
	}
	s := NewServer(&config.AppConfig{})
	s.DefinitionStore = store
	return s
}

func decodeJSONMap(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON map: %v", err)
	}
	return m
}

func assertStartErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatusCode int, wantMessageLike string) {
	t.Helper()
	if rec.Code != wantStatusCode {
		t.Fatalf("expected status %d, got %d body=%s", wantStatusCode, rec.Code, rec.Body.String())
	}
	payload := decodeJSONMap(t, rec.Body.Bytes())
	if payload["status"] != "error" {
		t.Fatalf("expected payload status=error, got %+v", payload)
	}
	msg, _ := payload["message"].(string)
	if strings.TrimSpace(msg) == "" {
		t.Fatalf("expected non-empty error message, got %+v", payload)
	}
	if wantMessageLike != "" && !strings.Contains(strings.ToLower(msg), strings.ToLower(wantMessageLike)) {
		t.Fatalf("expected message to contain %q, got %q", wantMessageLike, msg)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func withWebUISeams(t *testing.T, s webuiSeams) {
	t.Helper()
	origLoadCollections := loadCollectionsFn
	origLoadQueries := loadQueriesFn
	origConnect := connectFn
	origDisconnect := disconnectFn
	origCreateCollections := createCollectionsFn
	origCreateIndexes := createIndexesFn
	origInsertRandomDocs := insertRandomDocsFn
	origRunWorkload := runWorkloadFn
	origRunRawInjector := runRawInjectorFn

	if s.loadCollections != nil {
		loadCollectionsFn = s.loadCollections
	}
	if s.loadQueries != nil {
		loadQueriesFn = s.loadQueries
	}
	if s.connect != nil {
		connectFn = s.connect
	}
	if s.disconnect != nil {
		disconnectFn = s.disconnect
	}
	if s.createCollections != nil {
		createCollectionsFn = s.createCollections
	}
	if s.createIndexes != nil {
		createIndexesFn = s.createIndexes
	}
	if s.insertRandomDocs != nil {
		insertRandomDocsFn = s.insertRandomDocs
	}
	if s.runWorkload != nil {
		runWorkloadFn = s.runWorkload
	}
	if s.runRawInjector != nil {
		runRawInjectorFn = s.runRawInjector
	}

	t.Cleanup(func() {
		loadCollectionsFn = origLoadCollections
		loadQueriesFn = origLoadQueries
		connectFn = origConnect
		disconnectFn = origDisconnect
		createCollectionsFn = origCreateCollections
		createIndexesFn = origCreateIndexes
		insertRandomDocsFn = origInsertRandomDocs
		runWorkloadFn = origRunWorkload
		runRawInjectorFn = origRunRawInjector
	})
}

func TestHandleStartSuccessWithLoadedConfigFlow(t *testing.T) {
	var mu sync.Mutex
	loadedDefaultCollections := false
	loadedDefaultQueries := false
	connectedDBName := ""
	createCollectionsCalled := false
	createIndexesCalled := false
	disconnectCalled := false

	runWorkloadCalled := make(chan []config.QueryDefinition, 1)

	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			loadedDefaultCollections = loadDefault
			return &config.CollectionsFile{
				Collections: []config.CollectionDefinition{
					{Name: "flights", DatabaseName: "benchdb"},
				},
			}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			loadedDefaultQueries = loadDefault
			return &config.QueriesFile{
				Queries: []config.QueryDefinition{
					{Name: "find_flights", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}},
				},
			}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			connectedDBName = dbName
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {
			disconnectCalled = true
		},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			createCollectionsCalled = true
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			createIndexesCalled = true
			return nil
		},
		insertRandomDocs: func(ctx context.Context, database *driverMongo.Database, col config.CollectionDefinition, count int, cfg *config.AppConfig) error {
			t.Fatalf("insertRandomDocs should not be called when documents_count is 0")
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			mu.Lock()
			filtered := append([]config.QueryDefinition(nil), queries...)
			mu.Unlock()
			runWorkloadCalled <- filtered
			return nil
		},
	})

	s := NewServer(&config.AppConfig{
		Duration:        "1s",
		Iterations:      1,
		CollectionsPath: "ignored",
		QueriesPath:     "ignored",
	})

	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload": "on",
	}, nil)

	s.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "started" {
		t.Fatalf("expected status=started, got %+v", resp)
	}

	select {
	case queries := <-runWorkloadCalled:
		if len(queries) != 1 || queries[0].Collection != "flights" || queries[0].Database != "benchdb" {
			t.Fatalf("expected validated queries for flights/benchdb, got %+v", queries)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}

	if !loadedDefaultCollections || !loadedDefaultQueries {
		t.Fatalf("expected load functions to receive loadDefault=true from default_workload=on")
	}
	if connectedDBName != "benchdb" {
		t.Fatalf("expected dbName from first collection, got %q", connectedDBName)
	}
	if !createCollectionsCalled || !createIndexesCalled {
		t.Fatalf("expected createCollections and createIndexes calls")
	}

	waitUntil(t, 2*time.Second, func() bool { return !s.IsRunning }, "expected IsRunning to reset to false after goroutine completion")
	if s.IsRunning {
		t.Fatalf("expected IsRunning to reset to false after goroutine completion")
	}
	if !disconnectCalled {
		t.Fatalf("expected disconnect to be called at end of run")
	}
}

func TestHandleStartSuccessWithUploadedCustomFilesSkipsLoaders(t *testing.T) {
	loadCalled := false
	connectedDBName := ""
	runCalled := make(chan struct{}, 1)

	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			loadCalled = true
			return nil, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			loadCalled = true
			return nil, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			connectedDBName = dbName
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
			runCalled <- struct{}{}
			return nil
		},
	})

	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})

	req, rec := newMultipartRequest(t,
		map[string]string{"default_workload": "false"},
		map[string]string{
			"collections_file": `[{"database":"customdb","collection":"customcol","fields":{}}]`,
			"queries_file":     `[{"name":"find_customcol","collection":"customcol","operation":"find","filter":{}}]`,
		},
	)
	s.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}

	if loadCalled {
		t.Fatalf("expected load functions not to be called when custom files are provided")
	}
	if connectedDBName != "customdb" {
		t.Fatalf("expected dbName from uploaded custom collections, got %q", connectedDBName)
	}
}

func TestHandleStartSuccessWithSelectedStoredDefinitions(t *testing.T) {
	loadCalled := false
	runCalled := make(chan []config.QueryDefinition, 1)

	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			loadCalled = true
			return nil, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			loadCalled = true
			return nil, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			if dbName != "stored_db" {
				t.Fatalf("expected stored_db connection, got %q", dbName)
			}
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
			runCalled <- append([]config.QueryDefinition(nil), queries...)
			return nil
		},
	})

	s := newServerWithTempDefinitions(t)
	s.AppConfig.Duration = "1s"
	s.AppConfig.Iterations = 1
	collDef, err := s.DefinitionStore.Create(definitions.KindCollection, definitions.Input{
		Name:    "stored collections",
		Content: `{"collections":[{"database":"stored_db","collection":"orders","fields":{}}]}`,
	})
	if err != nil {
		t.Fatalf("create stored collection definition: %v", err)
	}
	queryDef, err := s.DefinitionStore.Create(definitions.KindQuery, definitions.Input{
		Name:    "stored queries",
		Content: `{"queries":[{"name":"find_stored","collection":"orders","operation":"find","filter":{}}]}`,
	})
	if err != nil {
		t.Fatalf("create stored query definition: %v", err)
	}

	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":         "false",
		"collection_definition_id": collDef.ID,
		"query_definition_id":      queryDef.ID,
	}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case queries := <-runCalled:
		if len(queries) != 1 || queries[0].Database != "stored_db" || queries[0].SourceType != "stored_definition" {
			t.Fatalf("expected query bound from stored definitions, got %+v", queries)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}
	if loadCalled {
		t.Fatalf("expected file loaders not to be called when stored definitions are selected")
	}
}

func TestHandleStartSuccessWithSelectedBuiltInDefaultDefinitions(t *testing.T) {
	loadedDefaults := 0
	runCalled := make(chan []config.QueryDefinition, 1)

	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			if !loadDefault {
				t.Fatalf("expected built-in collection loader to request defaults")
			}
			loadedDefaults++
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{
				{DatabaseName: "airline", Name: "flights", Fields: map[string]config.CollectionField{}},
			}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			if !loadDefault {
				t.Fatalf("expected built-in query loader to request defaults")
			}
			loadedDefaults++
			return &config.QueriesFile{Queries: []config.QueryDefinition{
				{Name: "find_flights", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}},
			}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			if dbName != "airline" {
				t.Fatalf("expected airline connection, got %q", dbName)
			}
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
			runCalled <- append([]config.QueryDefinition(nil), queries...)
			return nil
		},
	})

	s := newServerWithTempDefinitions(t)
	s.AppConfig.Duration = "1s"
	s.AppConfig.Iterations = 1
	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":         "false",
		"collection_definition_id": builtinCollectionDefinitionID,
		"query_definition_id":      builtinQueryDefinitionID,
	}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case queries := <-runCalled:
		if len(queries) != 1 || queries[0].Database != "airline" || queries[0].SourceType != "stored_definition" {
			t.Fatalf("expected query bound from built-in definitions, got %+v", queries)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}
	if loadedDefaults != 2 {
		t.Fatalf("expected built-in collection and query defaults to load once each, got %d", loadedDefaults)
	}
}

func TestHandleStartRejectsUnknownQueryCollectionReference(t *testing.T) {
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{
				{DatabaseName: "db1", Name: "orders", Fields: map[string]config.CollectionField{}},
			}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{
				{Name: "find_missing", Collection: "missing", Operation: "find", Filter: map[string]interface{}{}},
			}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			t.Fatalf("connect should not be called when query validation fails")
			return nil, nil
		},
	})

	s := NewServer(&config.AppConfig{CollectionsPath: "ignored", QueriesPath: "ignored"})
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on"}, nil)
	s.handleStart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected JSON error response with 400 status, got %d", rec.Code)
	}
	payload := decodeJSONMap(t, rec.Body.Bytes())
	if payload["status"] != "error" {
		t.Fatalf("expected status=error, got %+v", payload)
	}
	msg, _ := payload["message"].(string)
	if !strings.Contains(msg, "unknown collection") {
		t.Fatalf("expected unknown collection error, got %q", msg)
	}
}

func TestHandleStartSupportsWrappedQueriesFileUpload(t *testing.T) {
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
			runCalled <- append([]config.QueryDefinition(nil), queries...)
			return nil
		},
	})

	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})
	req, rec := newMultipartRequest(t,
		map[string]string{"default_workload": "false"},
		map[string]string{
			"collections_file": `{"collections":[{"database":"shop","collection":"orders","fields":{}}]}`,
			"queries_file":     `{"queries":[{"name":"find_orders","collection":"orders","operation":"find","filter":{}}]}`,
		},
	)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-runCalled:
		if len(got) != 1 {
			t.Fatalf("expected one query, got %+v", got)
		}
		if got[0].Database != "shop" {
			t.Fatalf("expected database bound from collection definition, got %q", got[0].Database)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected runWorkload to be called")
	}
}

func TestHandleStartLoadCollectionsErrorReturnsErrorResponse(t *testing.T) {
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return nil, context.DeadlineExceeded
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			t.Fatalf("connect should not be called when load collections fails")
			return nil, nil
		},
	})

	s := NewServer(&config.AppConfig{CollectionsPath: "ignored", QueriesPath: "ignored"})
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on"}, nil)
	s.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected JSON error response with 200 status, got %d", rec.Code)
	}
	payload := decodeJSONMap(t, rec.Body.Bytes())
	if payload["status"] != "error" {
		t.Fatalf("expected status=error, got %+v", payload)
	}
	if s.IsRunning {
		t.Fatalf("expected IsRunning reset after load error")
	}
}
