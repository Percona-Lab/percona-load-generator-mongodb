package webui

import (
	"crypto/x509"
	"testing"
)

func TestGenerateSelfSignedCertHasLoopbackSANs(t *testing.T) {
	cert, err := generateSelfSignedCert()
	if err != nil {
		t.Fatalf("generateSelfSignedCert() error = %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected at least one certificate in the chain")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}

	// Verify hostname verification would succeed for the loopback names the UI
	// is reachable by, so a trusted-cert deployment does not fail on mismatch.
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("VerifyHostname(%q) = %v, want nil", host, err)
		}
	}
}
