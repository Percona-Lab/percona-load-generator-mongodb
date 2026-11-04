package mongo

import (
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func TestReadPreferenceOf(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *config.AppConfig
		wantMode readpref.Mode
		wantNil  bool
	}{
		{name: "nil config", cfg: nil, wantNil: true},
		{name: "none set", cfg: &config.AppConfig{URI: "mongodb://localhost:27017"}, wantNil: true},
		{
			name:     "read_preference setting",
			cfg:      &config.AppConfig{URI: "mongodb://localhost:27017", ConnectionParams: config.ConnectionParams{ReadPreference: "secondary"}},
			wantMode: readpref.SecondaryMode,
		},
		{
			name:     "readPreference in the URI",
			cfg:      &config.AppConfig{URI: "mongodb://localhost:27017/?readPreference=secondaryPreferred"},
			wantMode: readpref.SecondaryPreferredMode,
		},
		{
			name:     "read_preference setting wins over the URI",
			cfg:      &config.AppConfig{URI: "mongodb://localhost:27017/?readPreference=primary", ConnectionParams: config.ConnectionParams{ReadPreference: "nearest"}},
			wantMode: readpref.NearestMode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := readPreferenceOf(tc.cfg)
			if tc.wantNil {
				if rp != nil {
					t.Fatalf("readPreferenceOf() = %v, want nil", rp.Mode())
				}
				return
			}
			if rp == nil || rp.Mode() != tc.wantMode {
				t.Fatalf("readPreferenceOf() = %v, want mode %v", rp, tc.wantMode)
			}
		})
	}
}

func TestFastFindRunCmdOptionsCarryReadPreference(t *testing.T) {
	if got := fastFindRunCmdOptions(nil); got != nil {
		t.Fatalf("without a read preference the fast find must send no options, got %d", len(got))
	}

	wCfg := workloadConfig{fastFindOptions: fastFindRunCmdOptions(readpref.Secondary())}
	opts := wCfg.fastFindOptions
	if len(opts) != 1 {
		t.Fatalf("got %d options, want 1", len(opts))
	}
	var applied options.RunCmdOptions
	for _, set := range opts[0].List() {
		if err := set(&applied); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	if applied.ReadPreference == nil || applied.ReadPreference.Mode() != readpref.SecondaryMode {
		t.Fatalf("fast find read preference = %v, want secondary", applied.ReadPreference)
	}
}
