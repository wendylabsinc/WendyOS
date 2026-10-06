package mcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// authTestCertPEM is a self-signed leaf that expires at notAfter.
func authTestCertPEM(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wendy/user/test"},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func authConfigWith(pems ...string) *config.Config {
	cfg := &config.Config{}
	for _, p := range pems {
		cfg.Auth = append(cfg.Auth, config.AuthConfig{Certificates: []config.CertificateInfo{{PemCertificate: p}}})
	}
	return cfg
}

func statusOf(t *testing.T, s *mcpServer) map[string]any {
	t.Helper()
	r, err := s.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil {
		t.Fatal(err)
	}
	return structuredMap(t, r)
}

func TestWendyStatus_AuthLoggedOut(t *testing.T) {
	out := statusOf(t, New(&config.Config{}, nil))
	if out["auth"] != "logged_out" {
		t.Fatalf("auth = %v", out["auth"])
	}
	if !strings.Contains(out["suggested_next_step"].(string), "auth_login") {
		t.Fatalf("a signed-out, unconnected status should point at auth_login: %v", out["suggested_next_step"])
	}
}

func TestWendyStatus_AuthExpired(t *testing.T) {
	expired := authTestCertPEM(t, time.Now().Add(-time.Hour))
	out := statusOf(t, New(authConfigWith(expired), nil))
	if out["auth"] != "expired" {
		t.Fatalf("auth = %v", out["auth"])
	}
	if !strings.Contains(out["suggested_next_step"].(string), "auth_login") {
		t.Fatalf("an expired sign-in should point at auth_login: %v", out["suggested_next_step"])
	}
}

func TestWendyStatus_AuthLoggedInWhenAnySessionValid(t *testing.T) {
	expired := authTestCertPEM(t, time.Now().Add(-time.Hour))
	valid := authTestCertPEM(t, time.Now().Add(24*time.Hour))
	out := statusOf(t, New(authConfigWith(expired, valid), nil))
	if out["auth"] != "logged_in" {
		t.Fatalf("auth = %v", out["auth"])
	}
	if strings.Contains(out["suggested_next_step"].(string), "auth_login") {
		t.Fatalf("a signed-in status should not ask for auth_login: %v", out["suggested_next_step"])
	}
}

// A `wendy auth login` in a terminal (or a finished auth_login) writes
// config.json; status must read it as it is now, not the startup snapshot.
func TestWendyStatus_AuthReadsConfigFromDisk(t *testing.T) {
	valid := authTestCertPEM(t, time.Now().Add(24*time.Hour))
	prev := reloadConfigFn
	reloadConfigFn = func() (*config.Config, error) { return authConfigWith(valid), nil }
	t.Cleanup(func() { reloadConfigFn = prev })

	out := statusOf(t, New(&config.Config{}, nil))
	if out["auth"] != "logged_in" {
		t.Fatalf("auth = %v, want logged_in from the reloaded config", out["auth"])
	}
}

func TestWendyStatus_AuthPendingWhileLoginRuns(t *testing.T) {
	stubLoginBrowser(t, false)
	valid := authTestCertPEM(t, time.Now().Add(24*time.Hour))
	s := New(authConfigWith(valid), nil)
	s.SetLoginStarter((&fakeStarter{}).start)
	callAuthLogin(t, s, context.Background())
	if out := statusOf(t, s); out["auth"] != "pending" {
		t.Fatalf("auth = %v, want pending while a sign-in waits for the browser", out["auth"])
	}
}

// Review Focus 2: an abandoned sign-in reports why, and status leaves pending.
func TestWendyStatus_AuthReportsFailedLogin(t *testing.T) {
	stubLoginBrowser(t, false)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)
	callAuthLogin(t, s, context.Background())
	starter.sessions[0].finish(errors.New("timed out after 5m0s: no browser finished the sign-in"))

	out := statusOf(t, s)
	if out["auth"] != "logged_out" {
		t.Fatalf("auth = %v, want logged_out after the sign-in failed", out["auth"])
	}
	if msg, _ := out["auth_login_error"].(string); !strings.Contains(msg, "timed out") {
		t.Fatalf("auth_login_error = %v", out["auth_login_error"])
	}
}

func TestAuthCertExpired_UnparseableCountsAsValid(t *testing.T) {
	// Same rule as the mTLS ladder (commands.certExpired): an unreadable cert
	// is still attempted, so it does not count as expired here either.
	if authCertExpired(config.CertificateInfo{PemCertificate: "not a pem"}, time.Now()) {
		t.Fatal("unparseable certificate reported as expired")
	}
}
