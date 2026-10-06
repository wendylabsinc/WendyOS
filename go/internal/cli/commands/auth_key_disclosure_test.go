//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// No silent algorithm downgrade (RULINGS 2026-10-04): the operator must be able
// to see which key algorithm a session signs with and which login minted it. A
// legacy session is classical; saying so is the whole point.

func mldsaCertPEM(t *testing.T) string {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func keyDisclosureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := statusConfig(t, time.Now().Add(365*24*time.Hour))
	cfg.Auth = append(cfg.Auth, config.AuthConfig{
		CloudGRPC:    "api.dev.wendy.sh:443",
		OAuthIssuer:  "https://auth.dev.wendy.sh/realms/wendy",
		Certificates: []config.CertificateInfo{{PemCertificate: mldsaCertPEM(t)}},
	})
	return cfg
}

func TestAuthStatusNamesSessionKeyAlgorithmAndPath(t *testing.T) {
	seedConfig(t, keyDisclosureConfig(t))

	out := runAuthStatus(t, false)
	for _, want := range []string{"Key:  ECDSA P-256 (legacy login)", "Key:  ML-DSA-65 (OIDC login)"} {
		if !strings.Contains(out, want) {
			t.Errorf("auth status missing %q, got:\n%s", want, out)
		}
	}
}

func TestAuthStatusJSONNamesSessionKeyAlgorithmAndPath(t *testing.T) {
	seedConfig(t, keyDisclosureConfig(t))

	var got struct {
		Sessions []struct {
			LoginPath   string `json:"loginPath"`
			Certificate struct {
				KeyAlgorithm string `json:"keyAlgorithm"`
			} `json:"certificate"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(runAuthStatus(t, true)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got.Sessions))
	}
	for i, want := range [][2]string{{"legacy", "ECDSA P-256"}, {"OIDC", "ML-DSA-65"}} {
		s := got.Sessions[i]
		if s.LoginPath != want[0] || s.Certificate.KeyAlgorithm != want[1] {
			t.Errorf("session %d = (%q, %q), want (%q, %q)", i, s.LoginPath, s.Certificate.KeyAlgorithm, want[0], want[1])
		}
	}
}

// The legacy login reports the algorithm of the key it actually generated, not
// a constant, so a change to that key shows up in the operator's output.
func TestLegacyLoginSessionReportsKeyAlgorithm(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, issuedCert(), nil)
	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	deliverLegacyCallback(t, session.URL(), url.Values{"token": {fakeEnrollmentToken()}})
	waitLoginDone(t, session)
	if err := session.Err(); err != nil {
		t.Fatal(err)
	}
	if got := session.KeyAlgorithm(); got != "ECDSA P-256" {
		t.Fatalf("KeyAlgorithm() = %q, want ECDSA P-256", got)
	}
}
