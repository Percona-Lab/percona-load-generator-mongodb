package mongo

import (
	"os"
	"strings"
	"testing"
)

// skipper is the part of testing.TB that the integration gate uses.
type skipper interface {
	Helper()
	Skipf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
}

func integrationBoolEnv(key string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// skipIntegration skips a test whose MongoDB prerequisite is missing. With
// PLGM_IT_REQUIRE_MONGO set (CI), it fails instead, so an unavailable required
// service cannot pass as a skip.
func skipIntegration(t skipper, format string, args ...interface{}) {
	t.Helper()
	if integrationBoolEnv("PLGM_IT_REQUIRE_MONGO") {
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

	t.Setenv("PLGM_IT_REQUIRE_MONGO", "true")
	r = &recordingSkipper{}
	skipIntegration(r, "mongo is not reachable")
	if r.failed == "" || r.skipped != "" || !strings.Contains(r.failed, "mongo is not reachable") {
		t.Fatalf("a required, unavailable service must fail, not skip: %+v", r)
	}
}
