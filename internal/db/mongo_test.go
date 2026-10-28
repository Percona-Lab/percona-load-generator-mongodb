package db

import (
	"net/url"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func TestBuildMongoURI(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.AppConfig
		want func(t *testing.T, u *url.URL)
	}{
		{
			name: "adds_defaults_and_credentials",
			cfg: &config.AppConfig{
				URI: "mongodb://localhost:27017",
				ConnectionParams: config.ConnectionParams{
					Username:         "user",
					Password:         "pass",
					AuthSource:       "admin",
					ReadPreference:   "nearest",
					ReplicaSetName:   "rs0",
					DirectConnection: true,
				},
			},
			want: func(t *testing.T, u *url.URL) {
				if u.User == nil {
					t.Fatalf("expected credentials to be present")
				}
				if u.User.Username() != "user" {
					t.Fatalf("expected username user, got %q", u.User.Username())
				}
				pw, ok := u.User.Password()
				if !ok || pw != "pass" {
					t.Fatalf("expected password pass")
				}
				q := u.Query()
				if q.Get("authSource") != "admin" || q.Get("readPreference") != "nearest" {
					t.Fatalf("missing connection params in query: %v", q)
				}
				if q.Get("replicaSet") != "rs0" || q.Get("directConnection") != "true" {
					t.Fatalf("expected replicaSet + directConnection in query: %v", q)
				}
				if q.Get("compressors") != "zstd" {
					t.Fatalf("expected default compressor zstd, got %q", q.Get("compressors"))
				}
			},
		},
		{
			name: "custom_compressor_overrides_default",
			cfg: &config.AppConfig{
				URI: "mongodb://localhost:27017",
				CustomParamsMap: map[string]interface{}{
					"compressors": "snappy",
					"retryWrites": true,
				},
			},
			want: func(t *testing.T, u *url.URL) {
				q := u.Query()
				if q.Get("compressors") != "snappy" {
					t.Fatalf("expected custom compressor, got %q", q.Get("compressors"))
				}
				if q.Get("retryWrites") != "true" {
					t.Fatalf("expected retryWrites=true, got %q", q.Get("retryWrites"))
				}
			},
		},
		{
			name: "tls_client_settings_are_added_and_enable_tls",
			cfg: &config.AppConfig{
				URI: "mongodb://localhost:27017",
				ConnectionParams: config.ConnectionParams{
					TLSCAFile:             "/etc/ssl/ca.crt",
					TLSCertificateKeyFile: "/etc/ssl/client.pem",
				},
			},
			want: func(t *testing.T, u *url.URL) {
				q := u.Query()
				if q.Get("tls") != "true" {
					t.Fatalf("expected tls=true when TLS files are configured, got %q", q.Get("tls"))
				}
				if q.Get("tlsCAFile") != "/etc/ssl/ca.crt" {
					t.Fatalf("expected tlsCAFile to be set, got %q", q.Get("tlsCAFile"))
				}
				if q.Get("tlsCertificateKeyFile") != "/etc/ssl/client.pem" {
					t.Fatalf("expected tlsCertificateKeyFile to be set, got %q", q.Get("tlsCertificateKeyFile"))
				}
			},
		},
		{
			name: "tls_defaults_false_without_client_material",
			cfg: &config.AppConfig{
				URI: "mongodb://localhost:27017",
			},
			want: func(t *testing.T, u *url.URL) {
				q := u.Query()
				if q.Get("tls") != "false" {
					t.Fatalf("expected tls=false when no TLS files are configured, got %q", q.Get("tls"))
				}
			},
		},
		{
			name: "invalid_uri",
			cfg:  &config.AppConfig{URI: "://bad-uri"},
			want: func(t *testing.T, u *url.URL) {
				t.Fatalf("unexpected parse for invalid uri: %v", u)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildMongoURI(tc.cfg)
			if tc.name == "invalid_uri" {
				if err == nil {
					t.Fatalf("expected error for invalid URI")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildMongoURI() error = %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse built uri: %v", err)
			}
			tc.want(t, u)
		})
	}
}

// Credentials in the URI take precedence over the separate username and
// password, so each target of a multi-target run can use its own.
func TestBuildMongoURICredentialPrecedence(t *testing.T) {
	cases := []struct {
		name, uri, user, pass string
		wantUser, wantPass    string
		wantHasPass           bool
	}{
		{"uri_credentials_win", "mongodb://alice:a%40pw@h:1/", "plgm", "other", "alice", "a@pw", true},
		{"separate_credentials_without_uri_user", "mongodb://h:1/", "plgm", "pw", "plgm", "pw", true},
		{"uri_user_takes_separate_password", "mongodb://alice@h:1/", "", "pw", "alice", "pw", true},
		{"uri_user_ignores_password_of_another_user", "mongodb://alice@h:1/", "bob", "pw", "alice", "", false},
		{"srv_uri_credentials_win", "mongodb+srv://alice:apw@cluster.example.net/", "plgm", "pw", "alice", "apw", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.AppConfig{URI: tc.uri, ConnectionParams: config.ConnectionParams{Username: tc.user, Password: tc.pass}}
			got, err := BuildMongoURI(cfg)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(got)
			if err != nil || u.User == nil {
				t.Fatalf("built %q: %v", got, err)
			}
			pw, has := u.User.Password()
			if u.User.Username() != tc.wantUser || pw != tc.wantPass || has != tc.wantHasPass {
				t.Fatalf("user %q password %q (set %v), want %q %q (%v)", u.User.Username(), pw, has, tc.wantUser, tc.wantPass, tc.wantHasPass)
			}
		})
	}
}

func TestBuildMongoURIKeepsAuthSourceFromURI(t *testing.T) {
	for uri, want := range map[string]string{"mongodb://u:p@h/?authSource=users": "users", "mongodb://h/": "admin"} {
		got, err := BuildMongoURI(&config.AppConfig{URI: uri, ConnectionParams: config.ConnectionParams{AuthSource: "admin"}})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(got)
		if a := u.Query().Get("authSource"); a != want {
			t.Fatalf("%s: authSource %q, want %q", uri, a, want)
		}
	}
}
