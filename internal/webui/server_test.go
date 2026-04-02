package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
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
		if payload["isRunning"] != false {
			t.Fatalf("expected isRunning=false without collector, got %+v", payload)
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

func decodeJSONMap(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON map: %v", err)
	}
	return m
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
					{Collection: "flights", Operation: "find"},
					{Collection: "other", Operation: "find"},
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
		if len(queries) != 1 || queries[0].Collection != "flights" {
			t.Fatalf("expected filtered queries for valid collection only, got %+v", queries)
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
			"queries_file":     `[{"collection":"customcol","operation":"find","filter":{}}]`,
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
