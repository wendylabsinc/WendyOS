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

	"github.com/google/uuid"
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
	if opts.Issuer == "" {
		return fmt.Errorf("--issuer is required (e.g. https://auth.wendy.sh/realms/acme)")
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

	// Step 2: discovery.
	meta, err := discoverOIDC(ctx, opts.Issuer)
	if err != nil {
		return err
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
	authURL, err := buildAuthorizeURL(meta, opts.ClientID, redirectURI, challenge, state, identityResource)
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
	effectiveIssuer, err := effectiveLoginIssuer(opts.Issuer, result.Iss)
	if err != nil {
		return err
	}
	if effectiveIssuer != strings.TrimSuffix(opts.Issuer, "/") {
		// A different realm was picked in the browser: re-discover so the token
		// exchange and refresh hit the picked realm's endpoints.
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

	// Step 7: verify the binding actually happened.
	//
	// A token without cnf.jkt, or with someone else's thumbprint, cannot be
	// refreshed with the local key. Fail now with a useful client-registration
	// diagnosis instead of saving a broken session.
	claims, err := decodeJWTClaims(identityToken.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting access token: %w", err)
	}
	bound := confirmationThumbprint(claims)
	if bound == "" {
		return fmt.Errorf("access token carries no cnf.jkt: the client %q is probably not registered as DPoP-bound in this realm", opts.ClientID)
	}
	if bound != thumbprint {
		return fmt.Errorf("access token is bound to a different key (cnf.jkt %s, ours %s)", bound, thumbprint)
	}
	fmt.Println(tui.SuccessMessage("Access token is sender-constrained to this key (cnf.jkt matches)."))

	if !audienceContains(claims["aud"], identityResource) {
		return fmt.Errorf("access token audience does not include pki-core identity resource %s", identityResource)
	}
	if issuer, _ := claims["iss"].(string); issuer != effectiveIssuer {
		return fmt.Errorf("access token issuer %q does not match %q", issuer, effectiveIssuer)
	}

	if opts.PrintClaims {
		printClaims(claims, identityToken)
	}

	subject, _ := claims["sub"].(string)
	if subject == "" {
		return fmt.Errorf("pki-core identity token carries no sub claim")
	}
	tenantUUID, _ := claims["tenant_uuid"].(string)
	if tenantUUID == "" {
		return fmt.Errorf("pki-core identity token carries no tenant_uuid: realm %q is not linked to a pki-core tenant", issuerRealm(effectiveIssuer))
	}
	tenantID, err := uuid.Parse(tenantUUID)
	if err != nil {
		return fmt.Errorf("pki-core identity token carries invalid tenant_uuid %q", tenantUUID)
	}
	tenantUUID = tenantID.String()
	if identityToken.RefreshToken == "" {
		return fmt.Errorf("wendy-auth returned no refresh token; cannot obtain a separate Cloud API token after PKI enrollment")
	}

	// Step 8: create a PKCS#10 request with the same key used for DPoP and ask
	// pki-core directly. pki-core enforces the three-way binding between this
	// key, token cnf.jkt, and the proof's embedded JWK.
	fmt.Println(tui.InfoMessage("Requesting an operator certificate from pki-core..."))
	certInfo, err := requestPKIIdentityCertificate(
		ctx, http.DefaultClient, opts.IdentityEndpoint, privateKeyPEM, key,
		identityToken.AccessToken, tenantUUID, subject,
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
	cloudClaims, err := decodeJWTClaims(cloudToken.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting Cloud API access token: %w", err)
	}
	if confirmationThumbprint(cloudClaims) != thumbprint {
		return fmt.Errorf("Cloud API access token is not bound to the generated operator key")
	}
	if !audienceContains(cloudClaims["aud"], cloudResource) {
		return fmt.Errorf("Cloud API access token audience does not include %s", cloudResource)
	}
	if issuer, _ := cloudClaims["iss"].(string); issuer != effectiveIssuer {
		return fmt.Errorf("Cloud API access token issuer %q does not match %q", issuer, effectiveIssuer)
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
	accessToken, tenantUUID, subject string,
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
	principalURI := fmt.Sprintf("spiffe://wendy.sh/tenant/%s/operator/%s", tenantUUID, subject)
	principalMatches := 0
	for _, uri := range leaf.URIs {
		if uri.String() == principalURI {
			principalMatches++
			continue
		}
		if uri.Scheme == "spiffe" && uri.Host == "wendy.sh" && strings.HasPrefix(uri.Path, "/tenant/") {
			return config.CertificateInfo{}, fmt.Errorf("pki-core returned a certificate for a different principal")
		}
	}
	if principalMatches != 1 {
		return config.CertificateInfo{}, fmt.Errorf("pki-core certificate does not contain the expected operator identity %s", principalURI)
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
	claims, err := decodeJWTClaims(identityToken.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting pki-core identity token: %w", err)
	}
	if confirmationThumbprint(claims) != thumbprint {
		return fmt.Errorf("pki-core identity token is not bound to the stored DPoP key")
	}
	if !audienceContains(claims["aud"], identityResource) {
		return fmt.Errorf("pki-core identity token audience does not include %s", identityResource)
	}
	if issuer, _ := claims["iss"].(string); issuer != strings.TrimSuffix(auth.OAuthIssuer, "/") {
		return fmt.Errorf("pki-core identity token issuer %q does not match %q", issuer, strings.TrimSuffix(auth.OAuthIssuer, "/"))
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return fmt.Errorf("pki-core identity token carries no sub claim")
	}
	tenantUUID, _ := claims["tenant_uuid"].(string)
	if tenantUUID == "" {
		return fmt.Errorf("pki-core identity token carries no tenant_uuid: realm %q is not linked to a pki-core tenant", issuerRealm(auth.OAuthIssuer))
	}
	tenantID, err := uuid.Parse(tenantUUID)
	if err != nil {
		return fmt.Errorf("pki-core identity token carries invalid tenant_uuid %q", tenantUUID)
	}
	tenantUUID = tenantID.String()

	certInfo, err := requestPKIIdentityCertificate(
		ctx, http.DefaultClient, identityEndpoint, privateKeyPEM, key,
		identityToken.AccessToken, tenantUUID, subject,
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
	cloudClaims, err := decodeJWTClaims(cloudToken.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting refreshed Cloud API token: %w", err)
	}
	if confirmationThumbprint(cloudClaims) != thumbprint {
		return fmt.Errorf("refreshed Cloud API token is not bound to the stored DPoP key")
	}
	if !audienceContains(cloudClaims["aud"], auth.OAuthResource) {
		return fmt.Errorf("refreshed Cloud API token audience does not include %s", auth.OAuthResource)
	}
	if issuer, _ := cloudClaims["iss"].(string); issuer != strings.TrimSuffix(auth.OAuthIssuer, "/") {
		return fmt.Errorf("refreshed Cloud API token issuer %q does not match %q", issuer, strings.TrimSuffix(auth.OAuthIssuer, "/"))
	}
	auth.APIKey = cloudToken.AccessToken
	auth.OAuthExpiresAt = time.Now().Add(time.Duration(cloudToken.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	auth.PKIResource = identityResource
	auth.PKIEndpoint = identityEndpoint
	auth.Certificates = []config.CertificateInfo{certInfo}
	clitimesync.CacheProof(ctx)
	return nil
}

func ensureOAuthAccessToken(ctx context.Context, auth *config.AuthConfig) error {
	expiresAt, err := time.Parse(time.RFC3339, auth.OAuthExpiresAt)
	if err == nil && time.Until(expiresAt) > 90*time.Second {
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
	expiresAt, err = time.Parse(time.RFC3339, auth.OAuthExpiresAt)
	if err == nil && time.Until(expiresAt) > 90*time.Second {
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
	claims, err := decodeJWTClaims(token.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting refreshed access token: %w", err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		return fmt.Errorf("computing OAuth DPoP thumbprint: %w", err)
	}
	if confirmationThumbprint(claims) != thumbprint {
		return fmt.Errorf("refreshed access token is not bound to the stored DPoP key")
	}
	if !audienceContains(claims["aud"], auth.OAuthResource) {
		return fmt.Errorf("refreshed access token audience does not include %s", auth.OAuthResource)
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
func buildAuthorizeURL(meta *oidcProviderMetadata, clientID, redirectURI, challenge, state, resource string) (string, error) {
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
