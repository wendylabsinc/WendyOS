package commands

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The tenants of the 2026-10-28 mesh-1 report: the device's ("default"), the
// user's second organization on the same Cloud, and a development login.
const (
	meshTenant  = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	otherTenant = "e5b9b556-a7e2-4a93-baa6-8da537521f0f"
	devTenant   = "5b6b85fc-61bf-49a3-a7ec-eb8bfdde82bd"
)

func tenantCert(t *testing.T, tenant string, notAfter time.Time) config.CertificateInfo {
	t.Helper()
	cert := selfSignedCLICertUntil(t, 0, notAfter)
	cert.PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/operator/3701f33a-3617-4317-8f14-cca2e744a8e2"
	return cert
}

// meshReportConfig stores the logins the user had when mesh-1 refused them:
// the device's tenant expired an hour ago, another organization is valid.
func meshReportConfig(t *testing.T, current string) (cfg *config.Config, expiredAt time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	expiredAt = time.Now().Add(-time.Hour).Truncate(time.Second)
	cfg = &config.Config{
		CurrentContext: current,
		Auth: []config.AuthConfig{
			{Name: "default", CloudGRPC: "api.wendy.dev:443", OAuthIssuer: "https://auth.wendy.dev/realms/wendy", RefreshToken: "refresh", Certificates: []config.CertificateInfo{tenantCert(t, meshTenant, expiredAt)}},
			{Name: "wendy", CloudGRPC: developmentCloudLoginTarget.cloudGRPC, OAuthIssuer: "https://auth.dev.wendy.sh/realms/wendy", RefreshToken: "refresh", Certificates: []config.CertificateInfo{tenantCert(t, devTenant, time.Now().Add(-48*time.Hour))}},
			{Name: "gmail", CloudGRPC: "api.wendy.dev:443", OAuthIssuer: "https://auth.wendy.dev/realms/gmail", RefreshToken: "refresh", Certificates: []config.CertificateInfo{tenantCert(t, otherTenant, time.Now().Add(20*time.Hour))}},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	config.SetOrganizationName("api.wendy.dev:443", meshTenant, "Wendy Labs, Inc.")
	return cfg, expiredAt
}

func storedCerts(cfg *config.Config) []config.CertificateInfo {
	var all []config.CertificateInfo
	for _, auth := range cfg.Auth {
		all = append(all, auth.Certificates[0])
	}
	return all
}

// The report's cause: a tenant device reports org 0, and an org-0 diagnosis
// weighed every login, so the valid login for another tenant hid the device
// tenant's expired one and the refusal read as a device clock problem. The
// device's own tenant now selects the login, and its expiry is said plainly.
func TestExpiredTenantLoginIsNotReportedAsAClockError(t *testing.T) {
	cfg, expiredAt := meshReportConfig(t, "default")
	bad := errors.New("remote error: tls: bad certificate")

	before := chooseRejectionError(context.Background(), 0, storedCerts(cfg), bad)
	if expiredLoginFrom(before) != nil || certificateRefreshReason(before) != "" || !strings.Contains(before.Error(), "device clock") {
		t.Fatalf("org-only diagnosis changed; want the old clock wording without a tenant: %v", before)
	}

	err := chooseRejectionErrorForScope(context.Background(), certs.Scope{TenantUUID: meshTenant}, storedCerts(cfg), bad)
	login := expiredLoginFrom(err)
	if login == nil {
		t.Fatalf("expired login not found: %v", err)
	}
	want := "Your Wendy login for Wendy Labs, Inc. (auth context \"default\") expired at " + expiredAt.UTC().Format("2006-01-02 15:04 UTC") + ".\n  Run 'wendy auth login' and sign in to Wendy Labs, Inc., then retry."
	if err.Error() != want {
		t.Fatalf("message:\n%s\nwant:\n%s", err.Error(), want)
	}
	if steps := err.(interface{ NextSteps() []string }).NextSteps(); len(steps) != 1 || !strings.Contains(steps[0], "wendy auth login") {
		t.Fatalf("next steps = %q", steps)
	}
	for _, forbidden := range []string{"clock", "sync-time", "refresh-certs"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("expired login described with %q: %v", forbidden, err)
		}
	}
	if !login.renewable || login.context != "default" {
		t.Fatalf("login = %+v, want the renewable default context", login)
	}
	if got := ErrorClass(err); got != "auth_login_expired" {
		t.Fatalf("ErrorClass = %q", got)
	}
	// Terminal for the entry points: neither another address, the default
	// device picker nor a Cloud name lookup can help a reachable device.
	if !errors.Is(err, errProvisionedAgentUnauthorized) || !errors.Is(err, errTLSHandshakeRejected) {
		t.Fatalf("classification lost: %v", err)
	}
	if failure := classifyLANProbeFailure(err); failure.Code != "login_expired" || !strings.Contains(failure.Message, "expired at") {
		t.Fatalf("discover row = %+v", failure)
	}

	// A development login names the flag that reaches its Cloud.
	dev := expiredLoginFrom(chooseRejectionErrorForScope(context.Background(), certs.Scope{TenantUUID: devTenant}, storedCerts(cfg), bad))
	if dev == nil || !strings.Contains(dev.loginStep, "wendy auth login --development") {
		t.Fatalf("development login = %+v", dev)
	}
	// The device's tenant holds a valid login: expiry is not why it refused.
	if got := chooseRejectionErrorForScope(context.Background(), certs.Scope{TenantUUID: otherTenant}, storedCerts(cfg), bad); expiredLoginFrom(got) != nil {
		t.Fatalf("valid login reported as expired: %v", got)
	}
	// No login for the device's tenant at all: not an expired login either.
	if got := chooseRejectionErrorForScope(context.Background(), certs.Scope{TenantUUID: "00000000-0000-4000-8000-000000000000"}, storedCerts(cfg), bad); expiredLoginFrom(got) != nil {
		t.Fatalf("unrelated logins reported as the device's: %v", got)
	}
}

// A picked device the CLI reached before is pinned. When its candidate walk
// ends in the pinned refusal (some address rejected our certificate, but not
// the primary one), the pinned tenant's expired login still explains it, and
// the refusal stays in the chain so the Bluetooth fallback stays blocked.
func TestPinnedRefusalCarriesTheExpiredLogin(t *testing.T) {
	cfg, _ := meshReportConfig(t, "default")
	now := time.Now()
	rejected := &noAuthenticatedEndpointError{msg: "pinned, tls: bad certificate", certSeen: true}

	err := pinnedRefusalError(rejected, meshTenant, storedCerts(cfg), now)
	if login := expiredLoginFrom(err); login == nil || login.context != "default" {
		t.Fatalf("expired login not attached: %v", err)
	}
	if !blocksUnauthenticatedFallback(err) || !errors.Is(err, errProvisionedAgentUnauthorized) {
		t.Fatalf("pinned refusal lost from the chain: %v", err)
	}
	if strings.Contains(err.Error(), "clock") || !strings.Contains(err.Error(), "wendy auth login") {
		t.Fatalf("message = %q", err.Error())
	}

	// Unchanged when nothing rejected our certificate, when the pinned tenant
	// holds a valid login, and when the pin names no tenant.
	silent := &noAuthenticatedEndpointError{msg: "pinned, silent"}
	for name, got := range map[string]error{
		"silent":      pinnedRefusalError(silent, meshTenant, storedCerts(cfg), now),
		"valid login": pinnedRefusalError(rejected, otherTenant, storedCerts(cfg), now),
		"no tenant":   pinnedRefusalError(rejected, "", storedCerts(cfg), now),
	} {
		if expiredLoginFrom(got) != nil {
			t.Fatalf("%s: expired login attached: %v", name, got)
		}
	}
}

// End to end through the dial ladder against a device whose certificate names
// its tenant: the transport offers only the other tenant's valid certificate,
// as it did for mesh-1, and the diagnosis still finds the device's expired
// login from the tenant the device presented.
func TestDialLadderFindsTheDeviceTenantsExpiredLogin(t *testing.T) {
	server := selfSignedCLICert(t, 0)
	keyPair, err := tls.X509KeyPair([]byte(server.PemCertificate), []byte(server.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	principal, err := url.Parse("spiffe://wendy.sh/tenant/" + meshTenant + "/device/mesh-1")
	if err != nil {
		t.Fatal(err)
	}
	leaf.URIs = []*url.URL{principal}
	keyPair.Certificate[0], err = x509.CreateCertificate(rand.Reader, leaf, leaf, leaf.PublicKey, keyPair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	addr := serveRecoveryRejection(t, func(conn net.Conn) {
		attempts.Add(1)
		_ = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{keyPair}, NextProtos: []string{"h2"}}).Handshake()
	})
	cfg, _ := meshReportConfig(t, "default")
	setPinCache(t)
	withTempCertOrderCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := dialAgentLadderWithCerts(ctx, newDialTarget("mesh-1.local", addr), storedCerts(cfg))
	if conn != nil {
		conn.Close()
		t.Fatal("a refused handshake returned a connection")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("TLS attempts = %d, want 1: expired logins stay out of the transport", got)
	}
	if login := expiredLoginFrom(err); login == nil || login.context != "default" {
		t.Fatalf("expired login not diagnosed from the device's tenant: %v", err)
	}
}

// Recovery renews the expired login with its refresh token and retries the same
// endpoint once. No time proof is ever sent for it, and when renewal is not
// possible the error names the one next step.
func TestExpiredLoginRecoveryRenewsInsteadOfSyncingTheClock(t *testing.T) {
	for _, tc := range []struct {
		name       string
		renewable  bool
		renewErr   error
		retryErr   error
		wantRenew  int
		wantRetry  int
		wantStderr []string
	}{
		{"renewed and connected", true, nil, nil, 1, 1, []string{"expired at", "Renewing it...", "Login renewed. Connected."}},
		{"renewal refused", true, errors.New("refresh token expired"), nil, 1, 0, []string{"Could not renew the login: refresh token expired"}},
		{"not renewable", false, nil, nil, 0, 0, nil},
		{"renewed but refused again", true, nil, errDeviceIdentityRefused, 1, 1, []string{"Login renewed, but the connection retry failed."}},
	} {
		for _, entry := range []string{"named", "picked"} {
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				isolateConnectionRecovery(t) // fails on any time proof or refresh prompt
				oldRenew, oldAttempted := renewExpiredLoginFn, expiredLoginRenewalAttempted
				t.Cleanup(func() { renewExpiredLoginFn, expiredLoginRenewalAttempted = oldRenew, oldAttempted })
				expiredLoginRenewalAttempted = false
				login := &expiredLogin{tenant: meshTenant, context: "default", label: "Wendy Labs, Inc.", notAfter: time.Now().Add(-time.Hour), renewable: tc.renewable, loginStep: "Run 'wendy auth login' and sign in to Wendy Labs, Inc., then retry."}
				cause := tlsHandshakeRejectedError{cause: errors.New("remote error: tls: bad certificate"), expiredLogin: login}
				renewals, retries := 0, 0
				renewExpiredLoginFn = func(_ context.Context, got *expiredLogin) error {
					if got != login {
						t.Fatalf("renewed %+v, want the device's login", got)
					}
					renewals++
					return tc.renewErr
				}
				wantConn := &grpcclient.AgentConnection{Host: "mesh-1"}
				retry := func() (*grpcclient.AgentConnection, error) {
					retries++
					if tc.retryErr != nil {
						return nil, tc.retryErr
					}
					return wantConn, nil
				}
				var conn *grpcclient.AgentConnection
				var err error
				stdout, stderr := captureBoth(t, func() {
					if entry == "named" {
						conn, err = recoverAgentConnection(context.Background(), false, cause, retry)
					} else {
						conn, err = recoverRejectedHandshake(context.Background(), cause, retry)
					}
				})
				if stdout != "" {
					t.Fatalf("recovery wrote to stdout: %q", stdout)
				}
				if renewals != tc.wantRenew || retries != tc.wantRetry {
					t.Fatalf("renewals=%d retries=%d", renewals, retries)
				}
				switch {
				case tc.retryErr != nil:
					if !errors.Is(err, tc.retryErr) {
						t.Fatalf("latest failure lost: %v", err)
					}
				case tc.wantRetry == 1:
					if err != nil || conn != wantConn {
						t.Fatalf("conn=%v err=%v", conn, err)
					}
				default:
					if conn != nil || expiredLoginFrom(err) != login || !strings.Contains(err.Error(), "wendy auth login") {
						t.Fatalf("conn=%v err=%v, want the expired login and its next step", conn, err)
					}
				}
				for _, want := range tc.wantStderr {
					if !strings.Contains(stderr, want) {
						t.Fatalf("stderr missing %q:\n%s", want, stderr)
					}
				}
				if strings.Contains(stderr, "clock") {
					t.Fatalf("expired login described as a clock problem:\n%s", stderr)
				}
			})
		}
	}
}

// renewExpiredLogin renews the login the device needed, not the current
// context (WDY-3864), keeps the other sessions as they were, and does nothing
// when another process already renewed it.
func TestRenewExpiredLoginRenewsOnlyThatLogin(t *testing.T) {
	cfg, _ := meshReportConfig(t, "gmail")
	login := expiredLoginForTenant(meshTenant, storedCerts(cfg), time.Now())
	if login == nil {
		t.Fatal("expired login not found")
	}
	oldRefresh := refreshCertsForAuthFn
	t.Cleanup(func() { refreshCertsForAuthFn = oldRefresh })
	renewed := tenantCert(t, meshTenant, time.Now().Add(24*time.Hour))
	var refreshed []string
	refreshCertsForAuthFn = func(_ context.Context, auth *config.AuthConfig) error {
		refreshed = append(refreshed, auth.Name)
		auth.Certificates = []config.CertificateInfo{renewed}
		return nil
	}
	if err := renewExpiredLogin(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 1 || refreshed[0] != "default" {
		t.Fatalf("refreshed %q, want only the device's login", refreshed)
	}
	stored, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.CurrentContext != "gmail" {
		t.Fatalf("current context changed to %q", stored.CurrentContext)
	}
	for i, auth := range stored.Auth {
		want := cfg.Auth[i].Certificates[0].PemCertificate
		if auth.Name == "default" {
			want = renewed.PemCertificate
		}
		if auth.Certificates[0].PemCertificate != want {
			t.Fatalf("context %q certificate not as expected after renewal", auth.Name)
		}
	}

	// Renewed meanwhile by another process: no second refresh token is spent.
	if err := renewExpiredLogin(context.Background(), login); err != nil || len(refreshed) != 1 {
		t.Fatalf("err=%v refreshed=%q", err, refreshed)
	}
}

// A login the CLI cannot renew silently is never sent to the refresh path.
func TestRenewExpiredLoginRefusesASessionWithoutRefreshToken(t *testing.T) {
	cfg, _ := meshReportConfig(t, "default")
	cfg.Auth[0].OAuthIssuer = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	login := expiredLoginForTenant(meshTenant, storedCerts(cfg), time.Now())
	if login == nil || login.renewable {
		t.Fatalf("login = %+v, want a non-renewable expired login", login)
	}
	oldRefresh := refreshCertsForAuthFn
	t.Cleanup(func() { refreshCertsForAuthFn = oldRefresh })
	refreshCertsForAuthFn = func(context.Context, *config.AuthConfig) error {
		t.Fatal("refreshed a login without a refresh token")
		return nil
	}
	if err := renewExpiredLogin(context.Background(), login); err == nil {
		t.Fatal("renewal of a non-OAuth login succeeded")
	}
}
