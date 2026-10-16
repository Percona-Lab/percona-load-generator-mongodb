package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestWorkloadTargetsDefaultOff(t *testing.T) {
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkloadReplayTargets) != 0 || cfg.WorkloadReplayTargetsMode != WorkloadTargetsSequential ||
		cfg.WorkloadReplayOps != WorkloadReplayOpsAll || cfg.WorkloadDataFingerprint != DataFingerprintOff {
		t.Fatalf("multi-target settings must default off: %+v %q %q %q", cfg.WorkloadReplayTargets, cfg.WorkloadReplayTargetsMode, cfg.WorkloadReplayOps, cfg.WorkloadDataFingerprint)
	}
	if ReplayReadsOnly(cfg) || ConcurrentTargets(cfg) || DataFingerprintMode(cfg) != DataFingerprintOff {
		t.Fatal("helpers must report the defaults as off")
	}
}

func TestParseReplayTargets(t *testing.T) {
	got := ParseReplayTargets(" mongo8=mongodb://a:1,b:2/?replicaSet=rs&w=1 ;\nmongo9 = mongodb+srv://c/?x=y=z;; nolabel ")
	want := []ReplayTarget{
		{Label: "mongo8", URI: "mongodb://a:1,b:2/?replicaSet=rs&w=1"},
		{Label: "mongo9", URI: "mongodb+srv://c/?x=y=z"},
		{Label: "", URI: "nolabel"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseReplayTargets = %+v", got)
	}
	if TargetPath("out/outcomes.ndjson", "m8") != "out/outcomes.m8.ndjson" || TargetPath("outcomes", "m8") != "outcomes.m8" || TargetPath("", "m8") != "" {
		t.Fatal("TargetPath must put the label before the extension")
	}
}

func TestCloneQueriesIsDeep(t *testing.T) {
	orig := []QueryDefinition{{Name: "q", Filter: map[string]interface{}{"a": map[string]interface{}{"$gt": 1}},
		Pipeline: []interface{}{bson.D{{Key: "$sort", Value: bson.D{{Key: "z", Value: 1}, {Key: "a", Value: -1}}}}}, Update: map[string]interface{}{"$set": bson.M{"x": bson.A{1}}}}}
	cp := CloneQueries(orig)
	if !reflect.DeepEqual(cp, orig) {
		t.Fatalf("clone differs: %+v", cp)
	}
	cp[0].Filter["a"].(map[string]interface{})["$gt"] = 2
	cp[0].Pipeline[0].(bson.D)[0].Value.(bson.D)[0].Key = "y"
	cp[0].Update["$set"].(bson.M)["x"].(bson.A)[0] = 9
	if orig[0].Filter["a"].(map[string]interface{})["$gt"] != 1 || orig[0].Pipeline[0].(bson.D)[0].Value.(bson.D)[0].Key != "z" || orig[0].Update["$set"].(bson.M)["x"].(bson.A)[0] != 1 {
		t.Fatalf("clone shares nested values with the original: %+v", orig)
	}
	if CloneQueries(nil) != nil || CloneQueries([]QueryDefinition{{}})[0].Filter != nil {
		t.Fatal("nil values must stay nil")
	}
}

func TestCloneCollectionsIsDeep(t *testing.T) {
	ttl := int32(60)
	orig := []CollectionDefinition{{DatabaseName: "d", Name: "c",
		Fields:      map[string]CollectionField{"a": {Type: "string", Enum: []interface{}{"x"}, Fields: map[string]CollectionField{"b": {Type: "int"}}, Items: &CollectionField{Type: "object", Fields: map[string]CollectionField{"i": {Type: "int"}}}}},
		Indexes:     []IndexDefinition{{Keys: IndexKeys{{Field: "a", Value: 1}}, ExpireAfterSeconds: &ttl, PartialFilterExpression: []byte(`{"a":{"$exists":true}}`)}},
		ShardConfig: &ShardConfig{Key: IndexKeys{{Field: "a", Value: "hashed"}}}}}
	cp := CloneCollections(orig)
	if !reflect.DeepEqual(cp, orig) {
		t.Fatalf("clone differs: %+v", cp)
	}
	cp[0].Indexes[0].Keys[0].Value = int32(1)
	*cp[0].Indexes[0].ExpireAfterSeconds = 1
	cp[0].Indexes[0].PartialFilterExpression[0] = '['
	cp[0].ShardConfig.Key[0].Field = "z"
	cp[0].Fields["a"].Enum[0] = "y"
	cp[0].Fields["a"].Fields["b"] = CollectionField{Type: "x"}
	cp[0].Fields["a"].Items.Fields["i"] = CollectionField{Type: "x"}
	o := orig[0]
	if o.Indexes[0].Keys[0].Value != 1 || *o.Indexes[0].ExpireAfterSeconds != 60 || o.Indexes[0].PartialFilterExpression[0] != '{' || o.ShardConfig.Key[0].Field != "a" ||
		o.Fields["a"].Enum[0] != "x" || o.Fields["a"].Fields["b"].Type != "int" || o.Fields["a"].Items.Fields["i"].Type != "int" {
		t.Fatalf("clone shares nested values with the original: %+v", o)
	}
	if CloneCollections(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestWorkloadTargetsEnvAndYAML(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_REPLAY_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_PATH", "rec.ndjson")
	t.Setenv("PLGM_WORKLOAD_RECORD_OUTCOMES", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH", "out.ndjson")
	t.Setenv("PLGM_WORKLOAD_REPLAY_TARGETS", "a=mongodb://h1:27017;b=mongodb://h2:27017")
	t.Setenv("PLGM_WORKLOAD_REPLAY_TARGETS_MODE", "Concurrent")
	t.Setenv("PLGM_WORKLOAD_REPLAY_OPS", "READS_ONLY")
	t.Setenv("PLGM_WORKLOAD_DATA_FINGERPRINT", "dbhash")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkloadReplayTargets) != 2 || cfg.WorkloadReplayTargets[1].Label != "b" || !ConcurrentTargets(cfg) || !ReplayReadsOnly(cfg) || DataFingerprintMode(cfg) != DataFingerprintDBHash {
		t.Fatalf("env not applied: %+v %q %q %q", cfg.WorkloadReplayTargets, cfg.WorkloadReplayTargetsMode, cfg.WorkloadReplayOps, cfg.WorkloadDataFingerprint)
	}

	for _, k := range []string{"PLGM_WORKLOAD_REPLAY_TARGETS", "PLGM_WORKLOAD_REPLAY_TARGETS_MODE", "PLGM_WORKLOAD_REPLAY_OPS", "PLGM_WORKLOAD_DATA_FINGERPRINT"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	yaml := "workload_replay_targets:\n  - label: m8\n    uri: mongodb://h1/\n  - label: m9\n    uri: mongodb://h2/\nworkload_replay_targets_mode: sequential\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadAppConfig(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []ReplayTarget{{Label: "m8", URI: "mongodb://h1/"}, {Label: "m9", URI: "mongodb://h2/"}}; !reflect.DeepEqual(cfg.WorkloadReplayTargets, want) || ConcurrentTargets(cfg) {
		t.Fatalf("yaml targets = %+v mode %q", cfg.WorkloadReplayTargets, cfg.WorkloadReplayTargetsMode)
	}
}

func TestValidateWorkloadTargets(t *testing.T) {
	two := []ReplayTarget{{Label: "m8", URI: "mongodb://h1"}, {Label: "m9", URI: "mongodb://h2"}}
	replay := func(c AppConfig) AppConfig {
		c.WorkloadReplayEnabled, c.WorkloadReplayPath, c.WorkloadRecordOutcomes, c.WorkloadReplayOutcomesPath = true, "r", true, "o.ndjson"
		return c
	}
	det := func(c AppConfig) AppConfig {
		c.WorkloadDeterministic, c.WorkloadRecordOutcomes, c.WorkloadDeterministicOutcomesPath = true, true, "d.ndjson"
		return c
	}
	cases := []struct {
		name string
		cfg  AppConfig
		want string
	}{
		{name: "defaults"},
		{name: "replay targets", cfg: replay(AppConfig{WorkloadReplayTargets: two, WorkloadReplayTargetsMode: "concurrent"})},
		{name: "deterministic targets", cfg: det(AppConfig{WorkloadReplayTargets: two})},
		{name: "reads only replay", cfg: replay(AppConfig{WorkloadReplayOps: "reads_only"})},
		{name: "fingerprint on record", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadDataFingerprint: "counts"}},
		{name: "fingerprint on replay", cfg: replay(AppConfig{WorkloadDataFingerprint: "dbhash"})},
		{name: "bad mode", cfg: AppConfig{WorkloadReplayTargetsMode: "parallel"}, want: "workload_replay_targets_mode"},
		{name: "bad ops", cfg: AppConfig{WorkloadReplayOps: "writes"}, want: "workload_replay_ops must be"},
		{name: "reads only without replay", cfg: AppConfig{WorkloadReplayOps: "reads_only"}, want: "reads_only needs workload_replay_enabled"},
		{name: "bad fingerprint", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadDataFingerprint: "md5"}, want: "workload_data_fingerprint must be"},
		{name: "fingerprint alone", cfg: AppConfig{WorkloadDataFingerprint: "counts"}, want: "needs workload_record_enabled"},
		{name: "one target", cfg: replay(AppConfig{WorkloadReplayTargets: two[:1]}), want: "needs 2 to 8 targets"},
		{name: "too many targets", cfg: replay(AppConfig{WorkloadReplayTargets: make([]ReplayTarget, 9)}), want: "needs 2 to 8 targets"},
		{name: "targets without replay", cfg: AppConfig{WorkloadReplayTargets: two}, want: "needs workload_replay_enabled or workload_deterministic"},
		{name: "targets with recording", cfg: det(AppConfig{WorkloadReplayTargets: two, WorkloadRecordEnabled: true}), want: "cannot be combined with workload_record_enabled"},
		{name: "bad label", cfg: replay(AppConfig{WorkloadReplayTargets: []ReplayTarget{{Label: "a/b", URI: "mongodb://h1"}, two[1]}}), want: "label \"a/b\""},
		{name: "duplicate label", cfg: replay(AppConfig{WorkloadReplayTargets: []ReplayTarget{two[0], {Label: "M8", URI: "mongodb://h3"}}}), want: "used twice"},
		{name: "bad uri", cfg: replay(AppConfig{WorkloadReplayTargets: []ReplayTarget{two[0], {Label: "x", URI: "http://h3"}}}), want: "must start with mongodb://"},
		{name: "duplicate uri", cfg: replay(AppConfig{WorkloadReplayTargets: []ReplayTarget{two[0], {Label: "x", URI: "mongodb://h1"}}}), want: "listed twice"},
		{name: "replay targets without outcomes", cfg: AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: "r", WorkloadReplayTargets: two}, want: "workload_replay_outcomes_path set"},
		{name: "deterministic targets without outcome file", cfg: AppConfig{WorkloadDeterministic: true, WorkloadRecordOutcomes: true, WorkloadReplayTargets: two}, want: "workload_deterministic_outcomes_path set"},
	}
	for _, tc := range cases {
		cfg := tc.cfg
		err := ValidateWorkloadTargets(&cfg)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}
