// Package browserauth implements the CLI's PKCE, DPoP and operator certificate flow
// without filesystem storage or a native loopback listener.
package browserauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcmetadata "google.golang.org/grpc/metadata"
)

const AuthBase = "https://auth.dev.wendy.sh"
const IdentityResource = "https://pki.wendy.sh/identity"
const IdentityEndpoint = "https://identity.dev.pki.wendy.sh/v1/identity/certificate"
const CloudResource = "https://cloud.dev.wendy.sh/api"
const ClientID = "cloud-login"

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}
type metadata struct {
	Issuer    string `json:"issuer"`
	Authorize string `json:"authorization_endpoint"`
	Token     string `json:"token_endpoint"`
	JWKS      string `json:"jwks_uri"`
}
type token struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Type    string `json:"token_type"`
	Expires int    `json:"expires_in"`
}
type Profile struct {
	Email          string `json:"email"`
	Organization   string `json:"organization"`
	ProfileWarning string `json:"profileWarning,omitempty"`
	Name           string `json:"name"`
	Subject        string `json:"subject"`
	Tenant         string `json:"tenant"`
	Issuer         string `json:"issuer"`
	Principal      string `json:"principal"`
	Expires        string `json:"expires"`
}
type Session struct {
	Store                                 CredentialStore
	mu                                    sync.Mutex
	Client                                HTTPDoer
	meta                                  metadata
	key                                   crypto.Signer
	privatePEM, state, verifier, redirect string
	started                               time.Time
	certificate                           config.CertificateInfo
	tokens                                token
	profile                               Profile
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func validIssuer(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "auth.dev.wendy.sh" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.Path, "/realms/") && len(strings.Split(u.Path, "/")) == 3 && len(strings.TrimPrefix(u.Path, "/realms/")) > 0
}
func (s *Session) request(ctx context.Context, method, target, body string, headers map[string]string) ([]byte, *http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if e != nil {
		return nil, nil, e
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := s.Client.Do(req)
	if e != nil {
		return nil, nil, e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil {
		return nil, resp, e
	}
	if len(data) > 1<<20 {
		return nil, resp, errors.New("Authentication response too large")
	}
	return data, resp, nil
}
func (s *Session) discover(ctx context.Context, issuer string) (metadata, error) {
	var m metadata
	if !validIssuer(issuer) {
		return m, errors.New("Untrusted authentication issuer")
	}
	b, r, e := s.request(ctx, "GET", issuer+"/.well-known/openid-configuration", "", nil)
	if e != nil {
		return m, e
	}
	if r.StatusCode != 200 {
		return m, fmt.Errorf("Auth discovery returned %d", r.StatusCode)
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return m, e
	}
	if m.Issuer != issuer || m.Authorize != issuer+"/authorize" || m.Token != issuer+"/oauth2/token" || m.JWKS != issuer+"/.well-known/jwks.json" {
		return m, errors.New("Unexpected authentication endpoints")
	}
	return m, nil
}
func (s *Session) Begin(ctx context.Context, email, redirect string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, e := url.Parse(redirect)
	if e != nil || u.User != nil || u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && u.Host == "localhost:5173")) {
		return "", errors.New("Invalid browser callback")
	}
	issuer := AuthBase + "/realms/system"
	if email != "" {
		body, _ := json.Marshal(map[string]string{"email": email})
		b, r, e := s.request(ctx, "POST", AuthBase+"/api/login/realm", string(body), map[string]string{"Content-Type": "application/json"})
		if e != nil {
			return "", e
		}
		if r.StatusCode != 200 {
			return "", fmt.Errorf("Email lookup returned %d", r.StatusCode)
		}
		var out struct {
			LoginURL string `json:"loginURL"`
		}
		if e = json.Unmarshal(b, &out); e != nil {
			return "", e
		}
		issuer, e = issuerFromRealmLoginURL(out.LoginURL)
		if e != nil {
			return "", e
		}
	}
	m, e := s.discover(ctx, issuer)
	if e != nil {
		return "", e
	}
	private, e := certs.GenerateMLDSAKeyPair()
	if e != nil {
		return "", e
	}
	key, e := certs.ParseSigningPrivateKeyPEM([]byte(private))
	if e != nil {
		return "", e
	}
	s.meta = m
	s.key = key
	s.privatePEM = private
	s.state = random()
	s.verifier = random()
	s.redirect = redirect
	s.started = time.Now()
	s.tokens = token{}
	s.profile = Profile{}
	s.certificate = config.CertificateInfo{}
	challenge := sha256.Sum256([]byte(s.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {redirect}, "scope": {"openid email profile groups"}, "resource": {IdentityResource}, "state": {s.state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	return m.Authorize + "?" + strings.ReplaceAll(q.Encode(), "+", "%20"), nil
}
func (s *Session) exchange(ctx context.Context, form url.Values) (token, error) {
	form.Set("client_id", ClientID)
	nonce := ""
	for attempt := 0; attempt < 2; attempt++ {
		proof, e := cloudrequest.NewDPoPProof(s.key, "POST", s.meta.Token, nonce)
		if e != nil {
			return token{}, e
		}
		b, r, e := s.request(ctx, "POST", s.meta.Token, form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "DPoP": proof})
		if e != nil {
			return token{}, e
		}
		if r.StatusCode != 200 {
			var problem struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(b, &problem)
			if attempt == 0 && problem.Error == "use_dpop_nonce" && r.Header.Get("DPoP-Nonce") != "" {
				nonce = r.Header.Get("DPoP-Nonce")
				continue
			}
			return token{}, fmt.Errorf("Token exchange failed (%d): %s", r.StatusCode, problem.Error)
		}
		var t token
		if e = json.Unmarshal(b, &t); e != nil {
			return t, e
		}
		if t.Access == "" || t.Refresh == "" || !strings.EqualFold(t.Type, "DPoP") {
			return t, errors.New("Auth server did not return a DPoP session and refresh token")
		}
		return t, nil
	}
	return token{}, errors.New("DPoP nonce retry failed")
}
func (s *Session) Complete(ctx context.Context, code, state, issuer string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(s.state)) != 1 {
		return Profile{}, errors.New("Login state mismatch; start sign-in again")
	}
	if time.Since(s.started) > 10*time.Minute {
		s.state = ""
		return Profile{}, errors.New("Login expired; start sign-in again")
	}
	s.state = "" // Consume before network access, including failed code exchanges.
	if code == "" {
		return Profile{}, errors.New("No authorization code received")
	}
	m, e := s.discover(ctx, issuer)
	if e != nil {
		return Profile{}, e
	}
	s.meta = m
	t, e := s.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {s.verifier}, "redirect_uri": {s.redirect}, "resource": {IdentityResource}})
	if e != nil {
		return Profile{}, e
	}
	claims, e := s.verifyAccess(ctx, t.Access, IdentityResource)
	if e != nil {
		return Profile{}, e
	}
	subject, _ := claims["sub"].(string)
	tenant, _ := claims["tenant_uuid"].(string)
	if subject == "" || !canonicalUUID(tenant) {
		return Profile{}, errors.New("Token is missing its operator or tenant identity")
	}
	certificate, e := requestPKIIdentityCertificate(ctx, s.Client, IdentityEndpoint, s.privatePEM, s.key, t.Access, tenant, subject)
	if e != nil {
		return Profile{}, e
	}
	cloud, e := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.Refresh}, "resource": {CloudResource}})
	if e != nil {
		return Profile{}, e
	}
	cloudClaims, e := s.verifyAccess(ctx, cloud.Access, CloudResource)
	if e != nil {
		return Profile{}, e
	}
	if cloudClaims["sub"] != subject || cloudClaims["tenant_uuid"] != tenant {
		return Profile{}, errors.New("Cloud token identity changed")
	}
	name, _ := claims["email"].(string)
	if name == "" {
		name = "Signed in"
	}
	s.certificate = certificate
	s.tokens = cloud
	email, _ := claims["email"].(string)
	s.profile = Profile{Email: email, Name: name, Subject: subject, Tenant: tenant, Issuer: issuer, Principal: certificate.PrincipalURI, Expires: time.Now().Add(time.Duration(cloud.Expires) * time.Second).UTC().Format(time.RFC3339)}
	s.verifier = ""
	if err := s.loadUserInfo(ctx); err != nil {
		s.profile.ProfileWarning = "Email lookup unavailable: " + err.Error()
	}
	if err := s.saveLocked(); err != nil {
		s.profile.ProfileWarning = "Could not remember this sign-in: " + err.Error()
	}
	return s.profile, nil
}

// Verify both token signature and sender binding before displaying a signed-in identity.
func (s *Session) verifyAccess(ctx context.Context, raw, audience string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("Invalid access token")
	}
	decode := base64.RawURLEncoding.DecodeString
	h, e := decode(parts[0])
	if e != nil {
		return nil, e
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if e = json.Unmarshal(h, &header); e != nil {
		return nil, e
	}
	sig, e := decode(parts[2])
	if e != nil {
		return nil, e
	}
	body, r, e := s.request(ctx, "GET", s.meta.JWKS, "", nil)
	if e != nil {
		return nil, e
	}
	if r.StatusCode != 200 {
		return nil, errors.New("Could not load issuer signing keys")
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	if e = json.Unmarshal(body, &jwks); e != nil {
		return nil, e
	}
	verified := false
	message := []byte(parts[0] + "." + parts[1])
	for _, j := range jwks.Keys {
		if j["kid"] != header.Kid || j["alg"] != header.Alg {
			continue
		}
		switch header.Alg {
		case "ML-DSA-65":
			if j["kty"] != "AKP" {
				continue
			}
			pub, e := decode(j["pub"])
			if e != nil {
				continue
			}
			key, e := mldsa.NewPublicKey(mldsa.MLDSA65(), pub)
			if e == nil {
				verified = mldsa.Verify(key, message, sig, nil) == nil
			}
		case "ES256":
			if j["kty"] != "EC" || j["crv"] != "P-256" || len(sig) != 64 {
				continue
			}
			xb, e := decode(j["x"])
			if e != nil {
				continue
			}
			yb, e := decode(j["y"])
			if e != nil {
				continue
			}
			x, y := new(big.Int).SetBytes(xb), new(big.Int).SetBytes(yb)
			if !elliptic.P256().IsOnCurve(x, y) {
				continue
			}
			hash := sha256.Sum256(message)
			verified = ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
		if verified {
			break
		}
	}
	if !verified {
		return nil, errors.New("Access token signature verification failed")
	}
	payload, e := decode(parts[1])
	if e != nil {
		return nil, e
	}
	var claims map[string]any
	if e = json.Unmarshal(payload, &claims); e != nil {
		return nil, e
	}
	audOK := claims["aud"] == audience
	if a, ok := claims["aud"].([]any); ok {
		for _, v := range a {
			if v == audience {
				audOK = true
			}
		}
	}
	exp, _ := claims["exp"].(float64)
	if claims["iss"] != s.meta.Issuer || !audOK || exp <= float64(time.Now().Unix()) {
		return nil, errors.New("Access token issuer, audience or expiry is invalid")
	}
	thumb, e := cloudrequest.OperatorJWKThumbprint(s.key)
	if e != nil {
		return nil, e
	}
	cnf, _ := claims["cnf"].(map[string]any)
	if cnf["jkt"] != thumb {
		return nil, errors.New("Access token belongs to a different operator key")
	}
	return claims, nil
}

func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func requestPKIIdentityCertificate(
	ctx context.Context,
	client HTTPDoer,
	endpoint, privateKeyPEM string,
	key crypto.Signer,
	accessToken, tenantUUID, subject string,
) (config.CertificateInfo, error) {
	if accessToken == "" {
		return config.CertificateInfo{}, fmt.Errorf("requesting pki-core identity certificate: access token is empty")
	}
	htu, err := cloudrequest.CanonicalHTU(endpoint)
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
	proof, err := cloudrequest.NewDPoPAccessProof(key, http.MethodPost, htu, accessToken)
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

// Discover uses the CLI's v2 API and its per-RPC DPoP proof. The supplied dialer
// must carry HTTP/2 to the fixed Cloud authority over an authenticated TLS relay.
func (s *Session) Discover(ctx context.Context, dial func(context.Context, string) (net.Conn, error)) ([]*cloudpbv2.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens.Access == "" {
		return nil, errors.New("Sign in with Wendy first")
	}
	if err := s.refreshCloudToken(ctx); err != nil {
		return nil, err
	}
	auth := &config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443", OAuthIssuer: s.meta.Issuer, DPoPPrivateKey: s.privatePEM, Certificates: []config.CertificateInfo{s.certificate}}
	options := []grpc.DialOption{clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) { return dial(ctx, "api.dev.wendy.sh:443") }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	options = append(options, cloudrequest.DPoPDialOptions(auth, func(context.Context) (string, crypto.Signer, error) { return s.tokens.Access, s.key, nil })...)
	conn, e := grpc.NewClient("passthrough:///api.dev.wendy.sh:443", options...)
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	ctx = grpcmetadata.NewOutgoingContext(ctx, grpcmetadata.Pairs("x-wendy-client-cert", "URI="+s.profile.Principal, "x-forwarded-client-cert", "URI="+s.profile.Principal))
	if s.profile.Email == "" {
		if err := s.loadUserInfo(ctx); err != nil {
			s.profile.ProfileWarning = "Email lookup unavailable: " + err.Error()
		}
	}
	if s.profile.Organization == "" {
		orgCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		org, err := cloudpbv2.NewOrganizationServiceClient(conn).GetOrganization(orgCtx, &cloudpbv2.GetOrganizationRequest{Id: s.profile.Tenant})
		cancel()
		if err != nil {
			s.profile.ProfileWarning = "Organization lookup unavailable: " + err.Error()
		} else if org.GetId() != s.profile.Tenant {
			s.profile.ProfileWarning = "Organization lookup returned a different tenant"
		} else {
			s.profile.Organization = org.GetName()
			if s.profile.Email != "" {
				s.profile.ProfileWarning = ""
			}
		}
	}
	if err := s.saveLocked(); err != nil {
		s.profile.ProfileWarning = "Could not save this sign-in: " + err.Error()
	}
	client := cloudpbv2.NewAssetServiceClient(conn)
	assets := make([]*cloudpbv2.Asset, 0)
	compute := true
	onlineOnly := true
	limit := int32(200)
	for offset := int32(0); ; {
		stream, e := client.ListAssets(ctx, &cloudpbv2.ListAssetsRequest{OrganizationId: s.profile.Tenant, IsComputeDevice: &compute, OnlineOnly: &onlineOnly, Offset: &offset, Limit: &limit})
		if e != nil {
			return nil, e
		}
		count, total := int32(0), int32(0)
		for {
			item, e := stream.Recv()
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			if item.Asset == nil {
				return nil, errors.New("Cloud returned an empty device record")
			}
			if len(assets) >= 10000 {
				return nil, errors.New("Cloud device limit exceeded")
			}
			assets = append(assets, item.Asset)
			count++
			total = item.Total
		}
		offset += count
		if count == 0 || offset >= total {
			return assets, nil
		}
	}
}

// Realm discovery returns a root-relative loginURL. Resolve it only against
// the configured auth host, while also accepting its absolute equivalent.
func issuerFromRealmLoginURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return "", errors.New("Untrusted realm login URL")
	}
	if u.Scheme == "" && u.Host == "" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		base, _ := url.Parse(AuthBase)
		u = base.ResolveReference(u)
	}
	if u.Scheme != "https" || u.Host != "auth.dev.wendy.sh" {
		return "", errors.New("Untrusted realm login URL")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 3 || parts[1] != "realms" || parts[2] == "" {
		return "", errors.New("Realm lookup did not return an issuer")
	}
	for _, c := range parts[2] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", errors.New("Realm lookup returned an invalid realm")
		}
	}
	return AuthBase + "/realms/" + parts[2], nil
}

// UserInfo carries scope-gated email claims that access tokens may omit.
// Bind both the request and response to the authenticated operator.
func (s *Session) loadUserInfo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := s.meta.Issuer + "/userinfo"
	proof, err := cloudrequest.NewDPoPAccessProof(s.key, "GET", endpoint, s.tokens.Access)
	if err != nil {
		return err
	}
	body, resp, err := s.request(ctx, "GET", endpoint, "", map[string]string{"Authorization": "DPoP " + s.tokens.Access, "DPoP": proof})
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("UserInfo returned %d", resp.StatusCode)
	}
	var info struct {
		Subject string `json:"sub"`
		Email   string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return err
	}
	if info.Subject != s.profile.Subject {
		return errors.New("UserInfo subject does not match the signed-in operator")
	}
	if info.Email == "" {
		return errors.New("UserInfo did not include an email")
	}
	s.profile.Email = info.Email
	s.profile.Name = info.Email
	return nil
}
func (s *Session) Profile() Profile { s.mu.Lock(); defer s.mu.Unlock(); return s.profile }
