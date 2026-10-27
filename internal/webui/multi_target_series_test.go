package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

type seriesPayload struct {
	Targets []struct {
		Index        int                           `json:"index"`
		Label        string                        `json:"label"`
		Status       string                        `json:"status"`
		Ops          map[string]uint64             `json:"ops"`
		Dist         map[string]map[string]float64 `json:"dist"`
		ChartHistory []stats.UIChartPoint          `json:"chartHistory"`
		Heatmap      []stats.LatencyBucket         `json:"latencyHeatmap"`
	} `json:"targets"`
}

func getSeries(t *testing.T, s *WebServer, url string) (seriesPayload, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleStats(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var p seriesPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p, rec.Body.String()
}

func TestParseTargetSince(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/stats?targetSince=1:12:1700,2:0:0,x:1:1,3:-1:0,4:5", nil)
	got := parseTargetSince(r)
	if len(got) != 2 || got[1] != (targetCursor{chartSeq: 12, heatMs: 1700}) || got[2] != (targetCursor{}) {
		t.Fatalf("parseTargetSince = %+v", got)
	}
	if len(parseTargetSince(httptest.NewRequest(http.MethodGet, "/api/stats", nil))) != 0 {
		t.Fatal("no cursor means every target is sent in full")
	}
}

func TestMultiTargetSeriesCountsDistributionAndCursor(t *testing.T) {
	a, b := stats.NewCollector(), stats.NewCollector()
	a.Track("find", 2*time.Millisecond)
	a.Track("find", 4*time.Millisecond)
	a.Track("update", time.Millisecond)
	a.Track("upsert", time.Millisecond)
	b.Track("insert", 3*time.Millisecond)
	a.RecordUIChartSample(0.5, 1, false, nil)
	a.RecordUIChartSample(1.0, 1, false, nil)
	a.CaptureLatencyInterval()
	plan := &multitarget.Plan{Targets: []*multitarget.Target{{Index: 1, Label: "m8", Collector: a}, {Index: 2, Label: "m9", Collector: b}}}

	all := multiTargetSeries(plan, nil)
	if len(all) != 2 || all[0]["label"] != "m8" || all[1]["index"] != 2 {
		t.Fatalf("series = %+v", all)
	}
	ops := all[0]["ops"].(map[string]uint64)
	if ops["find"] != 2 || ops["update"] != 2 || ops["total"] != 4 {
		t.Fatalf("ops = %v (updates include upserts)", ops)
	}
	dist := all[0]["dist"].(map[string]map[string]float64)
	if dist["find"]["count"] != 2 || dist["find"]["max"] < 4 || dist["insert"]["count"] != 0 {
		t.Fatalf("dist = %v", dist)
	}
	if n := len(all[0]["chartHistory"].([]stats.UIChartPoint)); n != 2 {
		t.Fatalf("chart samples = %d, want 2", n)
	}
	heat := all[0]["latencyHeatmap"].([]stats.LatencyBucket)
	if len(heat) != 1 {
		t.Fatalf("heatmap windows = %d", len(heat))
	}

	// A cursor returns only what the dashboard has not drawn yet.
	next := multiTargetSeries(plan, map[int]targetCursor{1: {chartSeq: 1, heatMs: heat[0].TimestampUnixMs}})
	if pts := next[0]["chartHistory"].([]stats.UIChartPoint); len(pts) != 1 || pts[0].Seq != 2 {
		t.Fatalf("after the cursor: %+v", pts)
	}
	if n := len(next[0]["latencyHeatmap"].([]stats.LatencyBucket)); n != 0 {
		t.Fatalf("heatmap after the cursor = %d windows", n)
	}
	if multiTargetSeries(nil, nil) != nil {
		t.Fatal("no plan, no series")
	}
}

// A concurrent run samples every target on its own clock, so the dashboard
// can draw one line per server while they run.
func TestStatsSampleEveryRunningTargetOfAConcurrentRun(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	started := map[*stats.Collector]bool{}
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "flights", DatabaseName: "benchdb"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "f", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
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
			c := uiCollector[0]
			c.MarkWorkloadStart()
			c.Track("find", time.Millisecond)
			mu.Lock()
			started[c] = true
			mu.Unlock()
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored", TrendBaselineStore: "off"})
	waitForRunsOnCleanup(t, s)
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "documents_count": "0", "interval_delay": "0s",
		"workload_deterministic": "true", "operations_per_worker": "2", "random_seed": "3", "drop_collections": "true",
		"workload_record_outcomes": "true", "workload_deterministic_outcomes_path": filepath.Join(t.TempDir(), "det.ndjson"),
		"workload_replay_targets":      `[{"label":"m8","uri":"mongodb://user:secret@a:1"},{"label":"m9","uri":"mongodb://user:secret@b:2"}]`,
		"workload_replay_targets_mode": "concurrent"}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
	}
	defer func() {
		close(release)
		waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
		s.runWG.Wait()
	}()
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 2
	}, "both targets should be running")

	first, body := getSeries(t, s, "/api/stats?chartSince=0")
	if strings.Contains(body, "secret") || strings.Contains(body, "mongodb://") {
		t.Fatal("/api/stats must not expose target URIs")
	}
	if len(first.Targets) != 2 {
		t.Fatalf("targets = %+v", first.Targets)
	}
	for _, tg := range first.Targets {
		if tg.Status != multitarget.StatusRunning || len(tg.ChartHistory) != 1 || tg.Ops["find"] != 1 {
			t.Fatalf("%s: status %q, %d samples, ops %v; every running target is sampled", tg.Label, tg.Status, len(tg.ChartHistory), tg.Ops)
		}
		if e := tg.ChartHistory[0].ElapsedSec; e < 0 || e > 5 {
			t.Fatalf("%s: elapsed %v is not measured from its own start", tg.Label, e)
		}
	}
	cursor := "1:" + strconv.Itoa(first.Targets[0].ChartHistory[0].Seq) + ":0,2:" + strconv.Itoa(first.Targets[1].ChartHistory[0].Seq) + ":0"
	next, _ := getSeries(t, s, "/api/stats?chartSince=0&targetSince="+cursor)
	for _, tg := range next.Targets {
		if len(tg.ChartHistory) != 1 || tg.ChartHistory[0].Seq != 2 {
			t.Fatalf("%s: with a cursor, only the new sample is sent: %+v", tg.Label, tg.ChartHistory)
		}
	}
}

// A stored run keeps every target's charts, so Completed Runs can reopen the
// several-servers view.
func TestDashboardStatsKeepEveryTargetSeries(t *testing.T) {
	a, b := stats.NewCollector(), stats.NewCollector()
	a.RecordUIChartSample(1, 1, false, nil)
	b.RecordUIChartSample(1, 1, false, nil)
	s := NewServer(&config.AppConfig{Duration: "1s"})
	s.multiTarget = &multitarget.Plan{Targets: []*multitarget.Target{{Index: 1, Label: "m8", Collector: a}, {Index: 2, Label: "m9", Collector: b}}}
	out := dashboardStats(s, a, a.UIChartHistory(0), nil, 0)
	series, ok := out["targets"].([]map[string]any)
	if !ok || len(series) != 2 || len(series[1]["chartHistory"].([]stats.UIChartPoint)) != 1 {
		t.Fatalf("stored targets = %#v", out["targets"])
	}
}
