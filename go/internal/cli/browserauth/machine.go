package browserauth

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// MachineSession authenticates a registered service-account key through Wendy
// Auth's RFC 7523 grant. It never borrows a human's access or refresh token.
// One instance belongs to one immutable tenant and service-account subject.
type MachineSession struct {
	mu                 sync.Mutex
	session            Session
	tenant, subject    string
	expires            time.Time
	certificateExpires time.Time
}

// ConnectDevice establishes operator mTLS inside an authorized Cloud relay.
// expectedTenant and expectedSubject must come from the live authorization
// decision. Checking them before any network call prevents credential mixups.
func (m *MachineSession) ConnectDevice(ctx context.Context, expectedTenant, expectedSubject, device string) (*grpcclient.AgentConnection, error) {
	if expectedTenant != m.tenant || expectedSubject != m.subject || !canonicalUUID(device) {
		return nil, fmt.Errorf("machine identity or device mismatch")
	}
	auth, err := m.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	dialer := &tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}}
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", address)
	}
	return connectAuthenticatedDevice(ctx, m.session.settings(), auth, auth.APIKey, m.session.key, m.tenant, device, dial, nil)
}

// NewMachineSession requires an ML-DSA-65 key already registered with the named
// service account. Registration and owner consent belong to the control plane.
// Settings and identity come from trusted deployment configuration, not tools.
func NewMachineSession(settings Settings, issuer, tenant, subject, privatePEM string, client HTTPDoer) (*MachineSession, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if !canonicalUUID(tenant) || subject == "" || strings.ContainsAny(subject, "/?#%") {
		return nil, fmt.Errorf("invalid machine tenant or subject")
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(privatePEM))
	if err != nil {
		return nil, fmt.Errorf("reading machine key: %w", err)
	}
	pk, ok := key.(*mldsa.PrivateKey)
	if !ok || len(pk.PublicKey().Bytes()) != 1952 {
		return nil, fmt.Errorf("machine identity requires an ML-DSA-65 key")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	m := &MachineSession{tenant: tenant, subject: subject, session: Session{Settings: &settings, Client: client, key: key, privatePEM: privatePEM}}
	if !m.session.validIssuer(issuer) {
		return nil, fmt.Errorf("untrusted machine issuer")
	}
	m.session.meta.Issuer = issuer
	return m, nil
}

// Credentials returns a copy of the current DPoP-bound Cloud token and PKI
// service certificate. Refresh errors fail the request rather than reusing an
// expired credential. Callers must not log or return this value to MCP clients.
func (m *MachineSession) Credentials(ctx context.Context) (*config.AuthConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.session
	now := time.Now()
	if s.meta.Token == "" {
		meta, err := s.discover(ctx, s.meta.Issuer)
		if err != nil {
			return nil, err
		}
		s.meta = meta
	}
	if !now.Add(time.Minute).Before(m.certificateExpires) {
		identity, _, err := m.exchange(ctx, s.settings().IdentityResource)
		if err != nil {
			return nil, err
		}
		certificate, err := requestPKIPrincipalCertificate(ctx, s.Client, s.settings().IdentityEndpoint, s.privatePEM, s.key, identity.Access, m.tenant, "service", m.subject)
		if err != nil {
			return nil, err
		}
		leaves, err := certs.ParseCertsFromPEM([]byte(certificate.PemCertificate))
		if err != nil || len(leaves) != 1 {
			return nil, fmt.Errorf("invalid machine certificate")
		}
		leaf := leaves[0]
		if now.Before(leaf.NotBefore) || !now.Add(time.Minute).Before(leaf.NotAfter) {
			return nil, fmt.Errorf("machine certificate is not currently usable")
		}
		s.certificate, m.certificateExpires = certificate, leaf.NotAfter
	}
	if !now.Add(time.Minute).Before(m.expires) {
		token, expiry, err := m.exchange(ctx, s.settings().CloudResource)
		if err != nil {
			return nil, err
		}
		s.tokens, m.expires = token, expiry
	}
	return &config.AuthConfig{
		CloudGRPC: s.settings().CloudGRPC, APIKey: s.tokens.Access,
		OAuthIssuer: s.meta.Issuer, OAuthResource: s.settings().CloudResource,
		PKIResource: s.settings().IdentityResource, PKIEndpoint: s.settings().IdentityEndpoint,
		OAuthExpiresAt: m.expires.UTC().Format(time.RFC3339), DPoPPrivateKey: s.privatePEM,
		Certificates: []config.CertificateInfo{s.certificate},
	}, nil
}

func (m *MachineSession) exchange(ctx context.Context, resource string) (token, time.Time, error) {
	s := &m.session
	assertion, err := m.assertion()
	if err != nil {
		return token{}, time.Time{}, err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}, "resource": {resource}}
	body, response, err := s.request(ctx, http.MethodPost, s.meta.Token, form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return token{}, time.Time{}, err
	}
	if response.StatusCode != http.StatusOK {
		return token{}, time.Time{}, fmt.Errorf("machine token exchange returned HTTP %d", response.StatusCode)
	}
	var result token
	if err := json.Unmarshal(body, &result); err != nil {
		return token{}, time.Time{}, fmt.Errorf("invalid machine token response")
	}
	if result.Access == "" || result.Type != "DPoP" || result.Expires <= 0 || result.Expires > 86400 {
		return token{}, time.Time{}, fmt.Errorf("machine token must be DPoP-bound with an expiry")
	}
	claims, err := s.verifyAccess(ctx, result.Access, resource)
	if err != nil {
		return token{}, time.Time{}, err
	}
	if claims["principal_kind"] != "service" || claims["tenant_uuid"] != m.tenant || claims["sub"] != m.subject {
		return token{}, time.Time{}, fmt.Errorf("machine token principal mismatch")
	}
	expiry := time.Unix(int64(claims["exp"].(float64)), 0)
	if advertised := time.Now().Add(time.Duration(result.Expires) * time.Second); advertised.Before(expiry) {
		expiry = advertised
	}
	return result, expiry, nil
}

func (m *MachineSession) assertion() (string, error) {
	kid, err := cloudrequest.OperatorJWKThumbprint(m.session.key)
	if err != nil {
		return "", err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	header, err := json.Marshal(map[string]string{"alg": "ML-DSA-65", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", err
	}
	now := time.Now()
	payload, err := json.Marshal(map[string]any{"iss": m.subject, "sub": m.subject, "aud": m.session.meta.Token, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": encode(nonce[:])})
	if err != nil {
		return "", err
	}
	input := encode(header) + "." + encode(payload)
	signature, err := m.session.key.Sign(rand.Reader, []byte(input), &mldsa.Options{})
	if err != nil {
		return "", err
	}
	return input + "." + encode(signature), nil
}
