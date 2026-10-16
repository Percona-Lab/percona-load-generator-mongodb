package webui

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

var trafficFixture = filepath.Join("..", "trafficimport", "testdata", "mongod.log")

func trafficCall(t *testing.T, s *WebServer, method, target string, fields map[string]string, files map[string][]byte) (int, map[string]interface{}, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for name, b := range files {
		part, _ := mw.CreateFormFile("traffic_file", name)
		_, _ = part.Write(b)
	}
	_ = mw.Close()
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	if target == "/api/import-traffic" {
		s.handleImportTraffic(rec, req)
	} else {
		s.handleImportTrafficItem(rec, req)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec
}

func readTrafficFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(trafficFixture)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImportTrafficAPI(t *testing.T) {
	s := &WebServer{}
	t.Cleanup(s.removeTrafficImports)
	code, out, _ := trafficCall(t, s, http.MethodPost, "/api/import-traffic", map[string]string{"workers": "1", "compression": "gzip"}, map[string][]byte{"mongod.log": readTrafficFixture(t)})
	if code != http.StatusOK {
		t.Fatalf("POST: %d %v", code, out)
	}
	imp := out["import"].(map[string]interface{})
	rep := imp["report"].(map[string]interface{})
	if rep["converted"] != float64(8) || rep["workers"] != float64(1) || rep["path"] != nil || rep["compression"] != "gzip" {
		t.Fatalf("report = %v", rep)
	}
	path := imp["server_path"].(string)
	info, err := workloadrec.Scan(path)
	if err != nil || info.Ops != 8 || info.Compression != workloadrec.CompressionGzip || info.Header.Import == nil {
		t.Fatalf("kept recording %+v: %v", info, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if !strings.HasSuffix(imp["file_name"].(string), ".ndjson.gz") {
		t.Fatalf("file name = %v", imp["file_name"])
	}

	code, _, dl := trafficCall(t, s, http.MethodGet, imp["download_url"].(string), nil, nil)
	want, _ := os.ReadFile(path)
	if code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), want) || !strings.Contains(dl.Header().Get("Content-Disposition"), imp["file_name"].(string)) {
		t.Fatalf("download: %d %q", code, dl.Header().Get("Content-Disposition"))
	}
	_, list, _ := trafficCall(t, s, http.MethodGet, "/api/import-traffic", nil, nil)
	if n := len(list["imports"].([]interface{})); n != 1 || list["max_imports"] != float64(maxTrafficImports) {
		t.Fatalf("list = %v", list)
	}
	if code, _, _ := trafficCall(t, s, http.MethodGet, "/api/import-traffic/nope/recording", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}

	for i := 0; i < maxTrafficImports; i++ {
		if code, out, _ := trafficCall(t, s, http.MethodPost, "/api/import-traffic", nil, map[string][]byte{"mongod.log": readTrafficFixture(t)}); code != http.StatusOK {
			t.Fatalf("POST %d: %d %v", i, code, out)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the oldest import must be deleted once %d newer ones exist: %v", maxTrafficImports, err)
	}
	kept := s.trafficImportList()
	if len(kept) != maxTrafficImports {
		t.Fatalf("kept %d", len(kept))
	}
	s.removeTrafficImports()
	if _, err := os.Stat(kept[0].ServerPath); !os.IsNotExist(err) {
		t.Fatal("shutdown cleanup must delete the kept recordings")
	}
}

func TestImportTrafficAPIRejectsBadInput(t *testing.T) {
	s := &WebServer{}
	t.Cleanup(s.removeTrafficImports)
	cases := []struct {
		fields map[string]string
		files  map[string][]byte
		want   string
	}{
		{nil, nil, "traffic_file"},
		{map[string]string{"workers": "-1"}, map[string][]byte{"a.log": readTrafficFixture(t)}, "workers must be"},
		{map[string]string{"compression": "lz4"}, map[string][]byte{"a.log": readTrafficFixture(t)}, "unknown compression"},
		{nil, map[string][]byte{"a.log": []byte(`{"ctx":"listener","attr":{}}` + "\n")}, "no operations"},
	}
	for _, c := range cases {
		code, out, _ := trafficCall(t, s, http.MethodPost, "/api/import-traffic", c.fields, c.files)
		if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), c.want) {
			t.Fatalf("%v: %d %v", c.fields, code, out)
		}
	}
	if len(s.trafficImportList()) != 0 {
		t.Fatal("failed imports must not be kept")
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/import-traffic", nil)
	rec := httptest.NewRecorder()
	s.handleImportTraffic(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %d", rec.Code)
	}
}

func TestUIFixturesMatchAPITrafficImport(t *testing.T) {
	s := &WebServer{}
	t.Cleanup(s.removeTrafficImports)
	code, out, _ := trafficCall(t, s, http.MethodPost, "/api/import-traffic", nil, map[string][]byte{"mongod.log": readTrafficFixture(t)})
	if code != http.StatusOK {
		t.Fatalf("POST: %d %v", code, out)
	}
	_, failed, _ := trafficCall(t, s, http.MethodPost, "/api/import-traffic", nil, map[string][]byte{"empty.log": []byte(`{"ctx":"listener","attr":{}}` + "\n")})
	imp := out["import"].(map[string]interface{})
	id := imp["id"].(string)
	imp["server_path"] = "/tmp/plgm-imported-traffic-fixture.ndjson"
	imp["created_at"] = "normalized"
	imp["report"].(map[string]interface{})["header_hash"] = "normalized"
	raw, _ := json.Marshal(map[string]interface{}{"import": imp, "failed": failed})
	var fixture map[string]interface{}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(raw), id, "fixture-import-1")), &fixture); err != nil {
		t.Fatal(err)
	}
	checkUIFixture(t, "traffic_import.json", fixture)
}
