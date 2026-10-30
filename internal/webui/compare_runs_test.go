package webui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

func TestMongoURIHostLeavesOnlyTheHosts(t *testing.T) {
	cases := map[string]string{
		"mongodb://admin:s3cret@db1:27017/admin?authSource=admin": "db1:27017",
		"mongodb+srv://u:p@cluster0.example.net/test":             "cluster0.example.net",
		"mongodb://h1:1,h2:2/?replicaSet=rs0":                     "h1:1,h2:2",
		"mongodb://localhost:27017":                               "localhost:27017",
		"":                                                        "",
	}
	for uri, want := range cases {
		if got := mongoURIHost(uri); got != want {
			t.Errorf("mongoURIHost(%q) = %q, want %q", uri, got, want)
		}
		if strings.Contains(mongoURIHost(uri), "s3cret") {
			t.Errorf("password leaked for %q", uri)
		}
	}
}

func TestStoredRunNamesItsServerForCompareRuns(t *testing.T) {
	dir := t.TempDir()
	s := &WebServer{
		runHistory: OpenRunHistory(filepath.Join(dir, "runs")), dashboardID: "run-a", dashboardMode: "generic",
		LifecyclePhase: "completed", CurrentStats: stats.NewCollector(),
		AppConfig:     &config.AppConfig{URI: "mongodb://admin:s3cret@db1:27017/admin", Duration: "30s"},
		ExecStartedAt: time.Now().Add(-time.Minute), CompletedAt: time.Now(),
	}
	s.persistDashboard()
	list := s.runHistory.List(time.Now(), stats.DefaultTrendPolicy())
	if len(list) != 1 || len(list[0].Servers) != 1 || list[0].Servers[0] != "db1:27017" {
		t.Fatalf("servers = %+v", list)
	}
}

func TestLiveStatsNameTheDashboardSoCompareRunsCanDropItsStoredCopy(t *testing.T) {
	s := &WebServer{dashboardID: "run-live", CurrentStats: stats.NewCollector(), IsRunning: true, LifecyclePhase: "running",
		AppConfig: &config.AppConfig{Duration: "30s"}, ExecStartedAt: time.Now().Add(-time.Second), InitStartedAt: time.Now().Add(-2 * time.Second)}
	rec := httptest.NewRecorder()
	s.handleStats(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if got := decodeJSONMap(t, rec.Body.Bytes())["dashboardId"]; got != "run-live" {
		t.Fatalf("dashboardId = %v", got)
	}
}
