package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestCertNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		notAfter time.Time
		want     bool
	}{
		{"fresh, hours left", now.Add(4 * time.Hour), false},
		{"just outside the lead time", now.Add(renewLeadTime + time.Minute), false},
		{"inside the lead time", now.Add(renewLeadTime - time.Minute), true},
		{"already expired", now.Add(-time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := certNeedsRenewal(tc.notAfter, now); got != tc.want {
				t.Errorf("certNeedsRenewal() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitLeafAndChain(t *testing.T) {
	const leaf = "-----BEGIN CERTIFICATE-----\nQUFB\n-----END CERTIFICATE-----\n"
	const inter = "-----BEGIN CERTIFICATE-----\nQkJC\n-----END CERTIFICATE-----\n"

	t.Run("leaf and chain are split", func(t *testing.T) {
		gotLeaf, gotChain, err := splitLeafAndChain(leaf + inter)
		if err != nil {
			t.Fatalf("splitLeafAndChain: %v", err)
		}
		if gotLeaf != leaf {
			t.Errorf("leaf = %q, want %q", gotLeaf, leaf)
		}
		if gotChain != inter {
			t.Errorf("chain = %q, want %q", gotChain, inter)
		}
	})

	t.Run("leaf only yields an empty chain", func(t *testing.T) {
		gotLeaf, gotChain, err := splitLeafAndChain(leaf)
		if err != nil {
			t.Fatalf("splitLeafAndChain: %v", err)
		}
		if gotLeaf != leaf || gotChain != "" {
			t.Errorf("got (%q, %q), want (leaf, empty)", gotLeaf, gotChain)
		}
	})

	t.Run("no certificate is an error", func(t *testing.T) {
		if _, _, err := splitLeafAndChain("not pem at all"); err == nil {
			t.Fatal("expected an error for a response carrying no certificate")
		}
	})
}

// stubPreflight pins the clock and the renew endpoint, and captures whatever
// the pre-flight persists.
func stubPreflight(t *testing.T, now time.Time, endpoint string) *config.CertificateInfo {
	t.Helper()
	origNow := timeNowFn
	timeNowFn = func() time.Time { return now }
	t.Cleanup(func() { timeNowFn = origNow })

	t.Setenv(renewEndpointEnv, endpoint)

	saved := new(config.CertificateInfo)
	origPersist := persistRenewedCertificate
	persistRenewedCertificate = func(_ *config.AuthConfig, updated config.CertificateInfo) error {
		*saved = updated
		return nil
	}
	t.Cleanup(func() { persistRenewedCertificate = origPersist })
	return saved
}

func stubRenewCall(t *testing.T, certPEM, chainPEM, keyPEM string, err error) *int {
	t.Helper()
	calls := new(int)
	orig := renewViaPKICore
	renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
		*calls++
		return certPEM, chainPEM, keyPEM, err
	}
	t.Cleanup(func() { renewViaPKICore = orig })
	return calls
}

func authWithLeaf() *config.AuthConfig {
	return &config.AuthConfig{
		CloudGRPC:    "api.wendy.sh:443",
		Certificates: []config.CertificateInfo{{PemCertificate: "stored-leaf", OrganizationID: 7}},
	}
}

func TestEnsureFreshCertificate(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	stubLeafExpiry := func(t *testing.T, notAfter time.Time, err error) {
		t.Helper()
		orig := leafNotAfterFn
		leafNotAfterFn = func(string) (time.Time, error) { return notAfter, err }
		t.Cleanup(func() { leafNotAfterFn = orig })
	}

	t.Run("fresh certificate is left alone", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		calls := stubRenewCall(t, "", "", "", nil)
		stubLeafExpiry(t, now.Add(4*time.Hour), nil)

		if err := ensureFreshCertificate(context.Background(), authWithLeaf()); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil", err)
		}
		if *calls != 0 {
			t.Errorf("renew calls = %d, want 0 for a fresh certificate", *calls)
		}
	})

	t.Run("near expiry renews and persists", func(t *testing.T) {
		saved := stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		calls := stubRenewCall(t, "new-leaf", "new-chain", "new-key", nil)
		stubLeafExpiry(t, now.Add(time.Minute), nil)

		auth := authWithLeaf()
		if err := ensureFreshCertificate(context.Background(), auth); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil", err)
		}
		if *calls != 1 {
			t.Fatalf("renew calls = %d, want 1", *calls)
		}
		if saved.PemCertificate != "new-leaf" || saved.PemPrivateKey != "new-key" {
			t.Errorf("persisted %+v, want the renewed material", *saved)
		}
		if auth.Certificates[0].PemCertificate != "new-leaf" {
			t.Error("in-memory auth entry was not updated with the renewed leaf")
		}
		if saved.OrganizationID != 7 {
			t.Errorf("OrganizationID = %d, want 7 carried forward", saved.OrganizationID)
		}
	})

	t.Run("already expired skips the round trip and reports", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		calls := stubRenewCall(t, "", "", "", nil)
		stubLeafExpiry(t, now.Add(-time.Minute), nil)

		err := ensureFreshCertificate(context.Background(), authWithLeaf())
		if !errors.Is(err, errCertExpired) {
			t.Fatalf("ensureFreshCertificate() = %v, want errCertExpired", err)
		}
		if *calls != 0 {
			t.Errorf("renew calls = %d, want 0: past expiry the renew frontend needs a grant", *calls)
		}
	})

	t.Run("no endpoint configured stays silent while still valid", func(t *testing.T) {
		stubPreflight(t, now, "")
		calls := stubRenewCall(t, "", "", "", nil)
		stubLeafExpiry(t, now.Add(time.Minute), nil)

		if err := ensureFreshCertificate(context.Background(), authWithLeaf()); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil when nothing is configured to renew against", err)
		}
		if *calls != 0 {
			t.Errorf("renew calls = %d, want 0", *calls)
		}
	})

	t.Run("denial is surfaced, not swallowed", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		denial := renewUnavailableError{reason: "this certificate is not renewable, or its renewal budget is used up", detail: "renewal budget exhausted"}
		stubRenewCall(t, "", "", "", denial)
		stubLeafExpiry(t, now.Add(time.Minute), nil)

		err := ensureFreshCertificate(context.Background(), authWithLeaf())
		var got renewUnavailableError
		if !errors.As(err, &got) {
			t.Fatalf("ensureFreshCertificate() = %v, want a renewUnavailableError", err)
		}
		if got.detail != "renewal budget exhausted" {
			t.Errorf("detail = %q, want the frontend's own detail carried through", got.detail)
		}
	})

	t.Run("transport failure while still valid does not block the command", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		stubRenewCall(t, "", "", "", errors.New("dial tcp: connection refused"))
		stubLeafExpiry(t, now.Add(time.Minute), nil)

		if err := ensureFreshCertificate(context.Background(), authWithLeaf()); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil: the stored cert still has life left", err)
		}
	})

	t.Run("unparseable stored certificate is not diagnosed here", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		calls := stubRenewCall(t, "", "", "", nil)
		stubLeafExpiry(t, time.Time{}, errors.New("bad pem"))

		if err := ensureFreshCertificate(context.Background(), authWithLeaf()); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil", err)
		}
		if *calls != 0 {
			t.Errorf("renew calls = %d, want 0", *calls)
		}
	})

	t.Run("no certificates is a no-op", func(t *testing.T) {
		stubPreflight(t, now, "https://renew.example:8451/v1/renew")
		if err := ensureFreshCertificate(context.Background(), &config.AuthConfig{}); err != nil {
			t.Fatalf("ensureFreshCertificate() = %v, want nil", err)
		}
		if err := ensureFreshCertificate(context.Background(), nil); err != nil {
			t.Fatalf("ensureFreshCertificate(nil) = %v, want nil", err)
		}
	})
}

// TestRenewStatusMapping pins the HTTP contract of pki-core's renew frontend:
// each status has a different remedy, and collapsing them is what made the old
// behaviour misleading.
func TestRenewStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       any
		wantErr    bool
		wantDetail string
	}{
		{name: "200 returns the renewed chain", status: http.StatusOK,
			body: renewSuccessBody{Certificate: "-----BEGIN CERTIFICATE-----\nQUFB\n-----END CERTIFICATE-----\n"}},
		{name: "202 needs a grant", status: http.StatusAccepted,
			body: map[string]string{"detail": "grant required"}, wantErr: true, wantDetail: "grant required"},
		{name: "403 not renewable or budget used", status: http.StatusForbidden,
			body: map[string]string{"detail": "renewal budget exhausted"}, wantErr: true, wantDetail: "renewal budget exhausted"},
		{name: "401 possession not proven", status: http.StatusUnauthorized,
			body: map[string]string{}, wantErr: true},
		{name: "500 is reported as itself", status: http.StatusInternalServerError,
			body: map[string]string{}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/renew" {
					t.Errorf("path = %q, want /v1/renew", r.URL.Path)
				}
				if r.Method != http.MethodPost {
					t.Errorf("method = %q, want POST", r.Method)
				}
				var got renewRequestBody
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("request body did not decode: %v", err)
				} else if got.CSR == "" {
					t.Error("request carried no csr")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			// Exercise the status mapping without real mTLS: the handshake is
			// pki-core's possession proof and is covered there, not here.
			origClient := renewHTTPClientForFn
			renewHTTPClientForFn = func(config.CertificateInfo) (*http.Client, error) {
				return srv.Client(), nil
			}
			t.Cleanup(func() { renewHTTPClientForFn = origClient })

			origCSR := renewCSRForFn
			renewCSRForFn = func(config.CertificateInfo) (csrPEM, keyPEM string, err error) {
				return "csr-pem", "key-pem", nil
			}
			t.Cleanup(func() { renewCSRForFn = origCSR })

			auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PemCertificate: "stored"}}}
			certPEM, _, keyPEM, err := renewViaPKICoreImpl(context.Background(), srv.URL+"/v1/renew", auth)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for status %d", tc.status)
				}
				var ru renewUnavailableError
				if !errors.As(err, &ru) {
					t.Fatalf("err = %v, want a renewUnavailableError", err)
				}
				if tc.wantDetail != "" && ru.detail != tc.wantDetail {
					t.Errorf("detail = %q, want %q", ru.detail, tc.wantDetail)
				}
				return
			}
			if err != nil {
				t.Fatalf("renewViaPKICore: %v", err)
			}
			if certPEM == "" || keyPEM != "key-pem" {
				t.Errorf("got cert %q key %q, want the renewed leaf and the new key", certPEM, keyPEM)
			}
		})
	}
}

func TestRenewEndpointDerivesTheSessionsOwnPKI(t *testing.T) {
	for _, tc := range []struct {
		name           string
		auth           *config.AuthConfig
		override, want string
	}{
		{name: "cloud host alone derives nothing", auth: &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443"}},
		{name: "any deployment derives its own sibling", auth: &config.AuthConfig{PKIEndpoint: "https://identity.pki.example.com/v1/identity/certificate"}, want: "https://renew.pki.example.com/v1/renew"},
		{name: "port is carried across", auth: &config.AuthConfig{PKIEndpoint: "https://identity.pki.example:8451/v1/identity/certificate"}, want: "https://renew.pki.example:8451/v1/renew"},
		{name: "identity must be its own label", auth: &config.AuthConfig{PKIEndpoint: "https://identityx.pki.example/v1/identity/certificate"}},
		{name: "no host beyond the label", auth: &config.AuthConfig{PKIEndpoint: "https://identity./v1/identity/certificate"}},
		{name: "plaintext derives nothing", auth: &config.AuthConfig{PKIEndpoint: "http://identity.pki.example/v1/identity/certificate"}},
		{name: "userinfo derives nothing", auth: &config.AuthConfig{PKIEndpoint: "https://user@identity.pki.example/v1/identity/certificate"}},
		{name: "identity deployment", auth: &config.AuthConfig{PKIEndpoint: "https://identity.dev.pki.wendy.sh/v1/identity/certificate"}, want: "https://renew.dev.pki.wendy.sh/v1/renew"},
		{name: "explicit override", auth: &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443"}, override: " https://renew.example/v1/renew ", want: "https://renew.example/v1/renew"},
		{name: "cloud host never overrides the session's own PKI", auth: &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443", PKIEndpoint: "https://identity.example/v1/identity/certificate"}, want: "https://renew.example/v1/renew"},
		{name: "a PKI host with no identity label derives nothing", auth: &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443", PKIEndpoint: "https://pki.example/v1/identity/certificate"}},
		{name: "unknown deployment", auth: &config.AuthConfig{CloudGRPC: "api.example:443"}},
		{name: "a lookalike suffix is simply another deployment", auth: &config.AuthConfig{PKIEndpoint: "https://identity.dev.pki.wendy.sh.example/v1/identity/certificate"}, want: "https://renew.dev.pki.wendy.sh.example/v1/renew"},
		{name: "no session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(renewEndpointEnv, tc.override)
			if got := renewEndpoint(tc.auth); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsureFreshCertificateDerivesTheRenewEndpoint(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	stubPreflight(t, now, "")
	origLeaf := leafNotAfterFn
	leafNotAfterFn = func(string) (time.Time, error) { return now.Add(time.Minute), nil }
	t.Cleanup(func() { leafNotAfterFn = origLeaf })
	origRenew := renewViaPKICore
	var endpoint string
	renewViaPKICore = func(_ context.Context, e string, _ *config.AuthConfig) (string, string, string, error) {
		endpoint = e
		return "leaf", "chain", "key", nil
	}
	t.Cleanup(func() { renewViaPKICore = origRenew })
	auth := authWithLeaf()
	auth.PKIEndpoint = "https://identity.dev.pki.wendy.sh/v1/identity/certificate"
	if err := ensureFreshCertificate(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://renew.dev.pki.wendy.sh/v1/renew" {
		t.Fatalf("unexpected renewal endpoint %q", endpoint)
	}
}
