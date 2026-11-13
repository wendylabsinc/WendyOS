package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestPrepareDefaultCloudCertificateWaitsForAuthRefresh(t *testing.T) {
	for _, issuer := range []string{"", "https://auth.example"} {
		name := "legacy"
		if issuer != "" {
			name = "oauth"
		}
		t.Run(name, func(t *testing.T) {
			auth := pickerAuth(7)
			auth.OAuthIssuer = issuer
			auth.Certificates[0].PemCertificate = "expiring"
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
			oldExpiry, oldRenew := leafNotAfterFn, renewViaPKICore
			t.Cleanup(func() { leafNotAfterFn, renewViaPKICore = oldExpiry, oldRenew })
			leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(time.Minute), nil }
			calls := 0
			renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
				calls++
				return "renewed", "chain", "key", nil
			}
			unlock, err := acquireAuthRefreshLock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := prepareDefaultCloudCertificate(ctx, auth); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("preflight bypassed the active auth refresh: %v", err)
			}
			if calls != 0 {
				t.Errorf("renewal ran %d times while another auth writer held the lock", calls)
			}
		})
	}
}

func TestPrepareDefaultCloudCertificateReloadsBeforeRenewal(t *testing.T) {
	t.Setenv("WENDY_SECRET_STORE", "file")
	auth := pickerAuth(7)
	auth.OAuthIssuer = "https://auth.example"
	auth.RefreshToken = "consumed-token"
	auth.Certificates[0].PemCertificate = "stale-certificate"
	current := *auth
	current.RefreshToken = "current-token"
	current.Certificates = []config.CertificateInfo{{OrganizationID: 7, PemCertificate: "current-certificate"}}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{current}})
	t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
	oldExpiry, oldRenew := leafNotAfterFn, renewViaPKICore
	t.Cleanup(func() { leafNotAfterFn, renewViaPKICore = oldExpiry, oldRenew })
	leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(time.Minute), nil }
	renewViaPKICore = func(_ context.Context, _ string, selected *config.AuthConfig) (string, string, string, error) {
		if selected.RefreshToken != "current-token" || selected.Certificates[0].PemCertificate != "current-certificate" {
			t.Errorf("renewal used a stale session: token=%q, certificate=%q", selected.RefreshToken, selected.Certificates[0].PemCertificate)
		}
		return "renewed", "chain", "key", nil
	}
	if err := prepareDefaultCloudCertificate(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if auth.RefreshToken != "current-token" || auth.Certificates[0].PemCertificate != "renewed" {
		t.Fatal("connection retained credentials from the stale session")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth[0].RefreshToken != "current-token" || cfg.Auth[0].Certificates[0].PemCertificate != "renewed" {
		t.Fatal("renewal lost the stored token or certificate")
	}
}

func requireDefaultCloudAuthRefreshLock(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	unlock, err := acquireAuthRefreshLock(ctx)
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("certificate update did not hold the shared auth refresh lock: %v", err)
	}
}

func TestPrepareDefaultCloudCertificateLocksRenewalPersistence(t *testing.T) {
	auth := pickerAuth(7)
	auth.Certificates[0].PemCertificate = "expiring"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
	oldExpiry, oldRenew, oldPersist := leafNotAfterFn, renewViaPKICore, persistRenewedCertificate
	t.Cleanup(func() {
		leafNotAfterFn, renewViaPKICore, persistRenewedCertificate = oldExpiry, oldRenew, oldPersist
	})
	leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(time.Minute), nil }
	renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
		return "renewed", "chain", "key", nil
	}
	persisted := false
	persistRenewedCertificate = func(selected *config.AuthConfig, updated config.CertificateInfo) error {
		requireDefaultCloudAuthRefreshLock(t)
		persisted = true
		return oldPersist(selected, updated)
	}
	if err := prepareDefaultCloudCertificate(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("renewal did not persist the replacement certificate")
	}
}

func TestPrepareDefaultCloudCertificateKeepsLockForOAuthRecovery(t *testing.T) {
	resetAutomaticCertReissue(t)
	t.Setenv("WENDY_SECRET_STORE", "file")
	auth := pickerAuth(7)
	auth.OAuthIssuer = "https://auth.example"
	auth.RefreshToken = "original-token"
	auth.Certificates[0].PemCertificate = "expired"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	oldExpiry, oldRefresh := leafNotAfterFn, refreshDefaultCloudCertificateFn
	t.Cleanup(func() { leafNotAfterFn, refreshDefaultCloudCertificateFn = oldExpiry, oldRefresh })
	leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(-time.Minute), nil }
	refused := errors.New("identity issuance refused")
	refreshDefaultCloudCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
		requireDefaultCloudAuthRefreshLock(t)
		selected.RefreshToken = "rotated-token"
		return refused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := prepareDefaultCloudCertificate(ctx, auth); !errors.Is(err, refused) {
		t.Fatalf("OAuth recovery did not return its issuance failure: %v", err)
	}
	// The next lock holder must observe the rotated token even when issuance
	// failed after consuming its predecessor.
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth[0].RefreshToken != "rotated-token" {
		t.Fatal("failed certificate issuance lost the rotated OAuth token")
	}
}
