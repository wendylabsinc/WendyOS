package commands

// OpenID Connect login against wendy-auth for the Wendy Cloud API and
// pki-core. The native client uses authorization code + PKCE and binds both
// resource tokens and the operator CSR to one local DPoP key.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
)

// oidcScopes: `groups` is read once at session establishment and drives cloud's
// live authorization decisions. The realm must advertise it.
const oidcScopes = "openid email profile groups"

// oidcLoginOptions configures a single login attempt.
type oidcLoginOptions struct {
	// Issuer is the realm issuer URL, e.g.
	// https://auth.wendy.sh/realms/acme — NOT the bare host: every realm is a
	// separate issuer with its own keys (see wendy-auth's multi-tenancy model).
	Issuer string
	// ClientID is the public client registered in that realm. Public + PKCE,
	// token_endpoint_auth_method=none: a CLI cannot keep a secret.
	ClientID string
	// CloudResource is the audience used for the access token persisted for
	// ordinary Cloud API calls. IdentityResource is the pki-core audience used
	// only during certificate bootstrap.
	CloudResource    string
	IdentityResource string
	// IdentityEndpoint is pki-core's operator-identity CSR endpoint. Its exact
	// canonical URL is bound into the resource-request DPoP proof.
	IdentityEndpoint string
	// CloudURL and CloudGRPC identify the API environment this session targets.
	CloudURL  string
	CloudGRPC string
	// PrintClaims dumps the decoded access-token payload after exchange.
	PrintClaims bool
}

// oidcProviderMetadata is the subset of OIDC discovery this flow needs.
type oidcProviderMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	GrantTypes            []string `json:"grant_types_supported"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// oidcTokenResponse is the token endpoint's success payload.
type oidcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
}

// discoverOIDCIssuerFn is indirected so command tests can verify target routing
// without contacting a live wendy-auth deployment.
var discoverOIDCIssuerFn = discoverOIDCIssuer

// discoverOIDCIssuer uses wendy-auth's identifier-first API to route an email
// address to its home realm without exposing or requiring an organization list.
func discoverOIDCIssuer(ctx context.Context, authBase, email string) (string, error) {
	if strings.TrimSpace(email) == "" {
		return "", fmt.Errorf("--email is required when --issuer is not set")
	}
	authBase = strings.TrimSuffix(authBase, "/")
	body, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		return "", fmt.Errorf("encoding realm discovery request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authBase+"/api/login/realm", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building realm discovery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovering organization: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("organization discovery returned %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var result struct {
		LoginURL string `json:"loginURL"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding organization discovery response: %w", err)
	}
	u, err := url.Parse(result.LoginURL)
	if err != nil {
		return "", fmt.Errorf("parsing organization login URL: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "realms" || parts[1] == "" {
		return "", fmt.Errorf("organization discovery returned an invalid login URL")
	}
	return authBase + "/realms/" + url.PathEscape(parts[1]), nil
}

func issuerRealm(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil {
		return issuer
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := range parts {
		if parts[i] == "realms" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return issuer
}

// effectiveLoginIssuer picks the realm to exchange the authorization code at,
// honouring the RFC 9207 issuer (callbackIss) from the authorization response.
// callbackIss is "" when the server sent none, in which case the requested realm
// stands. A present issuer on a different origin than requested is refused: an
// unrelated host must never receive this code (the RFC 9207 mix-up defence). The
// returned issuer is trailing-slash-trimmed for direct comparison with token
// `iss` claims.
func effectiveLoginIssuer(requested, callbackIss string) (string, error) {
	requested = strings.TrimSuffix(requested, "/")
	if callbackIss == "" {
		return requested, nil
	}
	if !sameIssuerOrigin(callbackIss, requested) {
		return "", fmt.Errorf("authorization response issuer %q is not on the same host as the requested realm %q; refusing to exchange the code", callbackIss, requested)
	}
	return strings.TrimSuffix(callbackIss, "/"), nil
}

// sameIssuerOrigin reports whether two issuer URLs share scheme and host, i.e.
// name realms on the same wendy-auth deployment. The RFC 9207 issuer on an
// authorization response may switch realms (path) but must never move the code
// exchange to a different host — that would hand the authorization code to an
// unrelated issuer (the mix-up attack the check defends against).
func sameIssuerOrigin(a, b string) bool {
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil || ua.Scheme == "" || ua.Host == "" {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// discoverOIDC fetches the realm's OIDC metadata.
//
// It deliberately checks that the advertised issuer matches the one requested:
// a discovery document that names a different issuer is either a
// misconfiguration or an attempt to redirect the client to another realm, and
// every downstream check (token `iss`, JWKS location) keys off this value.
func discoverOIDC(ctx context.Context, issuer string) (*oidcProviderMetadata, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	discoveryURL := issuer + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building discovery request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("discovery at %s returned %d: %s", discoveryURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var meta oidcProviderMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding discovery document: %w", err)
	}
	if meta.Issuer != issuer {
		return nil, fmt.Errorf("discovery issuer mismatch: requested %q, document declares %q", issuer, meta.Issuer)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("discovery document missing authorization_endpoint or token_endpoint")
	}
	// PKCE S256 is mandatory on this authorization server; fail loudly rather
	// than silently downgrading to a flow it will reject anyway.
	if len(meta.CodeChallengeMethods) > 0 && !oidcContainsString(meta.CodeChallengeMethods, "S256") {
		return nil, fmt.Errorf("realm does not advertise PKCE S256 (got %v)", meta.CodeChallengeMethods)
	}
	return &meta, nil
}

// base64URL encodes without padding, as every JOSE/OAuth structure requires.
func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// newPKCEVerifier returns a high-entropy code_verifier and its S256 challenge.
func newPKCEVerifier() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generating PKCE verifier: %w", err)
	}
	verifier = base64URL(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64URL(sum[:]), nil
}

// randomURLSafe returns n bytes of entropy, base64url-encoded. Used for `state`
// and DPoP `jti`.
func randomURLSafe(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64URL(raw), nil
}

// The DPoP proof builder and the operator JWK/JWS helpers live in the shared
// cloudrequest package (WDY-3107) so the gRPC broker-path interceptor and the
// MCP tools path reuse the one signer. These thin wrappers keep the established
// OIDC login/refresh/enroll call sites and their tests unchanged.

func operatorPublicJWK(signer crypto.Signer) (map[string]string, string, error) {
	return cloudrequest.OperatorPublicJWK(signer)
}

func operatorJWKThumbprint(signer crypto.Signer) (string, error) {
	return cloudrequest.OperatorJWKThumbprint(signer)
}

func newDPoPProof(key crypto.Signer, htm, htu, nonce string) (string, error) {
	return cloudrequest.NewDPoPProof(key, htm, htu, nonce)
}

func newDPoPAccessProof(key crypto.Signer, htm, htu, accessToken string) (string, error) {
	return cloudrequest.NewDPoPAccessProof(key, htm, htu, accessToken)
}

func canonicalHTU(raw string) (string, error) {
	return cloudrequest.CanonicalHTU(raw)
}

// decodeJWTClaims returns the decoded payload of a JWS compact token.
//
// This does NOT verify the signature: it exists so the CLI can inspect what it
// received and assert the cnf.jkt binding locally. Cloud verifies the token
// signature, issuer, and audience before authorizing an API request.
func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a compact JWS (%d segments)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding token payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parsing token claims: %w", err)
	}
	return claims, nil
}

// confirmationThumbprint extracts cnf.jkt from decoded claims, if present.
func confirmationThumbprint(claims map[string]any) string {
	cnf, ok := claims["cnf"].(map[string]any)
	if !ok {
		return ""
	}
	jkt, _ := cnf["jkt"].(string)
	return jkt
}

func oidcContainsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

const (
	oidcCallbackAddr = "127.0.0.1:8765"
	oidcRedirectURI  = "http://127.0.0.1:8765/callback"
)

// startLoopbackListener binds the static OIDC callback port and returns its
// registered redirect URI.
//
// Loopback rather than the device-code grant: wendy-auth does not advertise
// urn:ietf:params:oauth:grant-type:device_code, and the existing
// `wendy auth login` already uses a loopback callback, so this keeps one shape.
func startLoopbackListener() (net.Listener, string, error) {
	listener, err := net.Listen("tcp", oidcCallbackAddr)
	if err != nil {
		return nil, "", fmt.Errorf("binding OIDC callback server to %s: %w", oidcCallbackAddr, err)
	}
	return listener, oidcRedirectURI, nil
}

// exchangeCodeForToken performs the authorization_code exchange with a DPoP
// proof and the RFC 8707 resource indicator.
//
// If the server answers `use_dpop_nonce`, the proof is rebuilt once with the
// supplied nonce and retried — RFC 9449 §8 requires clients to handle this, and
// wendy-auth can be configured to demand it
// (WENDY_AUTH_DPOP_REQUIRE_NONCE).
func exchangeCodeForToken(
	ctx context.Context,
	key crypto.Signer,
	meta *oidcProviderMetadata,
	clientID, code, verifier, redirectURI, resource string,
) (*oidcTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	if resource != "" {
		form.Set("resource", resource)
	}
	return exchangeDPoPToken(ctx, key, meta.TokenEndpoint, form)
}

func refreshOIDCToken(
	ctx context.Context,
	key crypto.Signer,
	meta *oidcProviderMetadata,
	clientID, refreshToken, resource string,
) (*oidcTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	if resource != "" {
		form.Set("resource", resource)
	}
	return exchangeDPoPToken(ctx, key, meta.TokenEndpoint, form)
}

func exchangeDPoPToken(
	ctx context.Context,
	key crypto.Signer,
	tokenEndpoint string,
	form url.Values,
) (*oidcTokenResponse, error) {
	htu, err := canonicalHTU(tokenEndpoint)
	if err != nil {
		return nil, err
	}

	doRequest := func(nonce string) (*http.Response, error) {
		proof, err := newDPoPProof(key, http.MethodPost, htu, nonce)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, fmt.Errorf("building token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", proof)
		return http.DefaultClient.Do(req)
	}

	resp, err := doRequest("")
	if err != nil {
		return nil, fmt.Errorf("calling token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		if oauthErr.Error == "use_dpop_nonce" {
			nonce := resp.Header.Get("DPoP-Nonce")
			if nonce == "" {
				return nil, fmt.Errorf("server demanded a DPoP nonce but sent no DPoP-Nonce header")
			}
			_ = resp.Body.Close()
			retry, err := doRequest(nonce)
			if err != nil {
				return nil, fmt.Errorf("retrying token request with nonce: %w", err)
			}
			defer func() { _ = retry.Body.Close() }()
			return parseTokenResponse(retry)
		}
		return nil, fmt.Errorf("token endpoint returned 400: %s", strings.TrimSpace(string(body)))
	}

	return parseTokenResponse(resp)
}

func parseTokenResponse(resp *http.Response) (*oidcTokenResponse, error) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out oidcTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token response contained no access_token")
	}
	return &out, nil
}
