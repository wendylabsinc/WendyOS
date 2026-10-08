package commands

// The interactive wendy-auth browser round-trip, token persistence, and refresh
// path. The discovery, PKCE, JWK thumbprint, and DPoP primitives live in
// auth_oidc.go.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// oidcCallbackResult carries what the redirect handler observed.
type oidcCallbackResult struct {
	Code  string
	State string
	// Iss is the RFC 9207 issuer identifier the authorization response carried,
	// naming the realm that actually issued the code. It differs from the
	// requested realm when the browser org picker (wendy-auth) switched realms.
	// Empty when the server did not send it.
	Iss string
	Err error
}

// performOIDCLogin creates and saves a refreshable Cloud API session.
//
// The DPoP key is generated first because wendy-auth binds the access and
// refresh-token family to its thumbprint.
func performOIDCLogin(ctx context.Context, opts oidcLoginOptions) error {
	if opts.Issuer == "" && opts.AuthorizationBase == "" {
		return fmt.Errorf("an OIDC issuer or authorization base is required")
	}
	if opts.ClientID == "" {
		return fmt.Errorf("--client-id is required")
	}
	if opts.CloudResource == "" {
		return fmt.Errorf("--resource is required for OIDC login")
	}
	if opts.IdentityResource == "" {
		return fmt.Errorf("--pki-resource is required for OIDC login")
	}
	if opts.IdentityEndpoint == "" {
		return fmt.Errorf("--pki-identity-endpoint is required for OIDC login")
	}
	if opts.CloudGRPC == "" {
		return fmt.Errorf("--cloud-grpc is required for OIDC login")
	}
	cloudResource := opts.CloudResource
	identityResource := opts.IdentityResource

	// Step 1: the key, before anything else.
	privateKeyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return fmt.Errorf("generating operator key: %w", err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(privateKeyPEM))
	if err != nil {
		return fmt.Errorf("parsing generated key: %w", err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		return fmt.Errorf("computing key thumbprint: %w", err)
	}
	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Generated operator key (jkt %s).", thumbprint)))

	// Step 2: explicit realms use discovery immediately. Realm-less login starts
	// only at the configured authority's fixed global endpoint; discovery waits
	// until the callback identifies the selected realm.
	var meta *oidcProviderMetadata
	if opts.Issuer != "" {
		opts.Issuer, err = validateRealmIssuer(opts.Issuer, opts.Issuer)
		if err != nil {
			return err
		}
		meta, err = discoverOIDC(ctx, opts.Issuer)
		if err != nil {
			return err
		}
	} else {
		opts.AuthorizationBase, err = canonicalAuthorizationBase(opts.AuthorizationBase)
		if err != nil {
			return err
		}
		meta = &oidcProviderMetadata{AuthorizationEndpoint: opts.AuthorizationBase + "/authorize"}
	}

	// Step 3: loopback listener for the redirect.
	listener, redirectURI, err := startLoopbackListener()
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	verifier, challenge, err := newPKCEVerifier()
	if err != nil {
		return err
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return fmt.Errorf("generating state: %w", err)
	}
	nonce, err := randomURLSafe(16)
	if err != nil {
		return fmt.Errorf("generating nonce: %w", err)
	}

	resultCh := make(chan oidcCallbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			desc := q.Get("error_description")
			http.Error(w, "authorization failed: "+errCode, http.StatusBadRequest)
			resultCh <- oidcCallbackResult{Err: fmt.Errorf("authorization failed: %s %s", errCode, desc)}
			return
		}
		code := q.Get("code")
		gotState := q.Get("state")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			resultCh <- oidcCallbackResult{Err: fmt.Errorf("callback received without an authorization code")}
			return
		}
		// Constant-time compare: `state` is a CSRF defence, so treat it like a
		// secret even though a timing leak here is a stretch.
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(state)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			resultCh <- oidcCallbackResult{Err: fmt.Errorf("state mismatch: the callback did not originate from this login attempt")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Wendy CLI</title>` +
			`<p style="font-family:system-ui;padding:2rem">Signed in. You can close this tab and return to the terminal.</p>`))
		resultCh <- oidcCallbackResult{Code: code, State: gotState, Iss: q.Get("iss")}
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			resultCh <- oidcCallbackResult{Err: fmt.Errorf("callback server: %w", serveErr)}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// Step 4: send the operator to the realm's authorize endpoint.
	authURL, err := buildAuthorizeURL(meta, opts.ClientID, redirectURI, challenge, state, nonce, identityResource)
	if err != nil {
		return err
	}
	if !humanPresent() {
		printLoginURLForManualOpen(authURL)
	} else {
		fmt.Println(tui.InfoMessage("Opening your browser to sign in..."))
		fmt.Println("  " + authURL)
		if openErr := openBrowser(authURL); openErr != nil {
			fmt.Println(tui.WarningMessage("Could not open a browser automatically; open the URL above manually."))
		}
	}

	// Step 5: wait for the redirect.
	var result oidcCallbackResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(browserLoginTimeout):
		return browserLoginTimeoutError()
	}
	if result.Err != nil {
		return result.Err
	}

	// Step 5b: honour the RFC 9207 issuer. When the browser org picker switches
	// realms, the callback names the realm that issued the code; the code must be
	// exchanged at THAT realm's token endpoint, and the resulting context's org
	// is derived from the token it returns — never from the realm we requested.
	effectiveIssuer, err := effectiveLoginIssuer(opts.Issuer, opts.AuthorizationBase, result.Iss)
	if err != nil {
		return err
	}
	if opts.Issuer == "" || effectiveIssuer != strings.TrimSuffix(opts.Issuer, "/") {
		// A realm-less start or browser realm switch discovers only after the
		// callback issuer passed the authority and canonical-path checks.
		meta, err = discoverOIDC(ctx, effectiveIssuer)
		if err != nil {
			return err
		}
	}

	// Step 6: exchange the code, DPoP-bound to the key from step 1.
	identityToken, err := exchangeCodeForToken(ctx, key, meta, opts.ClientID, result.Code, verifier, redirectURI, identityResource)
	if err != nil {
		return err
	}

	// Step 7: verify both signed OIDC identity and sender-constrained access.
	// The nonce binds the ID token to this browser attempt; the access-token
	// signature makes cnf, audience, issuer, and subject safe to inspect locally.
	tokenVerifier, err := loadOIDCTokenVerifier(ctx, meta)
	if err != nil {
		return err
	}
	subject, err := verifyIDToken(tokenVerifier, identityToken, opts.ClientID, nonce)
	if err != nil {
		return err
	}
	claims, err := verifyAccessToken(tokenVerifier, identityToken.AccessToken, identityResource, thumbprint)
	if err != nil {
		return fmt.Errorf("verifying pki-core identity access token: %w", err)
	}
	if accessSubject, _ := claims["sub"].(string); accessSubject != subject {
		return fmt.Errorf("ID token and access token identify different subjects")
	}
	fmt.Println(tui.SuccessMessage("Access token signature and sender constraint verified."))

	if opts.PrintClaims {
		printClaims(claims, identityToken)
	}
	if identityToken.RefreshToken == "" {
		return fmt.Errorf("wendy-auth returned no refresh token; cannot obtain a separate Cloud API token after PKI enrollment")
	}

	// Step 8: create a PKCS#10 request with the same key used for DPoP and ask
	// pki-core directly. pki-core enforces the three-way binding between this
	// key, token cnf.jkt, and the proof's embedded JWK.
	fmt.Println(tui.InfoMessage("Requesting an operator certificate from pki-core..."))
	certInfo, err := requestPKIIdentityCertificate(
		ctx, http.DefaultClient, opts.IdentityEndpoint, privateKeyPEM, key,
		identityToken.AccessToken, "", subject,
	)
	if err != nil {
		return err
	}

	// Step 9: wendy-auth currently issues one resource audience at a time.
	// Rotate the same sender-constrained refresh-token family from the PKI
	// audience to the Cloud API audience, and persist only this API token.
	cloudToken, err := refreshOIDCToken(
		ctx, key, meta, opts.ClientID, identityToken.RefreshToken, cloudResource,
	)
	if err != nil {
		return fmt.Errorf("obtaining Cloud API token after certificate enrollment: %w", err)
	}
	cloudClaims, err := verifyAccessToken(tokenVerifier, cloudToken.AccessToken, cloudResource, thumbprint)
	if err != nil {
		return fmt.Errorf("verifying Cloud API access token: %w", err)
	}
	if cloudSubject, _ := cloudClaims["sub"].(string); cloudSubject != subject {
		return fmt.Errorf("Cloud API token identifies a different subject")
	}
	refreshToken := cloudToken.RefreshToken
	if refreshToken == "" {
		refreshToken = identityToken.RefreshToken
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	authEntry := config.AuthConfig{
		CloudDashboard: opts.CloudURL,
		CloudGRPC:      opts.CloudGRPC,
		APIKey:         cloudToken.AccessToken,
		OAuthIssuer:    effectiveIssuer,
		OAuthClientID:  opts.ClientID,
		OAuthResource:  cloudResource,
		PKIResource:    identityResource,
		PKIEndpoint:    opts.IdentityEndpoint,
		OAuthExpiresAt: time.Now().Add(time.Duration(cloudToken.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
		RefreshToken:   refreshToken,
		DPoPPrivateKey: privateKeyPEM,
		Certificates:   []config.CertificateInfo{certInfo},
	}
	cfg.AddAuth(authEntry)
	// Name the new session as a context; the first login becomes "default" and
	// current. A later login does not change the current context.
	cfg.EnsureContexts()
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving OAuth session and certificates: %w", err)
	}
	// Record the org's display name while the access token is fresh, so
	// pickers and labels show it rather than the tenant UUID.
	cloudOrganizationName(ctx, &authEntry)
	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Signed in to %s. API session and certificates saved.", issuerRealm(effectiveIssuer))))
	fmt.Println(sessionKeyLine(keyAlgorithmName(key.Public()), "OIDC"))
	clitimesync.CacheProof(ctx)
	return nil
}

type oidcHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// requestPKIIdentityCertificate implements pki-core's operator identity wire
// contract: raw PKCS#10 body, DPoP authorization, and a leaf-first PEM chain.
func requestPKIIdentityCertificate(
	ctx context.Context,
	client oidcHTTPDoer,
	endpoint, privateKeyPEM string,
	key crypto.Signer,
	accessToken, expectedTenant, subject string,
) (config.CertificateInfo, error) {
	if accessToken == "" {
		return config.CertificateInfo{}, fmt.Errorf("requesting pki-core identity certificate: access token is empty")
	}
	htu, err := canonicalHTU(endpoint)
	if err != nil {
		return config.CertificateInfo{}, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return config.CertificateInfo{}, fmt.Errorf("invalid pki-core identity endpoint %q", endpoint)
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if (u.Scheme != "https" && !(u.Scheme == "http" && loopback)) ||
		u.User != nil || u.Fragment != "" || u.Path != "/v1/identity/certificate" {
		return config.CertificateInfo{}, fmt.Errorf("invalid pki-core identity endpoint %q: want HTTPS (or loopback HTTP) with path /v1/identity/certificate", endpoint)
	}
	csrPEM, err := certs.GenerateCSR([]byte(privateKeyPEM), subject, nil)
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("generating operator CSR: %w", err)
	}
	proof, err := newDPoPAccessProof(key, http.MethodPost, htu, accessToken)
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("building pki-core DPoP proof: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(csrPEM))
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("building pki-core identity request: %w", err)
	}
	req.Header.Set("Authorization", "DPoP "+accessToken)
	req.Header.Set("DPoP", proof)
	req.Header.Set("Content-Type", "application/pkcs10")
	req.Header.Set("Accept", "application/pem-certificate-chain")

	resp, err := client.Do(req)
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("calling pki-core identity endpoint %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil {
		return config.CertificateInfo{}, fmt.Errorf("reading pki-core identity response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusUnauthorized {
			detail = "unauthorized (verify the realm-to-tenant mapping and PKI audience)"
		}
		return config.CertificateInfo{}, fmt.Errorf("pki-core identity endpoint returned %d: %s", resp.StatusCode, detail)
	}
	if len(body) > 1<<20 {
		return config.CertificateInfo{}, fmt.Errorf("pki-core identity response exceeds 1 MiB")
	}
	leafPEM, chainPEM, leaf, err := splitCertificateChainPEM(body)
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("parsing pki-core certificate chain: %w", err)
	}
	wantPublicKey, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("encoding generated operator public key: %w", err)
	}
	gotPublicKey, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(gotPublicKey, wantPublicKey) {
		return config.CertificateInfo{}, fmt.Errorf("pki-core returned a certificate for a different key")
	}
	principalURI, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok {
		return config.CertificateInfo{}, fmt.Errorf("pki-core certificate must contain exactly one tenant SPIFFE principal")
	}
	identity, err := certs.ParsePrincipal(principalURI)
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("parsing pki-core certificate principal: %w", err)
	}
	operatorPrefix := fmt.Sprintf("spiffe://wendy.sh/tenant/%s/operator/", identity.TenantUUID)
	if !strings.HasPrefix(principalURI, operatorPrefix) || identity.EntityID != subject {
		return config.CertificateInfo{}, fmt.Errorf("pki-core returned a certificate for a different operator")
	}
	if expectedTenant != "" && identity.TenantUUID != expectedTenant {
		return config.CertificateInfo{}, fmt.Errorf("pki-core returned tenant %q while renewing tenant %q", identity.TenantUUID, expectedTenant)
	}
	return config.CertificateInfo{
		PemCertificate:      leafPEM,
		PemCertificateChain: chainPEM,
		PemPrivateKey:       privateKeyPEM,
		PrincipalURI:        principalURI,
	}, nil
}

func splitCertificateChainPEM(chain []byte) (string, string, *x509.Certificate, error) {
	rest := chain
	var encoded [][]byte
	var leaf *x509.Certificate
	for len(bytes.TrimSpace(rest)) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return "", "", nil, fmt.Errorf("response contains non-certificate PEM data")
		}
		if leaf == nil {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return "", "", nil, err
			}
			leaf = cert
		}
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
		rest = remaining
	}
	if leaf == nil {
		return "", "", nil, fmt.Errorf("response contains no certificate")
	}
	return string(encoded[0]), string(bytes.Join(encoded[1:], nil)), leaf, nil
}

// refreshOIDCCertificate replays the direct pki-core flow for an existing
// OAuth session. The refresh-token family is first scoped to pki-core and then
// rotated back to the Cloud API resource. The DPoP key cannot change here:
// pki-core requires it to remain the CSR key as well.
func refreshOIDCCertificate(ctx context.Context, auth *config.AuthConfig) error {
	refreshToken, err := auth.OAuthRefreshToken()
	if err != nil {
		return fmt.Errorf("loading OAuth refresh token: %w", err)
	}
	if refreshToken == "" {
		return fmt.Errorf("OAuth session has no refresh token; sign in again")
	}
	privateKeyPEM, err := auth.OAuthDPoPKey()
	if err != nil {
		return fmt.Errorf("loading OAuth DPoP key: %w", err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(privateKeyPEM))
	if err != nil {
		return fmt.Errorf("parsing OAuth DPoP key: %w", err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		return fmt.Errorf("computing OAuth DPoP thumbprint: %w", err)
	}
	meta, err := discoverOIDC(ctx, auth.OAuthIssuer)
	if err != nil {
		return err
	}
	identityResource := auth.PKIResource
	if identityResource == "" {
		identityResource = defaultPKIIdentityResource
	}
	identityEndpoint := auth.PKIEndpoint
	if identityEndpoint == "" {
		identityEndpoint = defaultDevPKIIdentityEndpoint
	}

	identityToken, err := refreshOIDCToken(
		ctx, key, meta, auth.OAuthClientID, refreshToken, identityResource,
	)
	if err != nil {
		return fmt.Errorf("obtaining pki-core identity token: %w", err)
	}
	if identityToken.RefreshToken == "" {
		return fmt.Errorf("wendy-auth did not rotate the refresh token for pki-core enrollment")
	}
	// The old handle has already been consumed. Retain the rotated one even if
	// a later validation or issuance step fails; refreshAllCerts persists this
	// mutation so the user's login is not stranded.
	auth.RefreshToken = identityToken.RefreshToken
	tokenVerifier, err := loadOIDCTokenVerifier(ctx, meta)
	if err != nil {
		return err
	}
	claims, err := verifyAccessToken(tokenVerifier, identityToken.AccessToken, identityResource, thumbprint)
	if err != nil {
		return fmt.Errorf("verifying pki-core identity token: %w", err)
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return fmt.Errorf("pki-core identity token carries no sub claim")
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return fmt.Errorf("stored OAuth session has no authoritative tenant certificate; sign in again")
	}
	expectedTenant := auth.Certificates[0].TenantUUID()

	certInfo, err := requestPKIIdentityCertificate(
		ctx, http.DefaultClient, identityEndpoint, privateKeyPEM, key,
		identityToken.AccessToken, expectedTenant, subject,
	)
	if err != nil {
		return err
	}
	cloudToken, err := refreshOIDCToken(
		ctx, key, meta, auth.OAuthClientID, identityToken.RefreshToken, auth.OAuthResource,
	)
	if err != nil {
		return fmt.Errorf("restoring Cloud API token after certificate refresh: %w", err)
	}
	if cloudToken.RefreshToken != "" {
		auth.RefreshToken = cloudToken.RefreshToken
	}
	cloudClaims, err := verifyAccessToken(tokenVerifier, cloudToken.AccessToken, auth.OAuthResource, thumbprint)
	if err != nil {
		return fmt.Errorf("verifying refreshed Cloud API token: %w", err)
	}
	if cloudSubject, _ := cloudClaims["sub"].(string); cloudSubject != subject {
		return fmt.Errorf("refreshed Cloud API token identifies a different subject")
	}
	auth.APIKey = cloudToken.AccessToken
	auth.OAuthExpiresAt = time.Now().Add(time.Duration(cloudToken.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	auth.PKIResource = identityResource
	auth.PKIEndpoint = identityEndpoint
	auth.Certificates = []config.CertificateInfo{certInfo}
	clitimesync.CacheProof(ctx)
	return nil
}

// oauthAccessTokenFreshFor reports whether the stored access token stays valid
// for longer than d.
func oauthAccessTokenFreshFor(auth *config.AuthConfig, d time.Duration) bool {
	expiresAt, err := time.Parse(time.RFC3339, auth.OAuthExpiresAt)
	return err == nil && time.Until(expiresAt) > d
}

func ensureOAuthAccessToken(ctx context.Context, auth *config.AuthConfig) error {
	if oauthAccessTokenFreshFor(auth, 90*time.Second) {
		return nil
	}
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := reloadOAuthSession(auth); err != nil {
		return err
	}
	// The process that held the lock may already have consumed our old token.
	if oauthAccessTokenFreshFor(auth, 90*time.Second) {
		return nil
	}
	if auth.ServiceAccount != "" {
		return remintServiceAccountSession(ctx, auth)
	}
	refreshToken, err := auth.OAuthRefreshToken()
	if err != nil {
		return fmt.Errorf("loading OAuth refresh token: %w", err)
	}
	if refreshToken == "" {
		return fmt.Errorf("OAuth session expired and has no refresh token; run 'wendy auth login --email <email>' again")
	}
	keyPEM, err := auth.OAuthDPoPKey()
	if err != nil {
		return fmt.Errorf("loading OAuth DPoP key: %w", err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		return fmt.Errorf("parsing OAuth DPoP key: %w", err)
	}
	meta, err := discoverOIDC(ctx, auth.OAuthIssuer)
	if err != nil {
		return err
	}
	token, err := refreshOIDCToken(ctx, key, meta, auth.OAuthClientID, refreshToken, auth.OAuthResource)
	if err != nil {
		return fmt.Errorf("refreshing OAuth session: %w", err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		return fmt.Errorf("computing OAuth DPoP thumbprint: %w", err)
	}
	tokenVerifier, err := loadOIDCTokenVerifier(ctx, meta)
	if err != nil {
		return err
	}
	if _, err := verifyAccessToken(tokenVerifier, token.AccessToken, auth.OAuthResource, thumbprint); err != nil {
		return fmt.Errorf("verifying refreshed access token: %w", err)
	}
	auth.APIKey = token.AccessToken
	if token.RefreshToken != "" {
		auth.RefreshToken = token.RefreshToken
	}
	auth.OAuthExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	return persistOAuthSession(auth)
}

func persistOAuthSession(auth *config.AuthConfig) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	for i := range cfg.Auth {
		if sameOAuthSession(&cfg.Auth[i], auth) {
			cfg.Auth[i] = *auth
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving refreshed OAuth session: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("OAuth session is no longer present in config")
}

// buildAuthorizeURL assembles the authorization request.
func buildAuthorizeURL(meta *oidcProviderMetadata, clientID, redirectURI, challenge, state, nonce, resource string) (string, error) {
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("parsing authorization_endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", oidcScopes)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if resource != "" {
		// Carried on the authorization request as well as the token request:
		// RFC 8707 allows either, and sending it here lets the server refuse an
		// unknown resource before the user authenticates.
		q.Set("resource", resource)
	}
	// url.Values.Encode() renders spaces as "+", which is correct only for an
	// application/x-www-form-urlencoded BODY. In a query string "+" is a
	// literal plus, and wendy-auth parses it strictly: `scope=openid+email`
	// arrives as the single unknown scope "openid+email" and the request is
	// rejected with invalid_scope. Percent-encoding is unambiguous in both
	// contexts, so rewrite the separators.
	//
	// Safe as a blanket replacement here: every other parameter is base64url
	// (which uses '-' and '_', never '+') or a loopback URI.
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String(), nil
}

// audienceContains reports whether `aud` includes want. RFC 7519 §4.1.3 permits
// either a bare string or an array.
func audienceContains(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func printClaims(claims map[string]any, token *oidcTokenResponse) {
	fmt.Println()
	fmt.Println("Access token claims:")
	for _, k := range []string{"iss", "sub", "org_id", "tenant_uuid", "aud", "scope", "exp"} {
		v, ok := claims[k]
		if !ok {
			continue
		}
		// encoding/json decodes every JSON number as float64, so a numeric date
		// like exp prints as 1.785860416e+09 under %v. Render whole numbers as
		// integers, and expiries as a human-readable time as well.
		if f, isNum := v.(float64); isNum && f == math.Trunc(f) {
			if k == "exp" || k == "iat" || k == "nbf" {
				ts := time.Unix(int64(f), 0)
				fmt.Printf("  %-12s %d  (%s, in %s)\n", k, int64(f),
					ts.Format(time.RFC3339), time.Until(ts).Round(time.Second))
				continue
			}
			fmt.Printf("  %-12s %d\n", k, int64(f))
			continue
		}
		fmt.Printf("  %-12s %v\n", k, v)
	}
	if _, ok := claims["tenant_uuid"]; !ok {
		// Cloud uses this claim to map the wendy-auth realm to an organization.
		fmt.Println(tui.WarningMessage(
			"  no tenant_uuid claim — this realm is not linked to a Cloud organization."))
	}
	if token.RefreshToken != "" {
		fmt.Printf("  %-12s (present)\n", "refresh")
	}
	fmt.Println()
}
