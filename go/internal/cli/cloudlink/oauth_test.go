package cloudlink

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const resource = "https://gateway.example/mcp"
const callbackURI = "https://chatgpt.example/oauth/callback"
const verifier = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"

type fakeSession struct {
	store browserauth.CredentialStore
	email string
}

func (s *fakeSession) Begin(_ context.Context, email, redirect string) (string, error) {
	s.email = email
	return "https://auth.example/realms/system/authorize?" + url.Values{"state": {"upstream-state"}, "redirect_uri": {redirect}}.Encode(), nil
}
func (s *fakeSession) Complete(_ context.Context, code, state, issuer string) (browserauth.Profile, error) {
	if code != "upstream-code" || state != "upstream-state" || issuer != "https://auth.example/realms/system" {
		return browserauth.Profile{}, errors.New("invalid upstream callback")
	}
	return browserauth.Profile{Subject: s.email}, s.store.Save([]byte("secret-cloud-refresh-token:" + s.email))
}
func (s *fakeSession) Restore(context.Context) (*browserauth.Profile, error) {
	raw, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(raw), "secret-cloud-refresh-token:") {
		return nil, errors.New("invalid saved session")
	}
	s.email = strings.TrimPrefix(string(raw), "secret-cloud-refresh-token:")
	return &browserauth.Profile{Subject: s.email}, nil
}
func (s *fakeSession) DiscoverFiltered(_ context.Context, _ func(context.Context, string) (net.Conn, error), online bool) ([]*cloudpbv2.Asset, error) {
	if err := s.store.Save([]byte("secret-cloud-refresh-token:" + s.email)); err != nil {
		return nil, err
	}
	return []*cloudpbv2.Asset{{Id: s.email, Name: s.email}}, nil
}
func (s *fakeSession) ConnectDevice(_ context.Context, asset string, _ func(context.Context, string) (net.Conn, error), _ *http.Client) (*grpcclient.AgentConnection, error) {
	if asset != s.email {
		return nil, errors.New("device belongs to another account")
	}
	return &grpcclient.AgentConnection{}, nil
}

func fixtureConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{Services: browserauth.Settings{AuthBase: "https://auth.example", ClientID: "hosted-cloud-login", IdentityResource: "https://pki.example/identity", IdentityEndpoint: "https://identity.example/v1/identity/certificate", CloudResource: "https://cloud.example/api", CloudGRPC: "api.example:443", RelayIssuer: "https://api.example"}, StateFile: filepath.Join(dir, "accounts.enc"), EncryptionKeyEnv: "KEY", Clients: []Client{{ID: "chatgpt", Name: "ChatGPT", RedirectURIs: []string{callbackURI}}, {ID: "other", Name: "Other client", RedirectURIs: []string{"https://other.example/callback"}}}, Scopes: []string{"robots:read", "cameras:capture"}}
}
func newFixture(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := New(cfg, resource, func(name string) string {
		if name == "KEY" {
			return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
		}
		return strings.Repeat("s", 32)
	})
	if err != nil {
		t.Fatal(err)
	}
	m.newSession = func(store browserauth.CredentialStore) cloudSession { return &fakeSession{store: store} }
	t.Cleanup(m.Close)
	return m
}
func request(m *Manager, method, path string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://gateway.example"+path, strings.NewReader(form.Encode()))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	if !m.ServeOAuth(w, r) {
		w.WriteHeader(404)
	}
	return w
}
func authorizationQuery() url.Values {
	challenge := sha256.Sum256([]byte(verifier))
	return url.Values{"client_id": {"chatgpt"}, "redirect_uri": {callbackURI}, "response_type": {"code"}, "resource": {resource}, "scope": {"robots:read cameras:capture"}, "state": {"downstream-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
}
func begin(t *testing.T, m *Manager) (*http.Cookie, string) {
	t.Helper()
	w := request(m, "GET", "/oauth/authorize?"+authorizationQuery().Encode(), nil, nil, "")
	if w.Code != 200 {
		t.Fatalf("authorize %d: %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatal("unsafe login cookie")
	}
	m.mu.Lock()
	csrf := m.pending[cookie.Value].csrf
	m.mu.Unlock()
	if !strings.Contains(w.Body.String(), csrf) || !strings.Contains(w.Body.String(), "cameras:capture") {
		t.Fatal("consent form lacks CSRF or permissions")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "https://auth.example") || !strings.Contains(csp, "https://chatgpt.example") || strings.Contains(csp, "*") {
		t.Fatal("consent redirects cannot pass the configured browser CSP", csp)
	}
	return cookie, csrf
}
func linkCode(t *testing.T, m *Manager, email string) string {
	t.Helper()
	cookie, csrf := begin(t, m)
	w := request(m, "POST", "/oauth/authorize", url.Values{"transaction": {cookie.Value}, "csrf": {csrf}, "email": {email}, "approve": {"yes"}}, cookie, m.origin)
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "https://auth.example/realms/system/authorize?") {
		t.Fatalf("approve %d: %s", w.Code, w.Body.String())
	}
	w = request(m, "GET", "/auth/callback?"+url.Values{"code": {"upstream-code"}, "state": {"upstream-state"}, "iss": {"https://auth.example/realms/system"}}.Encode(), nil, cookie, "")
	if w.Code != 303 {
		t.Fatalf("callback %d: %s", w.Code, w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	if u.Query().Get("state") != "downstream-state" || u.Query().Get("iss") != m.origin || u.Query().Get("code") == "" {
		t.Fatalf("invalid callback: %s", u)
	}
	return u.Query().Get("code")
}
func exchange(t *testing.T, m *Manager, code string) map[string]any {
	t.Helper()
	w := request(m, "POST", "/oauth/token", url.Values{"client_id": {"chatgpt"}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callbackURI}, "code_verifier": {verifier}, "resource": {resource}}, nil, "")
	if w.Code != 200 {
		t.Fatalf("exchange %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["token_type"] != "Bearer" || result["expires_in"].(float64) > 3600 || strings.Contains(w.Body.String(), "secret-cloud") {
		t.Fatal("unsafe token response")
	}
	return result
}

func TestLinkIsolationEncryptedRestartAndRevocation(t *testing.T) {
	cfg := fixtureConfig(t)
	m := newFixture(t, cfg)
	a := exchange(t, m, linkCode(t, m, "alice@example.com"))
	b := exchange(t, m, linkCode(t, m, "bob@example.com"))
	pa, err := m.Authenticate(context.Background(), a["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := m.Authenticate(context.Background(), b["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if pa.Subject == pb.Subject || pa.Subject == "alice@example.com" {
		t.Fatal("connections are not independently bound")
	}
	for _, test := range []struct {
		principal Principal
		email     string
	}{{pa, "alice@example.com"}, {pb, "bob@example.com"}} {
		assets, err := m.Discover(context.Background(), test.principal.Subject, true)
		if err != nil || len(assets) != 1 || assets[0].Name != test.email {
			t.Fatalf("account inventory leaked: %v %v", assets, err)
		}
	}
	if _, err := m.Connect(context.Background(), pa.Subject, "bob@example.com"); err == nil {
		t.Fatal("Alice connected using Bob's identity")
	}
	raw, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"secret-cloud", "alice@example.com", a["access_token"].(string), a["refresh_token"].(string)} {
		if strings.Contains(string(raw), plain) {
			t.Fatal("plaintext credentials in account database")
		}
	}
	info, _ := os.Stat(cfg.StateFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("account database is public")
	}
	if _, err := New(cfg, resource, func(string) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) }); err == nil {
		t.Fatal("a second writer opened the database")
	}
	m.Close()
	if _, err := m.Authenticate(context.Background(), a["access_token"].(string)); err == nil {
		t.Fatal("closed manager still authorizes requests")
	}
	if err := (accountStore{m, pa.Subject}).Save([]byte("late upstream token rotation")); err == nil {
		t.Fatal("closed manager can overwrite state after releasing its lock")
	}
	restored := newFixture(t, cfg)
	if _, err := restored.Authenticate(context.Background(), a["access_token"].(string)); err != nil {
		t.Fatal("access token did not survive restart", err)
	}
	assets, err := restored.Discover(context.Background(), pa.Subject, false)
	if err != nil || assets[0].Name != "alice@example.com" {
		t.Fatal("upstream credentials did not survive restart", err)
	}
	w := request(restored, "POST", "/oauth/revoke", url.Values{"client_id": {"other"}, "token": {a["refresh_token"].(string)}}, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, err := restored.Authenticate(context.Background(), a["access_token"].(string)); err != nil {
		t.Fatal("another client revoked Alice's connection")
	}
	w = request(restored, "POST", "/oauth/revoke", url.Values{"client_id": {"chatgpt"}, "token": {a["refresh_token"].(string)}}, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, err := restored.Authenticate(context.Background(), a["access_token"].(string)); err == nil {
		t.Fatal("revoked access token accepted")
	}
	if _, err := restored.Discover(context.Background(), pa.Subject, true); err == nil {
		t.Fatal("revoked account still has Cloud credentials")
	}
	if _, err := restored.Authenticate(context.Background(), b["access_token"].(string)); err != nil {
		t.Fatal("revocation affected another account", err)
	}
}

func TestRefreshRotationReplayAndScopeNarrowing(t *testing.T) {
	m := newFixture(t, fixtureConfig(t))
	a := exchange(t, m, linkCode(t, m, "alice@example.com"))
	form := url.Values{"client_id": {"chatgpt"}, "grant_type": {"refresh_token"}, "refresh_token": {a["refresh_token"].(string)}, "resource": {resource}, "scope": {"host:manage"}}
	w := request(m, "POST", "/oauth/token", form, nil, "")
	if w.Code != 400 {
		t.Fatal("scope escalation accepted")
	}
	form.Set("scope", "robots:read")
	w = request(m, "POST", "/oauth/token", form, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var rotated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	p, err := m.Authenticate(context.Background(), rotated["access_token"].(string))
	if err != nil || len(p.Scopes) != 1 || p.Scopes[0] != "robots:read" {
		t.Fatal("refresh widened scopes", p, err)
	}
	if _, err := m.Authenticate(context.Background(), a["access_token"].(string)); err != nil {
		t.Fatal(err)
	}
	w = request(m, "POST", "/oauth/token", form, nil, "")
	if w.Code != 400 {
		t.Fatal("used refresh token accepted")
	}
	if _, err := m.Authenticate(context.Background(), rotated["access_token"].(string)); err == nil {
		t.Fatal("refresh replay did not revoke the token family")
	}
}

func TestAuthorizationValidationAndReplay(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{{"redirect", "redirect_uri", "https://evil.example/callback"}, {"resource", "resource", "https://evil.example/mcp"}, {"plain PKCE", "code_challenge_method", "plain"}, {"scope", "scope", "host:manage"}, {"missing state", "state", ""}} {
		t.Run(test.name, func(t *testing.T) {
			m := newFixture(t, fixtureConfig(t))
			q := authorizationQuery()
			q.Set(test.key, test.value)
			w := request(m, "GET", "/oauth/authorize?"+q.Encode(), nil, nil, "")
			if w.Code != 400 || w.Header().Get("Location") != "" {
				t.Fatal("unsafe authorization accepted", w.Code)
			}
		})
	}
	m := newFixture(t, fixtureConfig(t))
	cookie, csrf := begin(t, m)
	form := url.Values{"transaction": {cookie.Value}, "csrf": {csrf}, "email": {"alice@example.com"}, "approve": {"yes"}}
	for _, test := range []struct {
		cookie *http.Cookie
		origin string
		csrf   string
	}{{nil, m.origin, csrf}, {cookie, "https://evil.example", csrf}, {cookie, m.origin, "wrong"}} {
		form.Set("csrf", test.csrf)
		w := request(m, "POST", "/oauth/authorize", form, test.cookie, test.origin)
		if w.Code < 400 {
			t.Fatal("consent CSRF accepted")
		}
	}
	form.Set("csrf", csrf)
	w := request(m, "POST", "/oauth/authorize", form, cookie, m.origin)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	callback := url.Values{"code": {"upstream-code"}, "state": {"upstream-state"}, "iss": {"https://auth.example/realms/system"}}
	for _, key := range []string{"state", "iss"} {
		wrong := url.Values{}
		for name, values := range callback {
			wrong[name] = append([]string(nil), values...)
		}
		wrong.Set(key, "wrong")
		if w := request(m, "GET", "/auth/callback?"+wrong.Encode(), nil, cookie, ""); w.Code != 400 {
			t.Fatal("callback mix-up accepted")
		}
	}
	w = request(m, "GET", "/auth/callback?"+callback.Encode(), nil, cookie, "")
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(m, "GET", "/auth/callback?"+callback.Encode(), nil, cookie, ""); w.Code != 400 {
		t.Fatal("callback replay accepted")
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	code := u.Query().Get("code")
	exchange(t, m, code)
	if w := request(m, "POST", "/oauth/token", url.Values{"client_id": {"chatgpt"}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callbackURI}, "code_verifier": {verifier}}, nil, ""); w.Code != 400 {
		t.Fatal("authorization code replay accepted")
	}
}

func TestStateIntegrityAndServiceBinding(t *testing.T) {
	cfg := fixtureConfig(t)
	m := newFixture(t, cfg)
	exchange(t, m, linkCode(t, m, "alice@example.com"))
	m.Close()
	wrongKey := func(string) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))) }
	if opened, err := New(cfg, resource, wrongKey); err == nil {
		opened.Close()
		t.Fatal("wrong encryption key accepted")
	}
	cfg.Services.CloudGRPC = "different.example:443"
	if opened, err := New(cfg, resource, func(string) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) }); err == nil {
		opened.Close()
		t.Fatal("saved credentials accepted for another service")
	}
	data, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(cfg.StateFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Services.CloudGRPC = "api.example:443"
	if opened, err := New(cfg, resource, func(string) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) }); err == nil {
		opened.Close()
		t.Fatal("tampered credentials accepted")
	}
}

func TestCodeExchangeRejectsChangedClientCallbackResourceAndPKCE(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"another client", "client_id", "other"},
		{"another callback", "redirect_uri", "https://evil.example/callback"},
		{"another resource", "resource", "https://evil.example/mcp"},
		{"wrong verifier", "code_verifier", strings.Repeat("x", 43)},
		{"short verifier", "code_verifier", "x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newFixture(t, fixtureConfig(t))
			code := linkCode(t, m, "alice@example.com")
			form := url.Values{"client_id": {"chatgpt"}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callbackURI}, "code_verifier": {verifier}, "resource": {resource}}
			form.Set(test.key, test.value)
			if w := request(m, "POST", "/oauth/token", form, nil, ""); w.Code != 400 || strings.Contains(w.Body.String(), "access_token") {
				t.Fatal("unsafe code exchange accepted", w.Code, w.Body.String())
			}
		})
	}
}

func TestConfidentialClientRequiresRegisteredSecret(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Clients[0].SecretEnv = "CLIENT_SECRET"
	m := newFixture(t, cfg)
	code := linkCode(t, m, "alice@example.com")
	form := url.Values{"client_id": {"chatgpt"}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callbackURI}, "code_verifier": {verifier}, "resource": {resource}}
	for _, secret := range []string{"", "wrong"} {
		form.Set("client_secret", secret)
		if w := request(m, "POST", "/oauth/token", form, nil, ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("confidential client accepted without its secret")
		}
	}
	form.Set("client_secret", strings.Repeat("s", 32))
	if w := request(m, "POST", "/oauth/token", form, nil, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestParallelAccountsAndExpiry(t *testing.T) {
	m := newFixture(t, fixtureConfig(t))
	a := exchange(t, m, linkCode(t, m, "alice@example.com"))
	b := exchange(t, m, linkCode(t, m, "bob@example.com"))
	var wg sync.WaitGroup
	for _, token := range []map[string]any{a, b} {
		p, err := m.Authenticate(context.Background(), token["access_token"].(string))
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.Discover(context.Background(), p.Subject, true); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	m.mu.Lock()
	access := m.data.Access[tokenHash(a["access_token"].(string))]
	access.Expires = time.Now().Add(-time.Second).Unix()
	m.data.Access[tokenHash(a["access_token"].(string))] = access
	m.mu.Unlock()
	if _, err := m.Authenticate(context.Background(), a["access_token"].(string)); err == nil {
		t.Fatal("expired token accepted")
	}
}
