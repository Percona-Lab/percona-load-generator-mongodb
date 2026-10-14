package webui

import (
	"os"
	"strings"
	"testing"
)

type skipper interface {
	Helper()
	Skipf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
}

// skipIntegration skips a test whose MongoDB prerequisite is missing. With
// PLGM_IT_REQUIRE_MONGO set (CI), it fails instead, so an unavailable required
// service cannot pass as a skip.
func skipIntegration(t skipper, format string, args ...interface{}) {
	t.Helper()
	switch strings.TrimSpace(strings.ToLower(os.Getenv("PLGM_IT_REQUIRE_MONGO"))) {
	case "1", "true", "yes", "on":
		t.Fatalf("required integration prerequisite unavailable (PLGM_IT_REQUIRE_MONGO is set): "+format, args...)
		return
	}
	t.Skipf(format, args...)
}

type recordingSkipper struct{ skipped, failed string }

func (r *recordingSkipper) Helper() {}
func (r *recordingSkipper) Skipf(format string, args ...interface{}) {
	r.skipped = format
}
func (r *recordingSkipper) Fatalf(format string, args ...interface{}) {
	r.failed = format
}

func TestSkipIntegrationFailsWhenMongoIsRequired(t *testing.T) {
	t.Setenv("PLGM_IT_REQUIRE_MONGO", "")
	r := &recordingSkipper{}
	skipIntegration(r, "mongo is not reachable")
	if r.skipped == "" || r.failed != "" {
		t.Fatalf("without the requirement the test must skip: %+v", r)
	}

	t.Setenv("PLGM_IT_REQUIRE_MONGO", "1")
	r = &recordingSkipper{}
	skipIntegration(r, "mongo is not reachable")
	if r.failed == "" || r.skipped != "" {
		t.Fatalf("a required, unavailable service must fail, not skip: %+v", r)
	}
}
