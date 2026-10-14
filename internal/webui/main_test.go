package webui

import (
	"os"
	"testing"
)

// TestMain keeps NewServer from reading or writing trend baselines in the
// user's config directory; tests that need a store open one in t.TempDir().
func TestMain(m *testing.M) {
	os.Setenv("PLGM_TREND_BASELINE_STORE", "off")
	os.Exit(m.Run())
}
