package trustedkeys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSPKI(t *testing.T) []byte {
	t.Helper()
	k, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// Open -> Closed -> keys and keyless switch -> reload -> Open again.
func TestStoreShapeLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path)
	if err != nil || s.Closed() {
		t.Fatalf("missing file: closed=%v err=%v, want Open", s.Closed(), err)
	}
	now := time.Now()

	ownerKey, opKey := newSPKI(t), newSPKI(t)
	owner, err := s.Close(ownerKey, "laptop", now)
	if err != nil || !s.Closed() {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Close(newSPKI(t), "", now); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("second Close = %v, want ErrAlreadyClosed", err)
	}
	if role, _ := s.Lookup(ownerKey); role != RoleOwner {
		t.Fatalf("closing key role = %v, want owner", role)
	}

	op, err := s.Add(opKey, RoleOperator, "ci", owner.Fingerprint(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(newSPKI(t), RoleViewer, "", op.Fingerprint(), now); !errors.Is(err, ErrActorNotOwner) {
		t.Fatalf("operator added a key: %v", err)
	}
	if _, err := s.Add(opKey, RoleViewer, "", owner.Fingerprint(), now); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("one key got two roles: %v", err)
	}
	if err := s.SetKeylessApps(true, owner.Fingerprint()); err != nil || !s.KeylessApps() {
		t.Fatalf("SetKeylessApps: %v", err)
	}
	if _, err := s.Remove(owner.Fingerprint(), owner.Fingerprint()); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removed the last owner: %v", err)
	}

	// The file is the setting: a restart reads the same shape, keys and switch.
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi, err)
	}
	r, err := Open(path)
	if err != nil || !r.Closed() || !r.KeylessApps() || len(r.List()) != 2 {
		t.Fatalf("reload: closed=%v keyless=%v keys=%d err=%v", r.Closed(), r.KeylessApps(), len(r.List()), err)
	}
	if role, _ := r.Lookup(opKey); role != RoleOperator {
		t.Fatalf("reloaded operator role = %v", role)
	}

	removed, err := r.Remove(op.Fingerprint(), owner.Fingerprint())
	if err != nil || removed.Fingerprint() != op.Fingerprint() {
		t.Fatalf("Remove = %v, %v", removed.Fingerprint(), err)
	}
	if role, _ := r.Lookup(opKey); role != RoleNone {
		t.Fatal("removed key still trusted")
	}

	if err := r.Reopen(op.Fingerprint()); !errors.Is(err, ErrActorNotOwner) {
		t.Fatalf("non-owner reopened: %v", err)
	}
	if err := r.Reopen(owner.Fingerprint()); err != nil || r.Closed() {
		t.Fatalf("Reopen: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open device still has its file: %v", err)
	}
	if role, _ := r.Lookup(ownerKey); role != RoleNone {
		t.Fatal("Open device still trusts a key")
	}
}

// A file that is present but unusable never loads as Open: Open reports it
// and returns a Closed store that trusts no key, so the agent keeps running
// fail-closed instead of exiting into the supervisor's image-agent fallback.
func TestOpenUnusableFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, path := range map[string]string{
		"garbage":        write("garbage.json", "{", 0o600),
		"no owner":       write("noowner.json", `{"shape":"closed","keys":[]}`, 0o600),
		"unknown shape":  write("shape.json", `{"shape":"open","keys":[]}`, 0o600),
		"group writable": write("mode.json", `{"shape":"closed","keys":[]}`, 0o620),
	} {
		s, err := Open(path)
		if err == nil {
			t.Errorf("%s: loaded", name)
		}
		if !s.Closed() || s.KeylessApps() || len(s.List()) != 0 {
			t.Errorf("%s: closed=%v keyless=%v keys=%d, want Closed with no key", name, s.Closed(), s.KeylessApps(), len(s.List()))
		}
		if _, err := s.Close(newSPKI(t), "", time.Now()); !errors.Is(err, ErrAlreadyClosed) {
			t.Errorf("%s: Close = %v, want ErrAlreadyClosed", name, err)
		}
		if err := s.Reopen(""); err == nil {
			t.Errorf("%s: reopened without an owner", name)
		}
	}
}

// Only ML-DSA-65, the CLI's local-key algorithm, is accepted; every classical
// algorithm and the other ML-DSA sizes are refused, on add and on load.
func TestKeysMustBeMLDSA65(t *testing.T) {
	gen := map[string]func() (crypto.Signer, error){
		"RSA":       func() (crypto.Signer, error) { return rsa.GenerateKey(rand.Reader, 2048) },
		"P-224":     func() (crypto.Signer, error) { return ecdsa.GenerateKey(elliptic.P224(), rand.Reader) },
		"P-256":     func() (crypto.Signer, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) },
		"Ed25519":   func() (crypto.Signer, error) { _, k, err := ed25519.GenerateKey(rand.Reader); return k, err },
		"ML-DSA-44": func() (crypto.Signer, error) { return mldsa.GenerateKey(mldsa.MLDSA44()) },
		"ML-DSA-87": func() (crypto.Signer, error) { return mldsa.GenerateKey(mldsa.MLDSA87()) },
	}
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	for name, g := range gen {
		k, err := g()
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(k.Public())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePublicKey(der); err == nil {
			t.Errorf("%s key accepted", name)
		}
		if _, err := store.Close(der, "", time.Now()); err == nil {
			t.Errorf("%s key closed the device", name)
		}
	}
	if _, err := ParsePublicKey(newSPKI(t)); err != nil {
		t.Errorf("ML-DSA-65 key refused: %v", err)
	}
}

// Labels reach other owners' terminals: bounded, printable text only.
func TestLabelsAreBoundedPrintableText(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a\nfake row", "\x1b[2Jcleared", strings.Repeat("x", MaxLabelLen+1), "\xff"} {
		if _, err := s.Close(newSPKI(t), bad, time.Now()); !errors.Is(err, ErrBadLabel) {
			t.Errorf("label %q: %v, want ErrBadLabel", bad, err)
		}
	}
	if _, err := s.Close(newSPKI(t), "Sem's laptop – ünï", time.Now()); err != nil {
		t.Fatalf("printable label refused: %v", err)
	}
}
