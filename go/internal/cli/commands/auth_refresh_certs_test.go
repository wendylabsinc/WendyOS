package commands

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
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const (
	testTenantPrincipal = "spiffe://wendy.sh/tenant/0e6a3d2c-1f4b-4a37-9c8e-2b5d6f7a8c90/service/user-u1"
	testLegacyURN       = "urn:wendy:org:7:user:u1"
)

// leafWithURIs mints a self-signed leaf carrying the given URI SANs. The
// renewal pre-check reads those SANs and nothing else, so this is enough to
// stand in for both a pki-core-issued leaf and a pre-teardown cloud one.
func leafWithURIs(t *testing.T, uris ...string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wendy/user/u1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing SAN %q: %v", raw, err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// cancelledCtx keeps the post-renewal Roughtime proof refresh from reaching the
// network: it is best-effort and fails immediately on an abandoned context.
func cancelledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRefreshCertsForAuthRenewsAgainstPKICore(t *testing.T) {
	t.Setenv(renewEndpointEnv, "https://renew.pki.example:8451/v1/renew")

	var gotEndpoint string
	orig := renewViaPKICore
	renewViaPKICore = func(_ context.Context, endpoint string, _ *config.AuthConfig) (string, string, string, error) {
		gotEndpoint = endpoint
		return "renewed-leaf", "renewed-chain", "renewed-key", nil
	}
	t.Cleanup(func() { renewViaPKICore = orig })

	auth := &config.AuthConfig{
		CloudGRPC: "api.example:443",
		Certificates: []config.CertificateInfo{{
			PemCertificate:      leafWithURIs(t, testTenantPrincipal, testLegacyURN),
			PemCertificateChain: "old-chain",
			PemPrivateKey:       "old-key",
			OrganizationID:      7,
			UserID:              "u1",
			AssetID:             42,
			PrincipalURI:        testTenantPrincipal,
		}},
	}

	if err := refreshCertsForAuth(cancelledCtx(t), auth); err != nil {
		t.Fatalf("refreshCertsForAuth() error = %v", err)
	}

	if gotEndpoint != "https://renew.pki.example:8451/v1/renew" {
		t.Errorf("renewed against %q, want the configured renew frontend", gotEndpoint)
	}
	got := auth.Certificates[0]
	if got.PemCertificate != "renewed-leaf" || got.PemCertificateChain != "renewed-chain" || got.PemPrivateKey != "renewed-key" {
		t.Errorf("renewed material not stored: %+v", got)
	}
	// A renewal replaces key material only. Losing these silently re-labels the
	// session as a different identity for every later command.
	if got.OrganizationID != 7 || got.UserID != "u1" || got.AssetID != 42 || got.PrincipalURI != testTenantPrincipal {
		t.Errorf("identity fields not carried through the renewal: %+v", got)
	}
}

func TestRefreshCertsForAuthRefusesWhatPKICoreCannotRenew(t *testing.T) {
	tests := []struct {
		name     string
		certPEM  string
		endpoint string
		want     error
	}{
		{
			name:     "a leaf with no tenant principal is not pki-core's to renew",
			certPEM:  leafWithURIs(t, testLegacyURN),
			endpoint: "https://renew.pki.example:8451/v1/renew",
			want:     errCertNotPKIIssued,
		},
		{
			name:     "an unparseable certificate carries no principal either",
			certPEM:  "not a pem at all",
			endpoint: "https://renew.pki.example:8451/v1/renew",
			want:     errCertNotPKIIssued,
		},
		{
			name:    "no renew frontend configured",
			certPEM: leafWithURIs(t, testTenantPrincipal),
			want:    errNoRenewEndpoint,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(renewEndpointEnv, tc.endpoint)

			orig := renewViaPKICore
			renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
				t.Fatal("renewal was attempted for a certificate pki-core cannot renew")
				return "", "", "", nil
			}
			t.Cleanup(func() { renewViaPKICore = orig })

			auth := &config.AuthConfig{
				Certificates: []config.CertificateInfo{{PemCertificate: tc.certPEM}},
			}
			err := refreshCertsForAuth(cancelledCtx(t), auth)
			if !errors.Is(err, tc.want) {
				t.Fatalf("refreshCertsForAuth() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// The refusals a renewal cannot recover from have to reach the same re-login
// offer the cloud path had, or `refresh-certs` dead-ends on an expired session.
func TestRenewalRefusalsOfferRelogin(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no tenant lineage", errCertNotPKIIssued, true},
		{"expired certificate", errCertExpired, true},
		{"budget exhausted", renewUnavailableError{reason: "not renewable", needsFreshCert: true}, true},
		{"possession not proven", renewUnavailableError{reason: "not accepted", needsFreshCert: true}, true},
		{"needs an approved grant", renewUnavailableError{reason: "needs a grant"}, false},
		{"frontend was unreachable", renewUnavailableError{reason: "the renew frontend answered 503"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthenticatedCloudError(tc.err); got != tc.want {
				t.Errorf("isUnauthenticatedCloudError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRefreshCertsForAuthDerivesTheRenewEndpoint(t *testing.T) {
	t.Setenv(renewEndpointEnv, "")
	orig := renewViaPKICore
	var endpoint string
	renewViaPKICore = func(_ context.Context, e string, _ *config.AuthConfig) (string, string, string, error) {
		endpoint = e
		return "leaf", "chain", "key", nil
	}
	t.Cleanup(func() { renewViaPKICore = orig })
	auth := &config.AuthConfig{
		CloudGRPC:    "api.dev.wendy.sh:443",
		PKIEndpoint:  "https://identity.dev.pki.wendy.sh/v1/identity/certificate",
		Certificates: []config.CertificateInfo{{PemCertificate: leafWithURIs(t, testTenantPrincipal)}},
	}
	if err := refreshCertsForAuth(cancelledCtx(t), auth); err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://renew.dev.pki.wendy.sh/v1/renew" {
		t.Fatalf("unexpected renewal endpoint %q", endpoint)
	}
}
