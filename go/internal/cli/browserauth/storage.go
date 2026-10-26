package browserauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
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

// WrappedKeyStore is a CredentialStore that keeps the operator key out of the
// session record (WDY-3920). SaveWrapped stores the session without the key and
// wraps the key's ML-DSA seed under a key the store cannot export, with aad as
// the additional data; LoadKey unwraps it. The browser worker implements it, as
// the console does; Session writes version 1 records (key inline) only to
// stores without it.
type WrappedKeyStore interface {
	CredentialStore
	SaveWrapped(session, seed []byte, aad string) error
	LoadKey(aad string) ([]byte, error)
}
type savedSession struct {
	Version     int                    `json:"version"`
	Issuer      string                 `json:"issuer"`
	PrivateKey  string                 `json:"privateKey,omitempty"`
	Certificate config.CertificateInfo `json:"certificate"`
	Tokens      token                  `json:"tokens"`
	Profile     Profile                `json:"profile"`
}

func (s *Session) saveLocked() error {
	if s.Store == nil {
		return nil
	}
	saved := savedSession{1, s.meta.Issuer, s.privatePEM, s.certificate, s.tokens, s.profile}
	if wrapped, ok := s.Store.(WrappedKeyStore); ok {
		return saveWrapped(wrapped, saved, s.key)
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return s.Store.Save(raw)
}

// keyAAD binds a wrapped seed to its tenant and operator, as the console's
// record id does: a seed moved into another operator's slot does not unwrap.
func keyAAD(p Profile) string { return "operator-key:" + p.Tenant + ":" + p.Subject }

// saveWrapped writes a version 2 record: the session without the operator key,
// and the key's ML-DSA-65 seed for the store to wrap.
func saveWrapped(store WrappedKeyStore, saved savedSession, key crypto.Signer) error {
	sk, ok := key.(*mldsa.PrivateKey)
	if !ok || sk.PublicKey().Parameters() != mldsa.MLDSA65() {
		return errors.New("Operator key is not ML-DSA-65")
	}
	saved.Version, saved.PrivateKey, saved.Certificate.PemPrivateKey = 2, "", ""
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	seed := sk.Bytes()
	defer clear(seed)
	return store.SaveWrapped(raw, seed, keyAAD(saved.Profile))
}

// loadWrappedKey unwraps the operator's seed and returns the key as the
// PKCS#8 PEM the rest of the session uses.
func loadWrappedKey(store WrappedKeyStore, p Profile) (string, error) {
	seed, err := store.LoadKey(keyAAD(p))
	if err != nil {
		return "", err
	}
	defer clear(seed)
	sk, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), seed)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(sk)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}
func (s *Session) refreshCloudToken(ctx context.Context) error {
	expiry, _ := time.Parse(time.RFC3339, s.profile.Expires)
	if time.Until(expiry) >= time.Minute {
		return nil
	}
	return s.rotateCloudToken(ctx)
}

// rotateCloudToken exchanges the refresh token now, whatever the access token's
// remaining lifetime. The issuer rotates the refresh token on every exchange.
func (s *Session) rotateCloudToken(ctx context.Context) error {
	refreshed, err := s.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.tokens.Refresh}, "resource": {s.settings().CloudResource}})
	if err != nil {
		return err
	}
	claims, err := s.verifyAccess(ctx, refreshed.Access, s.settings().CloudResource)
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
	return s.restore(ctx, false)
}

// RestoreRotated restores like Restore but always exchanges the refresh token
// once first. If that exchange fails, the restore fails; the saved access token
// is never used instead. A stolen refresh token that was already used makes the
// issuer revoke the family, so this restore fails and the theft surfaces.
func (s *Session) RestoreRotated(ctx context.Context) (*Profile, error) {
	return s.restore(ctx, true)
}

func (s *Session) restore(ctx context.Context, rotate bool) (*Profile, error) {
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
	if len(raw) > 1<<20 || json.Unmarshal(raw, &saved) != nil || saved.Version < 1 || saved.Version > 2 || !s.validIssuer(saved.Issuer) || saved.Tokens.Refresh == "" || saved.Tokens.Access == "" || !canonicalUUID(saved.Profile.Tenant) || saved.Profile.Subject == "" {
		return nil, errors.New("Saved sign-in is invalid. Sign in again")
	}
	wrapped, canWrap := s.Store.(WrappedKeyStore)
	if saved.Version == 2 {
		if !canWrap || saved.PrivateKey != "" || saved.Certificate.PemPrivateKey != "" {
			return nil, errors.New("Saved sign-in is invalid. Sign in again")
		}
		if saved.PrivateKey, err = loadWrappedKey(wrapped, saved.Profile); err != nil {
			return nil, errors.New("Saved operator key is invalid. Sign in again")
		}
		saved.Certificate.PemPrivateKey = saved.PrivateKey
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
	if saved.Version == 1 && canWrap {
		// A record from before wrapped custody holds the key in plaintext.
		// Replacing it removes the plaintext, whatever the rest of the restore does.
		if err = saveWrapped(wrapped, saved, key); err != nil {
			return nil, fmt.Errorf("Could not protect the saved operator key: %w", err)
		}
		log.Print("Saved operator key moved into wrapped storage; the plaintext copy was removed")
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
	restored := &Session{Settings: s.Settings, Client: s.Client, Store: s.Store, RequireHostedMCPPolicy: s.RequireHostedMCPPolicy, meta: meta, key: key, privatePEM: saved.PrivateKey, certificate: saved.Certificate, tokens: saved.Tokens, profile: saved.Profile}
	refresh := restored.refreshCloudToken
	if rotate {
		refresh = restored.rotateCloudToken
	}
	if err = refresh(ctx); err != nil {
		return nil, fmt.Errorf("Could not restore sign-in: %w", err)
	}
	claims, err := restored.verifyAccess(ctx, restored.tokens.Access, s.settings().CloudResource)
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
