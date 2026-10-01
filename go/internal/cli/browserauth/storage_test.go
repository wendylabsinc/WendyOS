package browserauth

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type memoryStore struct{ raw []byte }

func (m *memoryStore) Load() ([]byte, error) { return append([]byte(nil), m.raw...), nil }
func (m *memoryStore) Save(raw []byte) error { m.raw = append([]byte(nil), raw...); return nil }
func (m *memoryStore) Clear() error          { m.raw = nil; return nil }
func TestSavedSignInRestoresSameCertificateAndRotatesTokens(t *testing.T) {
	s, calls := setup(t, nil)
	store := &memoryStore{}
	s.Store = store
	p, err := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.raw) == 0 {
		t.Fatal("sign-in was not saved")
	}
	oldKey, oldCert, oldRefresh := s.privatePEM, s.certificate.PemCertificate, s.tokens.Refresh
	// Simulate a new worker after a reload or browser restart, with an expired
	// access-token lifetime. The refresh family must survive and be rotated.
	s.profile.Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err = s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	next := &Session{Client: s.Client, Store: store}
	restored, err := next.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restored == nil || restored.Email != p.Email || next.privatePEM != oldKey || next.certificate.PemCertificate != oldCert {
		t.Fatal("session identity or certificate lost on restore")
	}
	if *calls != 4 || next.tokens.Refresh == oldRefresh {
		t.Fatal("expired session did not rotate refresh token")
	}
	var persisted savedSession
	if err = json.Unmarshal(store.raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Tokens.Refresh != next.tokens.Refresh {
		t.Fatal("rotated token was not persisted")
	}
	if err = next.SignOut(); err != nil {
		t.Fatal(err)
	}
	if len(store.raw) != 0 || next.privatePEM != "" || next.tokens.Access != "" {
		t.Fatal("sign-out retained credentials")
	}
	if p, err := next.Restore(context.Background()); err != nil || p != nil {
		t.Fatal("sign-out resurrected session")
	}
}
func TestSavedSignInRejectsMismatchedCertificate(t *testing.T) {
	s, _ := setup(t, nil)
	store := &memoryStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer); err != nil {
		t.Fatal(err)
	}
	var saved savedSession
	_ = json.Unmarshal(store.raw, &saved)
	saved.Profile.Subject = "another-operator"
	store.raw, _ = json.Marshal(saved)
	next := &Session{Client: s.Client, Store: store}
	if _, err := next.Restore(context.Background()); err == nil {
		t.Fatal("accepted certificate for different identity")
	}
	if len(store.raw) == 0 {
		t.Fatal("restore failure must not silently delete saved credentials")
	}
}
