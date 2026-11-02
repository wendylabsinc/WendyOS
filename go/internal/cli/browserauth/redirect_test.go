package browserauth

import (
	"context"
	"crypto/mldsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryPending struct {
	mu      sync.Mutex
	records map[string][]byte
	failure error
}

func (m *memoryPending) Put(h string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return m.failure
	}
	if m.records == nil {
		m.records = map[string][]byte{}
	}
	m.records[h] = append([]byte(nil), b...)
	return nil
}
func (m *memoryPending) Take(h string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return nil, m.failure
	}
	b := m.records[h]
	delete(m.records, h)
	return b, nil
}
func (m *memoryPending) Delete(h string) error { b, e := m.Take(h); clear(b); return e }

func redirectSetup(t *testing.T) (*Session, *memoryPending, RedirectCallback, *int) {
	t.Helper()
	pending := &memoryPending{}
	var start RedirectStart
	s, calls := setupLogin(t, nil, func(s *Session) (string, error) {
		s.Pending = pending
		var err error
		start, err = s.BeginRedirect(context.Background(), "", "", "http://localhost:5173/auth/callback")
		return start.URL, err
	})
	if s.key != nil || s.privatePEM != "" || s.verifier != "" || s.state != "" {
		t.Fatal("begin created live key or transaction")
	}
	u, _ := url.Parse(start.URL)
	if u.Query().Has("dpop_jkt") {
		t.Fatal("redirect pre-bound an absent key")
	}
	return s, pending, RedirectCallback{Pending: start.Pending, Redirect: "http://localhost:5173/auth/callback", Code: "valid-code", State: u.Query().Get("state"), Issuer: testIssuer}, calls
}

func TestRedirectFreshSessionSameKeyAndReplay(t *testing.T) {
	old, store, callback, calls := redirectSetup(t)
	// Only persistent bytes and transport survive; no fields from the old session.
	fresh := &Session{Client: old.Client, Pending: store, Store: &memoryStore{}}
	old = nil
	profile, err := fresh.CompleteRedirect(context.Background(), callback)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Subject != "operator-test" || *calls != 3 || fresh.key == nil {
		t.Fatal("incomplete resumed login")
	}
	// setupLogin checks nonce retries, both token audiences, CSR and PKI proof
	// against one public key. A real request signature uses that same key too.
	message := []byte("Cloud request")
	signature, err := fresh.key.Sign(nil, message, &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mldsa.Verify(fresh.key.Public().(*mldsa.PublicKey), message, signature, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.CompleteRedirect(context.Background(), callback); err == nil {
		t.Fatal("accepted replay")
	}
	if *calls != 3 {
		t.Fatal("replay reached token endpoint")
	}
	if len(store.records) != 0 {
		t.Fatal("transaction survived completion")
	}
}

func TestRedirectRejectsAndBurnsBeforeNetwork(t *testing.T) {
	scenarios := []string{"state", "empty-state", "issuer", "empty-issuer", "system", "pinned-issuer", "redirect", "empty-redirect", "settings", "policy", "expired", "future", "zero-time", "missing-code", "error", "error-with-code", "error-bad-state", "version", "verifier", "stored-state", "stored-redirect", "malformed", "unknown-field", "trailing", "oversized", "missing-record", "foreign-handle"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			old, store, c, _ := redirectSetup(t)
			var record redirectTransaction
			_ = json.Unmarshal(store.records[c.Pending], &record)
			c.Code = ""
			if scenario != "missing-code" {
				c.Code = "valid-code"
			}
			fresh := &Session{Pending: store, Settings: nil, Client: doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid callback made network request")
				return nil, nil
			})}
			switch scenario {
			case "state":
				c.State = random()
			case "empty-state":
				c.State = ""
			case "issuer":
				c.Issuer = "https://foreign.test/realms/acme"
			case "empty-issuer":
				c.Issuer = ""
			case "system":
				c.Issuer = AuthBase + "/realms/system"
			case "pinned-issuer":
				record.Issuer = AuthBase + "/realms/other"
			case "redirect":
				c.Redirect = "https://other.test/auth/callback"
			case "empty-redirect":
				c.Redirect = ""
			case "settings":
				settings := old.settings()
				settings.CloudResource = "https://cloud.other.test/api"
				fresh.Settings = &settings
			case "policy":
				fresh.RequireHostedMCPPolicy = true
			case "expired":
				record.Started = time.Now().Add(-RedirectLifetime)
			case "future":
				record.Started = time.Now().Add(time.Minute)
			case "zero-time":
				record.Started = time.Time{}
			case "error":
				c.Error = "access_denied"
				c.Code = ""
			case "error-with-code":
				c.Error = "access_denied"
			case "error-bad-state":
				c.Error = "access_denied"
				c.State = "wrong"
			case "version":
				record.Version = 2
			case "verifier":
				record.Verifier = "short"
			case "stored-state":
				record.State = "short"
			case "stored-redirect":
				record.Redirect = "https://other.test/auth/callback?"
			}
			raw, _ := json.Marshal(record)
			switch scenario {
			case "malformed":
				raw = []byte("{")
			case "unknown-field":
				raw = append(raw[:len(raw)-1], []byte(",\"key\":\"not allowed\"}")...)
			case "trailing":
				raw = append(raw, []byte(" {}")...)
			case "oversized":
				raw = append(raw, []byte(strings.Repeat(" ", MaxRedirectRecord))...)
			}
			store.records[c.Pending] = raw
			original := c.Pending
			if scenario == "missing-record" {
				delete(store.records, c.Pending)
			}
			if scenario == "foreign-handle" {
				c.Pending = random()
			}
			_, err := fresh.CompleteRedirect(context.Background(), c)
			if err == nil {
				t.Fatal("accepted invalid callback")
			}
			if fresh.key != nil {
				t.Fatal("generated key before validation")
			}
			if scenario == "foreign-handle" {
				if len(store.records) != 1 {
					t.Fatal("foreign handle removed another attempt")
				}
				return
			}
			if len(store.records) != 0 {
				t.Fatal("failed callback was not burned")
			}
			c.Pending = original
			if _, err := fresh.CompleteRedirect(context.Background(), c); err == nil {
				t.Fatal("replayed failure")
			}
		})
	}
}

func TestRedirectAtomicTakeAcrossSessions(t *testing.T) {
	old, store, c, calls := redirectSetup(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			s := &Session{Client: old.Client, Pending: store}
			if _, err := s.CompleteRedirect(context.Background(), c); err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 || *calls != 3 {
		t.Fatalf("successes=%d calls=%d", successes.Load(), *calls)
	}
}

func TestRedirectStorageFailureAndCancelIsolation(t *testing.T) {
	s, store, c, _ := redirectSetup(t)
	original := append([]byte(nil), store.records[c.Pending]...)
	second, err := s.BeginRedirect(context.Background(), "", "", c.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRedirect(second.Pending); err != nil {
		t.Fatal(err)
	}
	if string(store.records[c.Pending]) != string(original) || len(store.records) != 1 {
		t.Fatal("cancel affected another attempt")
	}
	store.failure = errors.New("storage blocked")
	start, err := s.BeginRedirect(context.Background(), "", "", c.Redirect)
	if err == nil || start.URL != "" || start.Pending != "" {
		t.Fatal("navigation allowed with blocked storage")
	}
	fresh := &Session{Pending: store, Client: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("storage failure made network request")
		return nil, nil
	})}
	if _, err := fresh.CompleteRedirect(context.Background(), c); err == nil {
		t.Fatal("ignored storage failure")
	}
	if err := s.CancelRedirect(c.Pending); err == nil {
		t.Fatal("ignored failed cancel")
	}
	if len(store.records) != 1 {
		t.Fatal("failed storage changed records")
	}
}

// The callback reports location.origin + location.pathname, so a redirect the
// browser would rewrite fails at begin rather than after the IdP round trip.
func TestRedirectBeginRequiresBrowserCanonicalForm(t *testing.T) {
	s, store, _, _ := redirectSetup(t)
	for _, redirect := range []string{
		"https://Console.Example/auth/callback",
		"https://console.example:443/auth/callback",
		"HTTPS://console.example/auth/callback",
		"https://console.example:/auth/callback",
		"https://console.example:08443/auth/callback",
		"https://console.example:65536/auth/callback",
		"https://%63onsole.example/auth/callback",
		"https://[::1]/auth/callback",
		"https://bücher.example/auth/callback",
	} {
		if start, err := s.BeginRedirect(context.Background(), "", "", redirect); err == nil || start.Pending != "" {
			t.Errorf("accepted %s", redirect)
		}
	}
	if len(store.records) != 1 {
		t.Fatal("refused redirect stored a transaction")
	}
	for _, redirect := range []string{"https://console.example/auth/callback", "https://console.example:8443/auth/callback", "https://my-console_1.example./auth/callback"} {
		if !validRedirect(redirect) {
			t.Errorf("refused %s", redirect)
		}
	}
}

func TestRedirectPreservesLegacyAttempt(t *testing.T) {
	s, calls := setup(t, nil)
	key, state, verifier := s.key, s.state, s.verifier
	s.Pending = &memoryPending{}
	start, err := s.BeginRedirect(context.Background(), "", "", "http://localhost:5173/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRedirect(start.Pending); err != nil {
		t.Fatal(err)
	}
	if s.key != key || s.state != state || s.verifier != verifier {
		t.Fatal("redirect displaced legacy attempt")
	}
	if _, err := s.Complete(context.Background(), "valid-code", state, testIssuer); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatal("legacy login failed")
	}
}

func TestRedirectOAuthErrorRetainsCauseWithoutIssuer(t *testing.T) {
	for _, code := range []string{"access_denied", "invalid_scope"} {
		t.Run(code, func(t *testing.T) {
			_, store, callback, _ := redirectSetup(t)
			callback.Error, callback.Code, callback.Issuer = code, "", ""
			fresh := &Session{Pending: store, Client: doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("OAuth error made network request")
				return nil, nil
			})}
			_, err := fresh.CompleteRedirect(context.Background(), callback)
			if err == nil || err.Error() != "Authentication failed: "+code {
				t.Fatalf("lost OAuth cause: %v", err)
			}
			if fresh.key != nil || len(store.records) != 0 {
				t.Fatal("error created key or kept transaction")
			}
		})
	}
}

func TestRedirectOAuthErrorValidatesBindingsAndBounds(t *testing.T) {
	for _, scenario := range []string{"state", "issuer", "control", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, callback, _ := redirectSetup(t)
			callback.Error, callback.Code, callback.Issuer = "access_denied", "", ""
			want := ""
			switch scenario {
			case "state":
				callback.State = "wrong"
				want = "state mismatch"
			case "issuer":
				callback.Issuer = "https://foreign.test/realms/acme"
				want = "issuer changed"
			case "control":
				callback.Error = "access_denied\nsecret"
				want = "Invalid OAuth error code"
			case "oversize":
				callback.Error = strings.Repeat("x", 129)
				want = "Invalid OAuth error code"
			}
			fresh := &Session{Pending: store, Client: doerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid error made network request")
				return nil, nil
			})}
			_, err := fresh.CompleteRedirect(context.Background(), callback)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong error: %v", err)
			}
			if fresh.key != nil || len(store.records) != 0 {
				t.Fatal("error generated key or retained pending")
			}
		})
	}
}
