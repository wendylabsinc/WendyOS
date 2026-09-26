package browserauth

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// CredentialStore is implemented by the browser worker. Credentials are never
// returned through the UI's request/reply channel.
type CredentialStore interface {
	Load() ([]byte, error)
	Save([]byte) error
	Clear() error
}
type savedSession struct {
	Version     int                    `json:"version"`
	Issuer      string                 `json:"issuer"`
	PrivateKey  string                 `json:"privateKey"`
	Certificate config.CertificateInfo `json:"certificate"`
	Tokens      token                  `json:"tokens"`
	Profile     Profile                `json:"profile"`
}

func (s *Session) saveLocked() error {
	if s.Store == nil {
		return nil
	}
	raw, err := json.Marshal(savedSession{1, s.meta.Issuer, s.privatePEM, s.certificate, s.tokens, s.profile})
	if err != nil {
		return err
	}
	return s.Store.Save(raw)
}
func (s *Session) refreshCloudToken(ctx context.Context) error {
	expiry, _ := time.Parse(time.RFC3339, s.profile.Expires)
	if time.Until(expiry) >= time.Minute {
		return nil
	}
	refreshed, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.tokens.Refresh}, "resource": {CloudResource}})
	if err != nil {
		return err
	}
	claims, err := s.verifyAccess(ctx, refreshed.Access, CloudResource)
	if err != nil {
		return err
	}
	if claims["sub"] != s.profile.Subject || claims["tenant_uuid"] != s.profile.Tenant {
		return errors.New("Refreshed session identity changed")
	}
	s.tokens = refreshed
	s.profile.Expires = time.Now().Add(time.Duration(refreshed.Expires) * time.Second).UTC().Format(time.RFC3339)
	return s.saveLocked()
}
func (s *Session) Restore(ctx context.Context) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Store == nil {
		return nil, nil
	}
	raw, err := s.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("Could not read saved sign-in: %w", err)
	}
	if len(raw) == 0 {
		s.clearLocked()
		return nil, nil
	}
	var saved savedSession
	if len(raw) > 1<<20 || json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || !validIssuer(saved.Issuer) || saved.Tokens.Refresh == "" || saved.Tokens.Access == "" || !canonicalUUID(saved.Profile.Tenant) || saved.Profile.Subject == "" {
		return nil, errors.New("Saved sign-in is invalid. Sign in again")
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(saved.PrivateKey))
	if err != nil {
		return nil, errors.New("Saved operator key is invalid. Sign in again")
	}
	_, _, leaf, err := splitCertificateChainPEM([]byte(saved.Certificate.PemCertificate))
	if err != nil {
		return nil, errors.New("Saved certificate is invalid. Sign in again")
	}
	pub, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	got, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	expected := "spiffe://wendy.sh/tenant/" + saved.Profile.Tenant + "/operator/" + saved.Profile.Subject
	matches := 0
	for _, uri := range leaf.URIs {
		if uri.String() == expected {
			matches++
		}
	}
	if err != nil || !bytes.Equal(pub, got) || matches != 1 || saved.Certificate.PrincipalURI != expected || saved.Certificate.PemPrivateKey != saved.PrivateKey {
		return nil, errors.New("Saved certificate does not match the operator. Sign in again")
	}
	if !time.Now().Before(leaf.NotAfter) || time.Now().Before(leaf.NotBefore) {
		return nil, errors.New("Saved certificate has expired or is not yet valid. Sign in again")
	}
	meta, err := s.discover(ctx, saved.Issuer)
	if err != nil {
		return nil, fmt.Errorf("Could not restore sign-in. Your saved credentials are still stored: %w", err)
	}
	// Validate into a temporary session; a failed restore cannot replace an
	// authenticated in-memory identity. Store is shared to retain token rotation.
	restored := &Session{Client: s.Client, Store: s.Store, meta: meta, key: key, privatePEM: saved.PrivateKey, certificate: saved.Certificate, tokens: saved.Tokens, profile: saved.Profile}
	if err = restored.refreshCloudToken(ctx); err != nil {
		return nil, fmt.Errorf("Could not restore sign-in: %w", err)
	}
	claims, err := restored.verifyAccess(ctx, restored.tokens.Access, CloudResource)
	if err != nil {
		return nil, err
	}
	if claims["sub"] != saved.Profile.Subject || claims["tenant_uuid"] != saved.Profile.Tenant {
		return nil, errors.New("Saved sign-in identity does not match its token")
	}
	s.meta = restored.meta
	s.key = key
	s.privatePEM = saved.PrivateKey
	s.certificate = saved.Certificate
	s.tokens = restored.tokens
	s.profile = restored.profile
	p := s.profile
	return &p, nil
}
func (s *Session) clearLocked() {
	s.key = nil
	s.privatePEM = ""
	s.tokens = token{}
	s.certificate = config.CertificateInfo{}
	s.profile = Profile{}
	s.state = ""
	s.verifier = ""
	s.redirect = ""
	s.meta = metadata{}
}
func (s *Session) SignOut() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Store != nil {
		if err := s.Store.Clear(); err != nil {
			return fmt.Errorf("Could not remove saved sign-in: %w", err)
		}
	}
	s.clearLocked()
	return nil
}
