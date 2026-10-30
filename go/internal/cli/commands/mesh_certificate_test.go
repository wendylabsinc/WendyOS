package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestMeshCertificateRefreshesSelectedSession(t *testing.T) {
	const tenantA = "11111111-1111-4111-8111-111111111111"
	const tenantB = "22222222-2222-4222-8222-222222222222"
	failure := errors.New("certificate issuance failed")
	for _, test := range []struct {
		name                   string
		lifetime               time.Duration
		refreshErr             error
		wantRenew, wantRefresh bool
	}{
		{name: "fresh", lifetime: 24 * time.Hour},
		{name: "expiring", lifetime: time.Minute, wantRenew: true},
		{name: "expired", lifetime: -time.Hour, wantRefresh: true},
		{name: "issuance failed after token rotation", lifetime: -time.Hour, wantRefresh: true, refreshErr: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := config.AuthConfig{Name: "a", CloudGRPC: "cloud", OAuthIssuer: "https://auth.example", Certificates: []config.CertificateInfo{meshTestCert(tenantA, "operator")}}
			b := config.AuthConfig{Name: "b", CloudGRPC: "cloud", OAuthIssuer: "https://auth.example", RefreshToken: "original", Certificates: []config.CertificateInfo{meshTestCert(tenantB, "operator")}}
			setTempConfig(t, &config.Config{CurrentContext: "b", Auth: []config.AuthConfig{a, b}})
			t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
			oldExpiry, oldRenew, oldRefresh := leafNotAfterFn, renewViaPKICore, refreshDefaultCloudCertificateFn
			t.Cleanup(func() {
				leafNotAfterFn, renewViaPKICore, refreshDefaultCloudCertificateFn = oldExpiry, oldRenew, oldRefresh
			})
			leafNotAfterFn = func(leaf string) (time.Time, error) {
				if leaf == "fresh-leaf" {
					return timeNowFn().Add(24 * time.Hour), nil
				}
				return timeNowFn().Add(test.lifetime), nil
			}
			renews, refreshes := 0, 0
			renewViaPKICore = func(_ context.Context, _ string, auth *config.AuthConfig) (string, string, string, error) {
				renews++
				if auth.Name != "b" {
					t.Fatal("renewed an unselected session")
				}
				return "fresh-leaf", "fresh-chain", "fresh-key", nil
			}
			refreshDefaultCloudCertificateFn = func(_ context.Context, auth *config.AuthConfig) error {
				refreshes++
				if auth.Name != "b" {
					t.Fatal("refreshed an unselected session")
				}
				auth.RefreshToken = "rotated"
				if test.refreshErr != nil {
					return test.refreshErr
				}
				auth.Certificates[0].PemCertificate = "fresh-leaf"
				auth.Certificates[0].PemCertificateChain = "fresh-chain"
				auth.Certificates[0].PemPrivateKey = "fresh-key"
				return nil
			}
			certificate, ok, err := prepareMeshCertificate(context.Background(), "")
			if !errors.Is(err, test.refreshErr) {
				t.Fatalf("err=%v, want %v", err, test.refreshErr)
			}
			if (renews == 1) != test.wantRenew || renews > 1 || (refreshes == 1) != test.wantRefresh || refreshes > 1 {
				t.Fatalf("renews=%d refreshes=%d", renews, refreshes)
			}
			if test.refreshErr != nil {
				if ok || certificate.PemCertificate != "" {
					t.Fatal("failed refresh supplied an expired certificate")
				}
			} else {
				if !ok || certificate.PrincipalURI != b.Certificates[0].PrincipalURI {
					t.Fatal("lost selected operator identity")
				}
				want := "leaf"
				if test.wantRenew || test.wantRefresh {
					want = "fresh-leaf"
				}
				if certificate.PemCertificate != want {
					t.Fatalf("certificate=%q, want %q", certificate.PemCertificate, want)
				}
			}
			stored, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if stored.Auth[0].Certificates[0].PemCertificate != "leaf" {
				t.Fatal("changed an unselected session")
			}
			if test.wantRefresh && stored.Auth[1].RefreshToken != "rotated" {
				t.Fatal("lost rotated refresh token")
			}
			if (test.wantRenew || test.wantRefresh) && test.refreshErr == nil && stored.Auth[1].Certificates[0].PemCertificate != "fresh-leaf" {
				t.Fatal("fresh certificate was not saved")
			}
		})
	}
}

func TestMeshCertificateSkipsUnselectedAndNonOperatorSessions(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	for _, cfg := range []*config.Config{
		{},
		{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{meshTestCert(tenant, "service")}}}},
		{CurrentContext: "missing", Auth: []config.AuthConfig{
			{Name: "a", Certificates: []config.CertificateInfo{meshTestCert(tenant, "operator")}},
			{Name: "b", Certificates: []config.CertificateInfo{meshTestCert(tenant, "operator")}},
		}},
	} {
		setTempConfig(t, cfg)
		oldEnsure := ensureFreshCertificateFn
		ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error {
			t.Fatal("renewed without a selected operator session")
			return nil
		}
		t.Cleanup(func() { ensureFreshCertificateFn = oldEnsure })
		if _, ok, err := prepareMeshCertificate(context.Background(), ""); ok || err != nil {
			t.Fatalf("ok=%t err=%v", ok, err)
		}
	}
}

func TestMeshCertificateCancellationDoesNotSuggestSigningIn(t *testing.T) {
	setTempConfig(t, &config.Config{CurrentContext: "selected", Auth: []config.AuthConfig{{
		Name: "selected", Certificates: []config.CertificateInfo{meshTestCert("11111111-1111-4111-8111-111111111111", "operator")},
	}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err := prepareMeshCertificate(ctx, "")
	if ok || err != context.Canceled {
		t.Fatalf("cancellation should not become an authentication error: ok=%v, err=%v", ok, err)
	}
}
