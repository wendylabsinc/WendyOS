package commands

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func resetAutomaticCertReissue(t *testing.T) {
	t.Helper()
	automaticCertReissue.Lock()
	attempted, err := automaticCertReissue.attempted, automaticCertReissue.err
	automaticCertReissue.attempted, automaticCertReissue.err = false, nil
	automaticCertReissue.Unlock()
	t.Cleanup(func() {
		automaticCertReissue.Lock()
		automaticCertReissue.attempted, automaticCertReissue.err = attempted, err
		automaticCertReissue.Unlock()
	})
}

func reissueTestLeaf(t *testing.T, key crypto.Signer, tenant, subject string, from, until time.Time) string {
	t.Helper()
	principal, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/operator/" + subject)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: subject},
		NotBefore: from, NotAfter: until, URIs: []*url.URL{principal},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Real discovery, Auth rotation, signed tokens, CSR issuance and disk writes.
// All credential endpoints are loopback fixtures; counters pin the spend limit.
type reissueTestOptions struct {
	beforeRequest func(*http.Request)
	cloudSubject  string
}

func reissueTestSession(t *testing.T, status int, tenant string, from, until time.Time, options ...reissueTestOptions) (*config.AuthConfig, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var opts reissueTestOptions
	if len(options) > 0 {
		opts = options[0]
	}
	resetAutomaticCertReissue(t)
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv(renewEndpointEnv, "")
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, _ := operatorJWKThumbprint(key)
	authority, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tokens, issues := new(atomic.Int32), new(atomic.Int32)
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.beforeRequest != nil {
			opts.beforeRequest(r)
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(oidcProviderMetadata{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []oidcJWK{testES256JWK(authority, "authority")}})
		case "/token":
			n := tokens.Add(1)
			_ = r.ParseForm()
			if got := r.Form.Get("refresh_token"); got != fmt.Sprintf("refresh-%d", n) {
				t.Errorf("reused/out-of-order refresh token at spend %d", n)
			}
			if status == -1 {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			claims := map[string]any{"iss": issuer, "aud": r.Form.Get("resource"), "sub": testOperatorSubject,
				"exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": thumbprint}}
			if r.Form.Get("resource") == "https://cloud.example/api" && opts.cloudSubject != "" {
				claims["sub"] = opts.cloudSubject
			}
			_ = json.NewEncoder(w).Encode(oidcTokenResponse{AccessToken: testES256Token(t, authority, "authority", claims), RefreshToken: fmt.Sprintf("refresh-%d", n+1), ExpiresIn: 3600, TokenType: "DPoP"})
		case "/v1/identity/certificate":
			issues.Add(1)
			if status != 200 {
				http.Error(w, "identity refused", status)
				return
			}
			_, _ = w.Write([]byte(reissueTestLeaf(t, key, tenant, testOperatorSubject, from, until)))
		default:
			http.NotFound(w, r)
		}
	}))
	issuer = server.URL
	t.Cleanup(server.Close)
	now := time.Now()
	leaf := reissueTestLeaf(t, key, testOperatorTenant, testOperatorSubject, now.Add(-2*time.Hour), now.Add(-time.Hour))
	auth := &config.AuthConfig{Name: "current", CloudGRPC: "cloud.example:443", OAuthIssuer: issuer, OAuthClientID: "wendy-cli", OAuthResource: "https://cloud.example/api", PKIResource: "https://pki.example/identity", PKIEndpoint: issuer + "/v1/identity/certificate", RefreshToken: "refresh-1", DPoPPrivateKey: keyPEM, Certificates: []config.CertificateInfo{{PemCertificate: leaf, PrincipalURI: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject}}}
	setTempConfig(t, &config.Config{CurrentContext: "current", Auth: []config.AuthConfig{*auth}})
	return auth, tokens, issues
}

func TestAutomaticCertificateReissueSpendAndCustody(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name                   string
		status                 int
		tenant                 string
		from, until            time.Time
		wantTokens, wantIssues int
		wantErr, authRefused   bool
	}{
		{"valid replacement", 200, testOperatorTenant, now, now.Add(time.Hour), 2, 1, false, false},
		{"ten second skew", 200, testOperatorTenant, now.Add(10 * time.Second), now.Add(time.Hour), 2, 1, false, false},
		{"one minute skew", 200, testOperatorTenant, now.Add(time.Minute), now.Add(time.Hour), 2, 1, false, false},
		{"excessive skew", 200, testOperatorTenant, now.Add(61 * time.Second), now.Add(time.Hour), 1, 1, true, false},
		{"ten minute policy", 200, testOperatorTenant, now, now.Add(10 * time.Minute), 2, 1, false, false},
		{"fifteen minute policy", 200, testOperatorTenant, now, now.Add(15 * time.Minute), 2, 1, false, false},
		{"one second policy", 200, testOperatorTenant, now, now.Add(time.Second), 2, 1, false, false},
		{"already inside adaptive margin", 200, testOperatorTenant, now.Add(-8 * time.Minute), now.Add(2 * time.Minute), 1, 1, true, false},
		{"expired replacement", 200, testOperatorTenant, now.Add(-time.Hour), now, 1, 1, true, false},
		{"tenant mismatch", 200, "11111111-1111-4111-8111-111111111111", now, now.Add(time.Hour), 1, 1, true, false},
		{"PKI permanent refusal", 403, testOperatorTenant, now, now.Add(time.Hour), 1, 1, true, false},
		{"PKI temporary failure", 503, testOperatorTenant, now, now.Add(time.Hour), 1, 1, true, false},
		{"Auth refusal", -1, testOperatorTenant, now, now.Add(time.Hour), 1, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, tokens, issues := reissueTestSession(t, tc.status, tc.tenant, tc.from, tc.until)
			oldNow := timeNowFn
			timeNowFn = func() time.Time { return now }
			t.Cleanup(func() { timeNowFn = oldNow })
			held := auth.Certificates[0].PemCertificate
			first := prepareDefaultCloudCertificate(context.Background(), auth)
			if (first != nil) != tc.wantErr || isOIDCSignInRefused(first) != tc.authRefused {
				t.Fatalf("outcome=%v", first)
			}
			if tc.name == "tenant mismatch" && !strings.Contains(first.Error(), "while renewing tenant") {
				t.Fatal(first)
			}
			for range 3 {
				_ = prepareDefaultCloudCertificate(context.Background(), auth)
			}
			if int(tokens.Load()) != tc.wantTokens || int(issues.Load()) != tc.wantIssues {
				t.Fatalf("token/issue spends=%d/%d", tokens.Load(), issues.Load())
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantErr && cfg.Auth[0].Certificates[0].PemCertificate != held {
				t.Fatal("invalid response replaced held custody")
			}
			if !tc.wantErr && cfg.Auth[0].Certificates[0].PemCertificate == held {
				t.Fatal("replacement was not saved")
			}
			if tc.status != -1 && cfg.Auth[0].RefreshToken != fmt.Sprintf("refresh-%d", tc.wantTokens+1) {
				t.Fatal("rotated handle lost")
			}
		})
	}
}

func TestOIDCTokenFailuresOnlySuggestLoginWhenAuthRefuses(t *testing.T) {
	for _, tc := range []struct {
		status  int
		code    string
		refused bool
	}{
		{400, "invalid_grant", true}, {400, "access_denied", true}, {401, "", true}, {403, "", true},
		{400, "invalid_resource", false}, {400, "invalid_scope", false}, {429, "", false}, {503, "", false},
	} {
		err := oidcTokenFailure(tc.status, []byte(fmt.Sprintf(`{"error":%q}`, tc.code)))
		if isOIDCSignInRefused(err) != tc.refused || strings.Contains(err.Error(), "wendy auth login") != tc.refused {
			t.Fatalf("unexpected sign-in guidance: %v", err)
		}
	}
}

func TestOIDCConnectionRecoveryIsSilentAndSingleAttempt(t *testing.T) {
	for _, mode := range []string{"interactive", "noninteractive", "json"} {
		t.Run(mode, func(t *testing.T) {
			isolateConnectionRecovery(t)
			auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, time.Now(), time.Now().Add(time.Hour))
			jsonOutput = mode == "json"
			cause := newTLSHandshakeRejectedError(errors.New("certificate is not valid for client authentication"))
			retries := 0
			var err error
			stdout, _ := captureBoth(t, func() {
				_, err = recoverAgentConnection(context.Background(), mode == "noninteractive", cause, func() (*grpcclient.AgentConnection, error) { retries++; return nil, errDeviceIdentityRefused })
			})
			if stdout != "" || !errors.Is(err, errDeviceIdentityRefused) || retries != 1 {
				t.Fatalf("stdout=%q retry=%d err=%v", stdout, retries, err)
			}
			for range 2 {
				_, _ = recoverAgentConnection(context.Background(), false, cause, func() (*grpcclient.AgentConnection, error) { t.Fatal("retry without new custody"); return nil, nil })
			}
			if tokens.Load() != 2 || issues.Load() != 1 {
				t.Fatalf("repeated spends: %d/%d", tokens.Load(), issues.Load())
			}
			_ = auth
		})
	}
}

func TestAutomaticCertificateReissueEligibility(t *testing.T) {
	for _, mode := range []string{"fresh", "held future", "other context", "no current context", "unknown device organization"} {
		t.Run(mode, func(t *testing.T) {
			isolateConnectionRecovery(t)
			auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, time.Now(), time.Now().Add(time.Hour))
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fresh" || mode == "held future" {
				keyPEM, err := auth.OAuthDPoPKey()
				if err != nil {
					t.Fatal(err)
				}
				key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
				if err != nil {
					t.Fatal(err)
				}
				from := time.Now().Add(-time.Hour)
				if mode == "held future" {
					from = time.Now().Add(2 * time.Minute)
				}
				cfg.Auth[0].Certificates[0].PemCertificate = reissueTestLeaf(t, key, testOperatorTenant, testOperatorSubject, from, time.Now().Add(time.Hour))
			} else {
				other := cfg.Auth[0]
				other.Name = "other"
				other.CloudGRPC = "other.example:443"
				cfg.Auth = append(cfg.Auth, other)
				if mode == "other context" {
					cfg.CurrentContext = "other"
				}
				if mode == "no current context" {
					cfg.CurrentContext = ""
				}
			}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if mode == "unknown device organization" {
				cause := chooseRejectionError(context.Background(), 0, []config.CertificateInfo{cfg.Auth[0].Certificates[0], cfg.Auth[1].Certificates[0]}, errors.New("remote error: tls: bad certificate"))
				_, err = recoverAgentConnection(context.Background(), false, cause, func() (*grpcclient.AgentConnection, error) { t.Fatal("retried ambiguous target"); return nil, nil })
				if !strings.Contains(err.Error(), "organization is unknown") {
					t.Fatal(err)
				}
			} else {
				err = prepareDefaultCloudCertificate(context.Background(), auth)
				if mode != "fresh" && err == nil {
					t.Fatal("expected refusal")
				}
			}
			if tokens.Load() != 0 || issues.Load() != 0 {
				t.Fatalf("ineligible context spent: %d/%d", tokens.Load(), issues.Load())
			}
		})
	}
}

func TestAutomaticCertificateReissueKeepsUsableHeldLeaf(t *testing.T) {
	auth, tokens, issues := reissueTestSession(t, 403, testOperatorTenant, time.Now(), time.Now().Add(time.Hour))
	keyPEM, _ := auth.OAuthDPoPKey()
	key, _ := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	auth.Certificates[0].PemCertificate = reissueTestLeaf(t, key, testOperatorTenant, testOperatorSubject, time.Now().Add(-time.Hour), time.Now().Add(5*time.Minute))
	if err := config.Save(&config.Config{CurrentContext: auth.Name, Auth: []config.AuthConfig{*auth}}); err != nil {
		t.Fatal(err)
	}
	held := auth.Certificates[0].PemCertificate
	for range 3 {
		if err := prepareDefaultCloudCertificate(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		if auth.Certificates[0].PemCertificate != held {
			t.Fatal("lost valid held leaf")
		}
	}
	if tokens.Load() != 1 || issues.Load() != 1 {
		t.Fatalf("refusal loop: %d/%d", tokens.Load(), issues.Load())
	}
}

func TestAutomaticCertificateReissuePersistenceFailureIsFatal(t *testing.T) {
	resetAutomaticCertReissue(t)
	auth := pickerAuth(7)
	auth.OAuthIssuer = "https://auth.example"
	auth.Certificates[0].PemCertificate = certPEM(t, time.Now().Add(5*time.Minute))
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	t.Setenv(renewEndpointEnv, "")
	previous := refreshDefaultCloudCertificateFn
	t.Cleanup(func() { refreshDefaultCloudCertificateFn = previous })
	calls := 0
	refreshDefaultCloudCertificateFn = func(context.Context, *config.AuthConfig) error {
		calls++
		// Make the selected session disappear before persistence.
		if err := config.Save(&config.Config{}); err != nil {
			t.Fatal(err)
		}
		return errors.New("PKI refused")
	}
	var persistence certificatePersistenceError
	if err := prepareDefaultCloudCertificate(context.Background(), auth); !errors.As(err, &persistence) {
		t.Fatalf("persistence failure hidden by held validity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestOIDCTokenFailureDoesNotEchoProviderSecretsOrControls(t *testing.T) {
	for _, body := range []string{
		"access-token-secret\n\x1b[31mforged log",
		`{"error":"invalid_grant","error_description":"access-token-secret\n\u001b[31mforged log"}`,
		`{"error":"access-token-secret\n\u001b[31mforged log"}`,
	} {
		err := oidcTokenFailure(400, []byte(body))
		if strings.ContainsAny(err.Error(), "\r\n\x1b") || strings.Contains(err.Error(), "access-token-secret") {
			t.Fatalf("unsafe diagnostic: %q", err.Error())
		}
	}
}

func TestAutomaticCertificateReissueRechecksExpiryAfterFailure(t *testing.T) {
	resetAutomaticCertReissue(t)
	auth := pickerAuth(7)
	auth.OAuthIssuer = "https://auth.example"
	until := time.Now().Add(time.Second)
	auth.Certificates[0].PemCertificate = certPEM(t, until)
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	t.Setenv(renewEndpointEnv, "")
	oldNow, oldRefresh := timeNowFn, refreshDefaultCloudCertificateFn
	t.Cleanup(func() { timeNowFn, refreshDefaultCloudCertificateFn = oldNow, oldRefresh })
	now := until.Add(-time.Second)
	timeNowFn = func() time.Time { return now }
	refused := errors.New("PKI unavailable")
	refreshDefaultCloudCertificateFn = func(context.Context, *config.AuthConfig) error { now = until; return refused }
	if err := prepareDefaultCloudCertificate(context.Background(), auth); !errors.Is(err, refused) {
		t.Fatalf("certificate expired during re-issue but was retained as usable: %v", err)
	}
}

func TestExplicitCertificateRefreshAcceptsShortPolicyLifetime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	_, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, now, now.Add(10*time.Minute))
	oldNow := timeNowFn
	timeNowFn = func() time.Time { return now }
	t.Cleanup(func() { timeNowFn = oldNow })
	for range 2 {
		if err := refreshCerts(context.Background(), false); err != nil {
			t.Fatalf("explicit refresh rejected short tenant policy: %v", err)
		}
	}
	if tokens.Load() != 4 || issues.Load() != 2 {
		t.Fatalf("explicit refresh counts=%d/%d", tokens.Load(), issues.Load())
	}
}

func TestOIDCCertificateRefreshCheckpointsEveryRotation(t *testing.T) {
	for _, cloudSubject := range []string{"", "different-operator"} {
		t.Run("cloud subject "+cloudSubject, func(t *testing.T) {
			checkpoints := new(atomic.Int32)
			opts := reissueTestOptions{cloudSubject: cloudSubject, beforeRequest: func(r *http.Request) {
				if r.URL.Path != "/jwks" && r.URL.Path != "/v1/identity/certificate" {
					return
				}
				cfg, err := config.Load()
				if err != nil {
					t.Error(err)
					return
				}
				if cfg.Auth[0].RefreshToken != "refresh-2" {
					t.Errorf("continued to %s before saving first rotation", r.URL.Path)
				}
				requireDefaultCloudAuthRefreshLock(t)
				checkpoints.Add(1)
			}}
			auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, time.Now(), time.Now().Add(10*time.Minute), opts)
			held := auth.Certificates[0].PemCertificate
			unlock, err := acquireAuthRefreshLock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			// Call the issuer directly so no final caller save can hide a missing
			// checkpoint. A rejected Cloud token must still retain its successor.
			err = refreshOIDCCertificate(context.Background(), auth, false)
			unlock()
			if (err != nil) != (cloudSubject != "") {
				t.Fatalf("issuance result=%v", err)
			}
			cfg, loadErr := config.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if cfg.Auth[0].RefreshToken != "refresh-3" {
				t.Fatal("second rotation was not saved before return/validation")
			}
			if cfg.Auth[0].Certificates[0].PemCertificate != held {
				t.Fatal("rotation checkpoint published uncommitted certificate custody")
			}
			if checkpoints.Load() != 2 || tokens.Load() != 2 || issues.Load() != 1 {
				t.Fatalf("checkpoint/token/issue counts=%d/%d/%d", checkpoints.Load(), tokens.Load(), issues.Load())
			}
		})
	}
}

func TestOIDCCertificateRefreshStopsAtFailedRotationCheckpoint(t *testing.T) {
	for _, resource := range []string{"https://pki.example/identity", "https://cloud.example/api"} {
		t.Run(resource, func(t *testing.T) {
			opts := reissueTestOptions{beforeRequest: func(r *http.Request) {
				if r.URL.Path != "/token" {
					return
				}
				_ = r.ParseForm()
				if r.Form.Get("resource") == resource {
					// Losing the stored session makes the next durable checkpoint fail.
					if err := config.Save(&config.Config{}); err != nil {
						t.Error(err)
					}
				}
			}}
			auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, time.Now(), time.Now().Add(10*time.Minute), opts)
			held := auth.Certificates[0].PemCertificate
			unlock, err := acquireAuthRefreshLock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = refreshOIDCCertificate(context.Background(), auth, false)
			unlock()
			var persistence certificatePersistenceError
			if !errors.As(err, &persistence) {
				t.Fatalf("checkpoint failure was not fatal: %v", err)
			}
			wantTokens, wantIssues := int32(1), int32(0)
			if resource == "https://cloud.example/api" {
				wantTokens, wantIssues = 2, 1
			}
			if tokens.Load() != wantTokens || issues.Load() != wantIssues {
				t.Fatalf("continued past failed checkpoint: token/issue=%d/%d", tokens.Load(), issues.Load())
			}
			if auth.Certificates[0].PemCertificate != held {
				t.Fatal("accepted certificate after failed checkpoint")
			}
		})
	}
}

func TestNonCurrentCertificateReissueIsQuietUntilExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, now, now.Add(time.Hour))
	keyPEM, _ := auth.OAuthDPoPKey()
	key, _ := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	until := now.Add(5 * time.Minute)
	auth.Certificates[0].PemCertificate = reissueTestLeaf(t, key, testOperatorTenant, testOperatorSubject, until.Add(-time.Hour), until)
	other := *auth
	other.Name = "other"
	other.CloudGRPC = "other.example:443"
	if err := config.Save(&config.Config{CurrentContext: "other", Auth: []config.AuthConfig{*auth, other}}); err != nil {
		t.Fatal(err)
	}
	var notices bytes.Buffer
	oldNotice, oldNow := noticeOut, timeNowFn
	noticeOut = &notices
	timeNowFn = func() time.Time { return now }
	t.Cleanup(func() { noticeOut, timeNowFn = oldNotice, oldNow })
	for range 3 {
		stdout, stderr := captureBoth(t, func() {
			if err := prepareDefaultCloudCertificate(context.Background(), auth); err != nil {
				t.Error(err)
			}
		})
		if stdout != "" || stderr != "" || notices.Len() != 0 {
			t.Fatalf("non-current certificate produced a false warning: %q %q %q", stdout, stderr, notices.String())
		}
	}
	now = until
	if err := prepareDefaultCloudCertificate(context.Background(), auth); err == nil || !strings.Contains(err.Error(), "wendy auth use current") {
		t.Fatalf("expired non-current certificate lost switch guidance: %v", err)
	}
	if tokens.Load() != 0 || issues.Load() != 0 {
		t.Fatalf("non-current selector spent authority: %d/%d", tokens.Load(), issues.Load())
	}
}

func TestPickerReissueRetriesRegistryKeyAfterAuthUnlock(t *testing.T) {
	auth, tokens, issues := reissueTestSession(t, 200, testOperatorTenant, time.Now(), time.Now().Add(time.Hour))
	directory := &keyDirectory{}
	auth.CloudGRPC = serveKeyDirectory(t, directory)
	if err := config.Save(&config.Config{CurrentContext: auth.Name, Auth: []config.AuthConfig{*auth}}); err != nil {
		t.Fatal(err)
	}
	held := auth.Certificates[0].PemCertificate
	picked, err := pickAuthEntry(auth.CloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	if picked.Certificates[0].PemCertificate == held || tokens.Load() != 2 || issues.Load() != 1 {
		t.Fatalf("picker did not silently replace the expired leaf: token/issue=%d/%d", tokens.Load(), issues.Load())
	}
	// Registration persists under the auth lock itself. Reaching this point
	// proves certificate preparation released that lock before the retry.
	if directory.calls != 1 || picked.RegistryKeyID == "" || savedRegistryKeyID(t) != picked.RegistryKeyID {
		t.Fatalf("re-issue did not register and save the key: calls=%d", directory.calls)
	}
	if _, err := pickAuthEntry(auth.CloudGRPC); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if directory.calls != 1 || tokens.Load() != 2 || issues.Load() != 1 || cfg.Auth[0].RefreshToken != "refresh-3" {
		t.Fatalf("repeated picker changed key/token custody: registrations=%d token/issue=%d/%d", directory.calls, tokens.Load(), issues.Load())
	}
}
