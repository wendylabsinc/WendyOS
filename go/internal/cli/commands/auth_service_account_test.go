package commands

import (
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const testSASubject = "5b0c8f7e-0000-4000-8000-00000000c1a0"

// fakeAccessToken is an unsigned JWS carrying claims; the CLI only decodes it.
func fakeAccessToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func decodeJWSPart(t *testing.T, part string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decoding JWS part: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parsing JWS part: %v", err)
	}
	return out
}

// fakeSAAuth is a wendy-auth realm that implements the service-account wire
// contract: discovery, the jwt-bearer assertion grant, and key enrollment.
// It verifies every assertion against pub, the key "registered" for the SA.
type fakeSAAuth struct {
	server *httptest.Server
	issuer string
	pub    *mldsa.PublicKey
	mints  atomic.Int32
}

func newFakeSAAuth(t *testing.T, pub *mldsa.PublicKey) *fakeSAAuth {
	t.Helper()
	f := &fakeSAAuth{pub: pub}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /realms/acme/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 f.issuer,
			"authorization_endpoint": f.issuer + "/oauth2/authorize",
			"token_endpoint":         f.issuer + "/oauth2/token",
		})
	})
	mux.HandleFunc("POST /realms/acme/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", got)
		}
		if r.Form.Has("client_id") {
			t.Error("assertion grant must not send client_id")
		}
		resource := r.Form.Get("resource")
		if resource != defaultDevCloudResource {
			t.Errorf("resource = %q", resource)
		}
		kid := f.verifyAssertion(t, r.Form.Get("assertion"))
		f.mints.Add(1)
		writeSAToken(t, w, f.issuer, kid, resource)
	})
	mux.HandleFunc("POST /realms/acme/service-accounts/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret    string            `json:"enrollment_secret"`
			PublicJWK map[string]string `json:"public_jwk"`
			Pop       string            `json:"pop"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding enroll body: %v", err)
		}
		if body.Secret != "one-time-secret" {
			t.Errorf("enrollment_secret = %q", body.Secret)
		}
		parts := strings.Split(body.Pop, ".")
		if len(parts) != 3 {
			t.Fatalf("pop is not a compact JWS")
		}
		header, claims := decodeJWSPart(t, parts[0]), decodeJWSPart(t, parts[1])
		if header["typ"] != "dpop+jwt" || header["alg"] != "ML-DSA-65" {
			t.Errorf("pop header = %v", header)
		}
		if claims["htm"] != "POST" || claims["htu"] != f.issuer+"/service-accounts/enroll" {
			t.Errorf("pop htm/htu = %v %v", claims["htm"], claims["htu"])
		}
		jwk, _ := header["jwk"].(map[string]any)
		if jwk["pub"] != body.PublicJWK["pub"] || body.PublicJWK["alg"] != "ML-DSA-65" || body.PublicJWK["kty"] != "AKP" {
			t.Errorf("pop jwk %v does not match public_jwk %v", jwk, body.PublicJWK)
		}
		pubBytes, err := base64.RawURLEncoding.DecodeString(body.PublicJWK["pub"])
		if err != nil {
			t.Fatal(err)
		}
		pub, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pubBytes)
		if err != nil {
			t.Fatalf("public_jwk pub: %v", err)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := mldsa.Verify(pub, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
			t.Errorf("pop signature: %v", err)
		}
		f.pub = pub
		writeSAToken(t, w, f.issuer, mldsaThumbprint(t, pub), "")
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	f.issuer = f.server.URL + "/realms/acme"
	return f
}

// verifyAssertion applies wendy-auth's ServiceAccountAssertionGrant checks and
// returns the kid, which the server binds into cnf.jkt.
func (f *fakeSAAuth) verifyAssertion(t *testing.T, assertion string) string {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion is not a compact JWS")
	}
	header, claims := decodeJWSPart(t, parts[0]), decodeJWSPart(t, parts[1])
	if header["alg"] != "ML-DSA-65" {
		t.Errorf("assertion alg = %v", header["alg"])
	}
	kid, _ := header["kid"].(string)
	if want := mldsaThumbprint(t, f.pub); kid != want {
		t.Errorf("assertion kid = %q, want registered key thumbprint %q", kid, want)
	}
	if claims["iss"] != testSASubject || claims["sub"] != testSASubject {
		t.Errorf("assertion iss/sub = %v/%v", claims["iss"], claims["sub"])
	}
	if claims["aud"] != f.issuer+"/oauth2/token" {
		t.Errorf("assertion aud = %v", claims["aud"])
	}
	if jti, _ := claims["jti"].(string); jti == "" {
		t.Error("assertion carries no jti")
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if d := time.Since(time.Unix(int64(iat), 0)); d < -time.Minute || d > time.Minute {
		t.Errorf("assertion iat off by %s", d)
	}
	if exp <= iat || exp-iat > 300 {
		t.Errorf("assertion exp-iat = %v", exp-iat)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := mldsa.Verify(f.pub, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		t.Errorf("assertion signature: %v", err)
	}
	return kid
}

func mldsaThumbprint(t *testing.T, pub *mldsa.PublicKey) string {
	t.Helper()
	canonical := `{"alg":"ML-DSA-65","kty":"AKP","pub":"` + base64.RawURLEncoding.EncodeToString(pub.Bytes()) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeSAToken(t *testing.T, w http.ResponseWriter, issuer, kid, resource string) {
	t.Helper()
	claims := map[string]any{
		"iss": issuer, "sub": testSASubject, "principal_kind": "service",
		"cnf": map[string]any{"jkt": kid}, "exp": time.Now().Add(time.Hour).Unix(),
	}
	if resource != "" {
		claims["aud"] = resource
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fakeAccessToken(t, claims), "token_type": "DPoP", "expires_in": 3600,
	})
}

// newTestSAKeyFile returns a service-account key file for the fake realm.
func newTestSAKeyFile(t *testing.T, issuer string) ([]byte, *mldsa.PublicKey) {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(serviceAccountKey{Issuer: issuer, Subject: testSASubject, PrivateKey: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	return data, signer.Public().(*mldsa.PublicKey)
}

func TestMintServiceAccountTokenAssertionShape(t *testing.T) {
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeSAAuth(t, signer.Public().(*mldsa.PublicKey))

	token, err := mintServiceAccountToken(context.Background(), signer, f.issuer, testSASubject, defaultDevCloudResource)
	if err != nil {
		t.Fatalf("mintServiceAccountToken: %v", err)
	}
	if token.AccessToken == "" || token.RefreshToken != "" {
		t.Fatalf("unexpected token response: %+v", token)
	}
	// jti is single-use at the server: a second mint must carry a fresh one.
	if _, err := mintServiceAccountToken(context.Background(), signer, f.issuer, testSASubject, defaultDevCloudResource); err != nil {
		t.Fatalf("second mint: %v", err)
	}
}

// A CI host has no terminal and no browser: the service-account login must
// complete without either and leave a DPoP-bound session behind.
func TestPerformServiceAccountLoginHeadless(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	stubNonInteractive(t)
	stubHumanPresent(t, false)
	opened := stubOpenBrowser(t)

	f := newFakeSAAuth(t, nil)
	keyFile, pub := newTestSAKeyFile(t, f.issuer)
	f.pub = pub

	captureStdout(t, func() {
		err := performServiceAccountLogin(context.Background(), serviceAccountLoginOptions{
			KeyFile:   keyFile,
			CloudURL:  defaultDevCloudDashboard,
			CloudGRPC: defaultDevCloudGRPC,
			Resource:  defaultDevCloudResource,
		})
		if err != nil {
			t.Fatalf("performServiceAccountLogin: %v", err)
		}
	})
	if urls := opened(); len(urls) != 0 {
		t.Fatalf("headless login opened a browser: %v", urls)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("stored %d sessions, want 1", len(cfg.Auth))
	}
	auth := cfg.Auth[0]
	if !cloudrequest.IsDPoPBound(&auth) {
		t.Fatal("service-account session is not DPoP-bound; it would be sent as Bearer")
	}
	if auth.ServiceAccount != testSASubject || auth.OAuthIssuer != f.issuer || auth.OAuthResource != defaultDevCloudResource {
		t.Fatalf("stored session = %+v", auth)
	}
	if auth.RefreshToken != "" || auth.APIKey == "" || auth.OAuthExpiresAt == "" {
		t.Fatalf("stored token state = %+v", auth)
	}
	if cfg.CurrentContext == "" {
		t.Fatal("first login did not become the current context")
	}
}

// No refresh token exists for a service account: an expired session is
// renewed by minting a new assertion with the stored key.
func TestEnsureOAuthAccessTokenRemintsServiceAccount(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	f := newFakeSAAuth(t, nil)
	keyFile, pub := newTestSAKeyFile(t, f.issuer)
	f.pub = pub
	var key serviceAccountKey
	if err := json.Unmarshal(keyFile, &key); err != nil {
		t.Fatal(err)
	}
	auth := config.AuthConfig{
		CloudDashboard: defaultDevCloudDashboard,
		CloudGRPC:      defaultDevCloudGRPC,
		APIKey:         "expired-token",
		OAuthIssuer:    f.issuer,
		OAuthResource:  defaultDevCloudResource,
		OAuthExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		DPoPPrivateKey: key.PrivateKey,
		ServiceAccount: testSASubject,
	}
	cfg := &config.Config{}
	cfg.AddAuth(auth)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	if err := ensureOAuthAccessToken(context.Background(), &auth); err != nil {
		t.Fatalf("ensureOAuthAccessToken: %v", err)
	}
	if f.mints.Load() != 1 || auth.APIKey == "expired-token" {
		t.Fatalf("mints = %d, token = %q", f.mints.Load(), auth.APIKey)
	}
	stored, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth[0].APIKey != auth.APIKey {
		t.Fatal("re-minted token was not persisted")
	}
}

func TestEnrollServiceAccountWritesKeyFile(t *testing.T) {
	f := newFakeSAAuth(t, nil)
	out := filepath.Join(t.TempDir(), "sa.json")

	captureStdout(t, func() {
		if err := enrollServiceAccount(context.Background(), f.issuer, "one-time-secret", out); err != nil {
			t.Fatalf("enrollServiceAccount: %v", err)
		}
	})

	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	key, signer, err := parseServiceAccountKey(data)
	if err != nil {
		t.Fatalf("parseServiceAccountKey: %v", err)
	}
	if key.Issuer != f.issuer || key.Subject != testSASubject {
		t.Fatalf("key file = %+v", key)
	}
	// The written key must be the one the server registered.
	if !signer.Public().(*mldsa.PublicKey).Equal(f.pub) {
		t.Fatal("key file holds a different key than the one enrolled")
	}

	// The enrolled key logs in straight away.
	if _, err := mintServiceAccountToken(context.Background(), signer, key.Issuer, key.Subject, defaultDevCloudResource); err != nil {
		t.Fatalf("minting with the enrolled key: %v", err)
	}
}

func TestEnrollServiceAccountRefusesToOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(out, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Fails before any network call: the issuer is unreachable on purpose.
	if err := enrollServiceAccount(context.Background(), "http://127.0.0.1:1/realms/acme", "s", out); err == nil {
		t.Fatal("enroll overwrote an existing key file")
	}
	if data, _ := os.ReadFile(out); string(data) != "keep" {
		t.Fatal("existing key file was modified")
	}
}

// The re-login prompts must send a new-cloud session back through wendy-auth,
// never through the legacy dashboard flow.
func TestReloginDispatchesOnStoredSession(t *testing.T) {
	var legacyCalls, oidcCalls int
	var gotOIDC oidcLoginOptions
	origLegacy, origOIDC := performLoginFn, performOIDCLoginFn
	t.Cleanup(func() { performLoginFn, performOIDCLoginFn = origLegacy, origOIDC })
	performLoginFn = func(_ context.Context, dashboard, grpc string) error {
		legacyCalls++
		if dashboard != defaultCloudDashboard || grpc != defaultCloudGRPC {
			t.Errorf("legacy login targets = %q %q", dashboard, grpc)
		}
		return nil
	}
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		oidcCalls++
		gotOIDC = opts
		return nil
	}

	t.Run("no session uses the legacy default", func(t *testing.T) {
		legacyCalls, oidcCalls = 0, 0
		if err := relogin(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if legacyCalls != 1 || oidcCalls != 0 {
			t.Fatalf("legacy=%d oidc=%d", legacyCalls, oidcCalls)
		}
	})

	t.Run("OIDC session signs in again at its realm", func(t *testing.T) {
		legacyCalls, oidcCalls = 0, 0
		auth := &config.AuthConfig{
			CloudDashboard: defaultDevCloudDashboard, CloudGRPC: defaultDevCloudGRPC,
			OAuthIssuer: "https://auth.dev.wendy.sh/realms/acme", OAuthClientID: "wendy-cli",
			OAuthResource: defaultDevCloudResource, DPoPPrivateKey: "pem",
		}
		if err := relogin(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		if legacyCalls != 0 || oidcCalls != 1 {
			t.Fatalf("legacy=%d oidc=%d", legacyCalls, oidcCalls)
		}
		want := oidcLoginOptions{
			Issuer: auth.OAuthIssuer, ClientID: "wendy-cli", CloudResource: defaultDevCloudResource,
			IdentityResource: defaultPKIIdentityResource, IdentityEndpoint: defaultDevPKIIdentityEndpoint,
			CloudURL: defaultDevCloudDashboard, CloudGRPC: defaultDevCloudGRPC,
		}
		if gotOIDC != want {
			t.Fatalf("OIDC options = %+v, want %+v", gotOIDC, want)
		}
	})

	t.Run("service-account session re-mints without a browser", func(t *testing.T) {
		legacyCalls, oidcCalls = 0, 0
		t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
		f := newFakeSAAuth(t, nil)
		keyFile, pub := newTestSAKeyFile(t, f.issuer)
		f.pub = pub
		var key serviceAccountKey
		if err := json.Unmarshal(keyFile, &key); err != nil {
			t.Fatal(err)
		}
		auth := &config.AuthConfig{
			CloudDashboard: defaultDevCloudDashboard, CloudGRPC: defaultDevCloudGRPC,
			OAuthIssuer: f.issuer, OAuthResource: defaultDevCloudResource,
			DPoPPrivateKey: key.PrivateKey, ServiceAccount: testSASubject,
		}
		cfg := &config.Config{}
		cfg.AddAuth(*auth)
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		captureStdout(t, func() {
			if err := relogin(context.Background(), auth); err != nil {
				t.Fatalf("relogin: %v", err)
			}
		})
		if legacyCalls != 0 || oidcCalls != 0 || f.mints.Load() != 1 {
			t.Fatalf("legacy=%d oidc=%d mints=%d", legacyCalls, oidcCalls, f.mints.Load())
		}
	})
}

func TestAuthLoginServiceAccountRejectsOtherModes(t *testing.T) {
	for _, extra := range [][]string{{"--legacy"}, {"--email", "a@b.c"}, {"--issuer", "https://x/realms/y"}, {"--api-key", "k"}} {
		cmd := newAuthLoginCmd()
		cmd.SetArgs(append([]string{"--service-account", "/nonexistent"}, extra...))
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--service-account") {
			t.Errorf("%v: err = %v, want a mode conflict", extra, err)
		}
	}
}

// Service-account sessions carry no certificate, so the organization key alone
// cannot tell two accounts in one realm apart: a second login or a re-mint must
// never overwrite another account's key and token.
func TestServiceAccountSessionsInOneRealmStayDistinct(t *testing.T) {
	base := config.AuthConfig{
		CloudDashboard: defaultDevCloudDashboard, CloudGRPC: defaultDevCloudGRPC,
		OAuthIssuer: "https://auth.dev.wendy.sh/realms/acme", DPoPPrivateKey: "pem",
	}
	ci, monitor := base, base
	ci.ServiceAccount, monitor.ServiceAccount = "sa-ci", "sa-monitor"

	cfg := &config.Config{}
	cfg.AddAuth(ci)
	cfg.AddAuth(monitor)
	if len(cfg.Auth) != 2 {
		t.Fatalf("stored %d sessions, want 2", len(cfg.Auth))
	}
	if sameOAuthSession(&cfg.Auth[0], &monitor) {
		t.Fatal("a re-mint for one service account would persist over another")
	}
	ci.APIKey = "new"
	cfg.AddAuth(ci)
	if len(cfg.Auth) != 2 || cfg.Auth[0].APIKey != "new" || cfg.Auth[1].ServiceAccount != "sa-monitor" {
		t.Fatalf("re-login did not replace its own session: %+v", cfg.Auth)
	}
}
