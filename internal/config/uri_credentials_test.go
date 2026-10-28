package config

import (
	"reflect"
	"testing"
)

func TestURICredentials(t *testing.T) {
	cases := map[string]struct {
		user    string
		hasPass bool
	}{
		"mongodb://u:p@h:1/":               {"u", true},
		"mongodb+srv://u:p%3A@c.example/":  {"u", true},
		"mongodb://u@h:1/":                 {"u", false},
		"mongodb://h:1/":                   {"", false},
		"  mongodb://u:p@h1:1,h2:2/?rs=a ": {"u", true},
		"://bad":                           {"", false},
	}
	for uri, want := range cases {
		user, hasPass := URICredentials(uri)
		if user != want.user || hasPass != want.hasPass {
			t.Errorf("URICredentials(%q) = %q, %v; want %q, %v", uri, user, hasPass, want.user, want.hasPass)
		}
	}
}

func TestSeparateCredentialsNeeded(t *testing.T) {
	cfg := &AppConfig{URI: "mongodb://u:p@h/"}
	if u, p := SeparateCredentialsNeeded(cfg); u || p {
		t.Fatalf("a URI with credentials needs no separate ones: %v %v", u, p)
	}
	// The single URI is ignored when targets are listed.
	cfg.WorkloadReplayTargets = []ReplayTarget{{Label: "a", URI: "mongodb://a:pa@a/"}, {Label: "b", URI: "mongodb://b@b/"}}
	if u, p := SeparateCredentialsNeeded(cfg); u || !p {
		t.Fatalf("target b has no password: %v %v", u, p)
	}
	cfg.WorkloadReplayTargets[1].URI = "mongodb://b/"
	if u, p := SeparateCredentialsNeeded(cfg); !u || !p {
		t.Fatalf("target b has no credentials: %v %v", u, p)
	}
}

func TestCredentialsOverriddenByURI(t *testing.T) {
	cfg := &AppConfig{URI: "mongodb://u:p@h/"}
	if got := CredentialsOverriddenByURI(cfg); got != nil {
		t.Fatalf("no separate credentials, nothing overridden: %v", got)
	}
	cfg.ConnectionParams.Username = "u"
	if got := CredentialsOverriddenByURI(cfg); got != nil {
		t.Fatalf("the same username alone changes nothing: %v", got)
	}
	cfg.ConnectionParams.Password = "x"
	if got := CredentialsOverriddenByURI(cfg); !reflect.DeepEqual(got, []string{"MongoDB URI"}) {
		t.Fatalf("got %v", got)
	}
	cfg.ConnectionParams = ConnectionParams{Username: "plgm"}
	cfg.WorkloadReplayTargets = []ReplayTarget{{Label: "m8", URI: "mongodb://a:pa@a/"}, {Label: "m9", URI: "mongodb://b/"}, {Label: "m10", URI: "mongodb://c:pc@c/"}}
	if got := CredentialsOverriddenByURI(cfg); !reflect.DeepEqual(got, []string{"m8", "m10"}) {
		t.Fatalf("got %v", got)
	}
}
