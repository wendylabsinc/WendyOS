package cloudlink

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const cookieName = "__Host-wendy-link"

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect Wendy Cloud</title></head><body><main><h1>Connect Wendy Cloud</h1><p>Connect your Wendy Cloud account to {{.Client}}. This connection can access devices authorized by your Cloud account with these permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><p>The gateway stores an encrypted Cloud session until you revoke the connection or it expires after 30 days.</p><form method="post" action="/oauth/authorize"><input type="hidden" name="transaction" value="{{.Transaction}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>Wendy account email <input type="email" name="email" required maxlength="254" autocomplete="email"></label><button name="approve" value="yes" type="submit">Continue to Wendy sign-in</button><button name="approve" value="no" type="submit">Cancel</button></form></main></body></html>`))

// ServeOAuth serves only known authorization routes. The gateway remains the
// resource server and owns /mcp, health checks, and protected-resource metadata.
func (m *Manager) ServeOAuth(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server", "/oauth/authorize", "/auth/callback", "/oauth/token", "/oauth/revoke":
	default:
		return false
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		oauthError(w, 503, "temporarily_unavailable")
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Browsers can apply form-action to redirects after the consent POST. Permit
	// only the configured auth origin and registered client callback origins.
	formOrigins := []string{"'self'", m.config.Services.AuthBase}
	for _, client := range m.config.Clients {
		for _, callback := range client.RedirectURIs {
			u, _ := url.Parse(callback)
			origin := u.Scheme + "://" + u.Host
			if !slices.Contains(formOrigins, origin) {
				formOrigins = append(formOrigins, origin)
			}
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action "+strings.Join(formOrigins, " ")+"; frame-ancestors 'none'; base-uri 'none'")
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		if !method(w, r, "GET") {
			return true
		}
		methods := []string{}
		for _, client := range m.config.Clients {
			if client.SecretEnv == "" {
				if !slices.Contains(methods, "none") {
					methods = append(methods, "none")
				}
			} else if !slices.Contains(methods, "client_secret_basic") {
				methods = append(methods, "client_secret_basic", "client_secret_post")
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"issuer": m.origin, "authorization_endpoint": m.origin + "/oauth/authorize", "token_endpoint": m.origin + "/oauth/token", "revocation_endpoint": m.origin + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": methods, "scopes_supported": m.config.Scopes})
	case "/oauth/authorize":
		if r.Method == http.MethodGet {
			m.authorize(w, r)
		} else if method(w, r, "POST") {
			m.approve(w, r)
		}
	case "/auth/callback":
		if method(w, r, "GET") {
			m.callback(w, r)
		}
	case "/oauth/token":
		if method(w, r, "POST") {
			m.token(w, r)
		}
	case "/oauth/revoke":
		if method(w, r, "POST") {
			m.revoke(w, r)
		}
	}
	return true
}

func method(w http.ResponseWriter, r *http.Request, expected string) bool {
	if r.Method == expected {
		return true
	}
	w.Header().Set("Allow", expected)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func oauthError(w http.ResponseWriter, status int, code string) {
	if code == "invalid_client" && status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="Wendy Cloud link"`)
	}
	writeJSON(w, status, map[string]string{"error": code})
}
func oneValue(values url.Values) bool {
	for _, values := range values {
		if len(values) != 1 {
			return false
		}
	}
	return true
}
func postForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/x-www-form-urlencoded" || r.ParseForm() != nil || !oneValue(r.PostForm) || len(r.URL.RawQuery) != 0 {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}
func same(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (m *Manager) prunePendingLocked() {
	now := time.Now()
	for id, p := range m.pending {
		if !p.expires.After(now) {
			delete(m.pending, id)
		}
	}
	for code, value := range m.codes {
		if !value.expires.After(now) {
			delete(m.codes, code)
		}
	}
}

func (m *Manager) authorize(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !oneValue(q) || len(r.URL.RawQuery) > 8192 {
		oauthError(w, 400, "invalid_request")
		return
	}
	client, ok := m.clients[q.Get("client_id")]
	redirect := q.Get("redirect_uri")
	// Never redirect errors until the exact registered callback is validated.
	if !ok || !slices.Contains(client.RedirectURIs, redirect) {
		oauthError(w, 400, "invalid_request")
		return
	}
	scopes := strings.Fields(q.Get("scope"))
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if q.Get("response_type") != "code" || q.Get("resource") != m.resource || q.Get("code_challenge_method") != "S256" || err != nil || len(challenge) != 32 || q.Get("state") == "" || len(q.Get("state")) > 512 || len(scopes) == 0 || len(intersection(scopes, m.config.Scopes)) != len(scopes) {
		oauthError(w, 400, "invalid_request")
		return
	}
	ticket, err := secret()
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	csrf, err := secret()
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	m.mu.Lock()
	m.prunePendingLocked()
	if len(m.pending) >= 128 {
		m.mu.Unlock()
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	m.pending[ticket] = &pending{client: client.ID, redirect: redirect, state: q.Get("state"), challenge: q.Get("code_challenge"), csrf: csrf, scopes: scopes, expires: time.Now().Add(10 * time.Minute)}
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: ticket, Path: "/", MaxAge: 600, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, struct {
		Client, Transaction, CSRF string
		Scopes                    []string
	}{client.Name, ticket, csrf, scopes})
}

func (m *Manager) approve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != m.origin {
		oauthError(w, 403, "invalid_request")
		return
	}
	if !postForm(w, r) {
		return
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || !same(cookie.Value, r.PostForm.Get("transaction")) {
		oauthError(w, 400, "invalid_request")
		return
	}
	m.mu.Lock()
	p := m.pending[cookie.Value]
	if p == nil || !p.expires.After(time.Now()) || p.upstream != nil || !same(p.csrf, r.PostForm.Get("csrf")) {
		m.mu.Unlock()
		oauthError(w, 400, "invalid_request")
		return
	}
	// Consume consent before network access; replay cannot replace a sign-in.
	delete(m.pending, cookie.Value)
	m.mu.Unlock()
	if r.PostForm.Get("approve") != "yes" {
		redirectError(w, r, p, "access_denied")
		return
	}
	email := r.PostForm.Get("email")
	if email == "" || len(email) > 254 {
		oauthError(w, 400, "invalid_request")
		return
	}
	p.store = &memoryStore{}
	p.upstream = m.newSession(p.store)
	target, err := p.upstream.Begin(r.Context(), email, m.origin+"/auth/callback")
	if err != nil {
		oauthError(w, 502, "server_error")
		return
	}
	u, err := url.Parse(target)
	if err != nil || u.Query().Get("state") == "" {
		oauthError(w, 502, "server_error")
		return
	}
	p.upstreamState = u.Query().Get("state")
	// Session.Begin pins this URL to the configured auth realm. Keep the expected
	// issuer separately, so a callback cannot substitute another trusted realm.
	p.issuer = strings.TrimSuffix(strings.Split(target, "?")[0], "/authorize")
	m.mu.Lock()
	m.pending[cookie.Value] = p
	m.mu.Unlock()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func redirectError(w http.ResponseWriter, r *http.Request, p *pending, code string) {
	u, _ := url.Parse(p.redirect)
	q := u.Query()
	q.Set("error", code)
	q.Set("state", p.state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (m *Manager) callback(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	cookie, cookieErr := r.Cookie(cookieName)
	if err != nil || !oneValue(q) || len(r.URL.RawQuery) > 8192 || cookieErr != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	m.mu.Lock()
	p := m.pending[cookie.Value]
	if p == nil || !p.expires.After(time.Now()) || p.upstream == nil || !same(p.upstreamState, q.Get("state")) || p.issuer != q.Get("iss") {
		m.mu.Unlock()
		oauthError(w, 400, "invalid_request")
		return
	}
	delete(m.pending, cookie.Value)
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if q.Get("error") != "" {
		redirectError(w, r, p, "access_denied")
		return
	}
	_, err = p.upstream.Complete(r.Context(), q.Get("code"), q.Get("state"), q.Get("iss"))
	if err != nil || len(p.store.raw) == 0 {
		redirectError(w, r, p, "server_error")
		return
	}
	id, err := secret()
	if err != nil {
		redirectError(w, r, p, "server_error")
		return
	}
	code, err := secret()
	if err != nil {
		redirectError(w, r, p, "server_error")
		return
	}
	m.mu.Lock()
	next := clone(m.data)
	next.Accounts[id] = account{Client: p.client, Scopes: p.scopes, Expires: time.Now().Add(5 * time.Minute).Unix(), Upstream: p.store.raw}
	err = m.commitLocked(next)
	if err == nil {
		m.codes[tokenHash(code)] = authorizationCode{id, p.client, p.redirect, p.challenge, time.Now().Add(5 * time.Minute)}
	}
	m.mu.Unlock()
	if err != nil {
		redirectError(w, r, p, "server_error")
		return
	}
	u, _ := url.Parse(p.redirect)
	values := u.Query()
	values.Set("code", code)
	values.Set("state", p.state)
	values.Set("iss", m.origin)
	u.RawQuery = values.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (m *Manager) identifyClient(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, supplied := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if username, password, basic := r.BasicAuth(); basic {
		// RFC 6749 Basic credentials are form-encoded before base64 encoding.
		var userErr, passwordErr error
		username, userErr = url.QueryUnescape(username)
		password, passwordErr = url.QueryUnescape(password)
		if userErr != nil || passwordErr != nil {
			oauthError(w, 401, "invalid_client")
			return "", false
		}
		if supplied != "" || (id != "" && id != username) {
			oauthError(w, 401, "invalid_client")
			return "", false
		}
		id, supplied = username, password
	} else if r.Header.Get("Authorization") != "" {
		oauthError(w, 401, "invalid_client")
		return "", false
	}
	_, ok := m.clients[id]
	expected := m.clientSecrets[id]
	if !ok || (expected != "" && !same(expected, supplied)) || (expected == "" && supplied != "") {
		oauthError(w, 401, "invalid_client")
		return "", false
	}
	return id, true
}

func (m *Manager) token(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != m.origin {
		oauthError(w, 403, "invalid_request")
		return
	}
	if !postForm(w, r) {
		return
	}
	client, ok := m.identifyClient(w, r)
	if !ok {
		return
	}
	if resource := r.PostForm.Get("resource"); resource != "" && resource != m.resource {
		oauthError(w, 400, "invalid_target")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var id string
	var consumedRefresh string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		hash := tokenHash(r.PostForm.Get("code"))
		code, found := m.codes[hash]
		if !found || !code.expires.After(time.Now()) || code.client != client || code.redirect != r.PostForm.Get("redirect_uri") {
			oauthError(w, 400, "invalid_grant")
			return
		}
		delete(m.codes, hash)
		verifier := r.PostForm.Get("code_verifier")
		if len(verifier) < 43 || len(verifier) > 128 || strings.IndexFunc(verifier, func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c))
		}) >= 0 {
			oauthError(w, 400, "invalid_grant")
			return
		}
		challenge := sha256.Sum256([]byte(verifier))
		if !same(code.challenge, base64.RawURLEncoding.EncodeToString(challenge[:])) {
			oauthError(w, 400, "invalid_grant")
			return
		}
		id = code.account
	case "refresh_token":
		consumedRefresh = tokenHash(r.PostForm.Get("refresh_token"))
		token, found := m.data.Refresh[consumedRefresh]
		if !found || token.Client != client || token.Expires <= time.Now().Unix() {
			oauthError(w, 400, "invalid_grant")
			return
		}
		if token.Used {
			next := clone(m.data)
			delete(next.Accounts, token.Account)
			if err := m.commitLocked(next); err != nil {
				oauthError(w, 503, "temporarily_unavailable")
				return
			}
			oauthError(w, 400, "invalid_grant")
			return
		}
		id = token.Account
	default:
		oauthError(w, 400, "unsupported_grant_type")
		return
	}
	a, exists := m.data.Accounts[id]
	if !exists || a.Client != client || a.Expires <= time.Now().Unix() {
		oauthError(w, 400, "invalid_grant")
		return
	}
	// Requested refresh scopes cannot broaden the original consent.
	if scopes := strings.Fields(r.PostForm.Get("scope")); len(scopes) > 0 {
		if len(intersection(scopes, a.Scopes)) != len(scopes) {
			oauthError(w, 400, "invalid_scope")
			return
		}
		a.Scopes = scopes
	}
	a.Scopes = intersection(a.Scopes, m.config.Scopes)
	access, err := secret()
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	refresh, err := secret()
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	next := clone(m.data)
	if consumedRefresh == "" {
		a.Expires = time.Now().Add(30 * 24 * time.Hour).Unix()
	} else {
		old := next.Refresh[consumedRefresh]
		old.Used = true
		next.Refresh[consumedRefresh] = old
	}
	next.Accounts[id] = a
	expiry := min(time.Now().Add(time.Hour).Unix(), a.Expires)
	next.Access[tokenHash(access)] = credential{Account: id, Client: client, Expires: expiry}
	next.Refresh[tokenHash(refresh)] = credential{Account: id, Client: client, Expires: a.Expires}
	if err := m.commitLocked(next); err != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": expiry - time.Now().Unix(), "scope": strings.Join(a.Scopes, " ")})
}

func (m *Manager) revoke(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != m.origin {
		oauthError(w, 403, "invalid_request")
		return
	}
	if !postForm(w, r) {
		return
	}
	client, ok := m.identifyClient(w, r)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := tokenHash(r.PostForm.Get("token"))
	credential, found := m.data.Refresh[hash]
	if !found {
		credential, found = m.data.Access[hash]
	}
	if found && credential.Client == client {
		next := clone(m.data)
		delete(next.Accounts, credential.Account)
		if err := m.commitLocked(next); err != nil {
			oauthError(w, 503, "temporarily_unavailable")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
