package browserauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
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
	p, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer)
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
func TestRestoreRotatedAlwaysExchangesAndFailsClosed(t *testing.T) {
	s, calls := setup(t, nil)
	store := &memoryStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	// The access token has an hour left: a plain restore must not exchange.
	before, oldRefresh := *calls, s.tokens.Refresh
	if _, err := (&Session{Client: s.Client, Store: store}).Restore(context.Background()); err != nil || *calls != before {
		t.Fatal("plain restore of a fresh token exchanged the refresh token", err)
	}
	next := &Session{Client: s.Client, Store: store}
	if _, err := next.RestoreRotated(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != before+1 || next.tokens.Refresh == oldRefresh {
		t.Fatal("rotated restore did not exchange the refresh token exactly once")
	}
	var persisted savedSession
	if err := json.Unmarshal(store.raw, &persisted); err != nil || persisted.Tokens.Refresh != next.tokens.Refresh {
		t.Fatal("rotated refresh token was not persisted")
	}
	// A refused exchange (a reused, revoked family) fails the restore; the
	// saved access token is not used instead.
	refuse := doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == testIssuer+"/oauth2/token" {
			return reply(400, map[string]string{"error": "invalid_grant"}), nil
		}
		return s.Client.Do(r)
	})
	failed := &Session{Client: refuse, Store: store}
	if p, err := failed.RestoreRotated(context.Background()); err == nil || p != nil || failed.tokens.Access != "" {
		t.Fatal("rotated restore fell through to the saved token after a refused exchange")
	}
}
func TestSavedSignInRejectsMismatchedCertificate(t *testing.T) {
	s, _ := setup(t, nil)
	store := &memoryStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
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

// wrappedStore stands in for the browser worker: it keeps the seed apart from
// the record and releases it only for the same aad (WebCrypto is tested in
// go/cmd/wendy-web).
type wrappedStore struct {
	memoryStore
	seed []byte
	aad  string
}

func (w *wrappedStore) SaveWrapped(session, seed []byte, aad string) error {
	w.raw, w.seed, w.aad = append([]byte(nil), session...), append([]byte(nil), seed...), aad
	return nil
}
func (w *wrappedStore) LoadKey(aad string) ([]byte, error) {
	if w.seed == nil || aad != w.aad {
		return nil, errors.New("does not unwrap")
	}
	return append([]byte(nil), w.seed...), nil
}

func TestWrappedSignInKeepsTheKeyOutOfTheRecord(t *testing.T) {
	s, _ := setup(t, nil)
	store := &wrappedStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store.raw, []byte("PRIVATE KEY")) || !bytes.Equal(store.seed, s.key.(*mldsa.PrivateKey).Bytes()) || store.aad != "operator-key:"+s.profile.Tenant+":"+s.profile.Subject {
		t.Fatalf("record holds the key or the seed was not wrapped: %s", store.raw)
	}
	next := &Session{Client: s.Client, Store: store}
	if _, err := next.Restore(context.Background()); err != nil || next.privatePEM != s.privatePEM || next.certificate.PemPrivateKey != s.privatePEM {
		t.Fatalf("restore lost the wrapped key: %v", err)
	}
	store.aad = "operator-key:" + s.profile.Tenant + ":another-operator"
	if _, err := (&Session{Client: s.Client, Store: store}).Restore(context.Background()); err == nil {
		t.Fatal("restored a seed wrapped for another operator")
	}
}

func TestPlaintextSignInMovesIntoWrappedStorage(t *testing.T) {
	s, _ := setup(t, nil)
	legacy := &memoryStore{}
	s.Store = legacy
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(legacy.raw, []byte("PRIVATE KEY")) {
		t.Fatal("fixture: a store without wrapping keeps the version 1 record")
	}
	store := &wrappedStore{memoryStore: memoryStore{raw: legacy.raw}}
	next := &Session{Client: s.Client, Store: store}
	if _, err := next.Restore(context.Background()); err != nil || next.privatePEM != s.privatePEM {
		t.Fatalf("migration lost the sign-in: %v", err)
	}
	var saved savedSession
	if err := json.Unmarshal(store.raw, &saved); err != nil || saved.Version != 2 || bytes.Contains(store.raw, []byte("PRIVATE KEY")) || store.seed == nil {
		t.Fatalf("plaintext key survived the migration: %s", store.raw)
	}
	// A wrapped record cannot be read back by a store that cannot unwrap it.
	if _, err := (&Session{Client: s.Client, Store: &memoryStore{raw: store.raw}}).Restore(context.Background()); err == nil {
		t.Fatal("restored a version 2 record without its wrapped key")
	}
}

// No downgrade: the store unwraps every seed as ML-DSA-65, so a key of any
// other kind is refused rather than wrapped.
func TestWrappedStorageRefusesAKeyThatIsNotMLDSA65(t *testing.T) {
	mldsa44, err := mldsa.GenerateKey(mldsa.MLDSA44())
	if err != nil {
		t.Fatal(err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]crypto.Signer{"ML-DSA-44": mldsa44, "P-256": p256} {
		store := &wrappedStore{}
		if err := saveWrapped(store, savedSession{Version: 1}, key); err == nil || store.seed != nil || store.raw != nil {
			t.Fatalf("%s key was wrapped", name)
		}
	}
}
