package trustedkeys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newSPKI(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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

// A file that is present but unusable never loads as Open.
func TestOpenRefusesUnusableFile(t *testing.T) {
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
		if _, err := Open(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
