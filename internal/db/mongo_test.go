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
