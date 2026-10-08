package devicepin_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

func makeCert(t *testing.T, sanURI string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	if sanURI != "" {
		u, _ := url.Parse(sanURI)
		tmpl.URIs = []*url.URL{u}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func TestStore_FirstConnection_Pins(t *testing.T) {
	dir := t.TempDir()
	store, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cert := makeCert(t, "urn:wendy:org:7:asset:42")
	if err := store.CheckAndUpdate(cert, "My Device"); err != nil {
		t.Fatalf("CheckAndUpdate first: %v", err)
	}
	// Pin file must exist.
	if _, err := os.Stat(filepath.Join(dir, "known_devices.json")); err != nil {
		t.Errorf("known_devices.json not created: %v", err)
	}
}

func TestStore_SameCert_UpdatesLastSeen(t *testing.T) {
	dir := t.TempDir()
	store, _ := devicepin.Open(dir)
	cert := makeCert(t, "urn:wendy:org:7:asset:42")
	_ = store.CheckAndUpdate(cert, "My Device")
	// Second call with same cert must not error.
	if err := store.CheckAndUpdate(cert, "My Device"); err != nil {
		t.Errorf("CheckAndUpdate second (same cert): %v", err)
	}
}

// TestStore_DifferentCert_WithinValidityIsRejected supersedes the old
// "rotation accepted" / "warns on mismatch" behavior: SPKI pinning now fails
// closed. A key change while the previously pinned cert is still valid is
// unexplained (a renewal replaces an expiring cert, it does not race a live
// one), so CheckAndUpdate must reject it instead of overwriting the pin with
// a warning.
func TestStore_DifferentCert_WithinValidityIsRejected(t *testing.T) {
	dir := t.TempDir()
	store, _ := devicepin.Open(dir)
	cert1 := makeCert(t, "urn:wendy:org:7:asset:42")
	_ = store.CheckAndUpdate(cert1, "My Device")

	// Different cert, same identity key, pinned cert still valid → hard fail.
	cert2 := makeCert(t, "urn:wendy:org:7:asset:42")
	err := store.CheckAndUpdate(cert2, "My Device")

	var mismatch *devicepin.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want PinMismatchError, got %v", err)
	}
	if !strings.Contains(mismatch.DisplayName, "My Device") {
		t.Errorf("mismatch.DisplayName = %q, want to contain %q", mismatch.DisplayName, "My Device")
	}
}

func TestStore_NonAssetCert_Skipped(t *testing.T) {
	dir := t.TempDir()
	store, _ := devicepin.Open(dir)
	// User cert (entity type "user") is not pinned.
	cert := makeCert(t, "urn:wendy:org:7:user:99")
	if err := store.CheckAndUpdate(cert, "user"); err != nil {
		t.Errorf("CheckAndUpdate user cert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "known_devices.json")); err == nil {
		// File may or may not exist; what matters is no error and no panic.
		// Read it and verify the user identity key is not present.
		data, _ := os.ReadFile(filepath.Join(dir, "known_devices.json"))
		if len(data) > 2 { // more than "{}"
			t.Logf("known_devices.json: %s", data)
		}
	}
}

func TestStore_NoCert_Identity_Skipped(t *testing.T) {
	dir := t.TempDir()
	store, _ := devicepin.Open(dir)
	// Cert with no Wendy identity → skipped, no error.
	cert := makeCert(t, "")
	if err := store.CheckAndUpdate(cert, "legacy"); err != nil {
		t.Errorf("CheckAndUpdate no-identity cert: %v", err)
	}
}

func TestStore_PersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	cert := makeCert(t, "urn:wendy:org:7:asset:42")

	store1, _ := devicepin.Open(dir)
	_ = store1.CheckAndUpdate(cert, "My Device")

	// Re-open from same dir — pin must survive.
	store2, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	// Same cert → no error (SPKI match).
	if err := store2.CheckAndUpdate(cert, "My Device"); err != nil {
		t.Errorf("CheckAndUpdate after reload: %v", err)
	}
}

// unused import guard — pem is imported by the brief verbatim.
var _ = pem.EncodeToMemory

// assetCert generates a fresh self-signed asset cert with a URI SAN of
// urn:wendy:org:<org>:asset:<assetID> and the given NotAfter. Modeled on
// selfSignedCert in shared/certs/server_verify_test.go. A fresh key is
// generated on every call, so two certs with the same org/assetID still
// differ in SPKI — required for the mismatch test below to be meaningful.
func assetCert(t *testing.T, org int32, assetID string, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	u, err := url.Parse(fmt.Sprintf("urn:wendy:org:%d:asset:%s", org, assetID))
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tmpl.URIs = []*url.URL{u}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	return cert
}

func TestCheckAndUpdateKeyChange(t *testing.T) {
	t.Run("within validity is a hard fail", func(t *testing.T) {
		dir := t.TempDir()
		s, err := devicepin.Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		first := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
		if err := s.CheckAndUpdate(first, "thor"); err != nil {
			t.Fatalf("first use: %v", err)
		}

		second := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
		err = s.CheckAndUpdate(second, "thor")

		var mismatch *devicepin.PinMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("want PinMismatchError, got %v", err)
		}
	})

	t.Run("a key change is blocking, so the TLS verifier drops the connection", func(t *testing.T) {
		dir := t.TempDir()
		s, err := devicepin.Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		first := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
		if err := s.CheckAndUpdate(first, "thor"); err != nil {
			t.Fatalf("first use: %v", err)
		}

		second := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
		err = s.CheckAndUpdate(second, "thor")

		// certs.BuildServerVerifyConnection fails the handshake on exactly this
		// property and nothing else, so a PinMismatchError that stopped
		// satisfying it would silently become advisory — the MITM protection
		// gone with every existing test still green.
		var blocking certs.BlockingPinError
		if !errors.As(err, &blocking) {
			t.Fatalf("CheckAndUpdate error = %v (%T), want one the TLS verifier treats as blocking", err, err)
		}
	})

	t.Run("after the pinned cert expires it re-pins silently", func(t *testing.T) {
		dir := t.TempDir()
		s, err := devicepin.Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		expired := assetCert(t, 7, "42", time.Now().Add(-time.Hour))
		if err := s.CheckAndUpdate(expired, "thor"); err != nil {
			t.Fatalf("first use: %v", err)
		}

		renewed := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
		if err := s.CheckAndUpdate(renewed, "thor"); err != nil {
			t.Fatalf("rotation after expiry must be accepted, got %v", err)
		}
	})
}

// unwritablePinFile plants an existing, read-only known_devices.json in dir so
// the store can load it but never write it — the shape of a read-only config
// directory, without depending on the test process's ability to chmod a
// directory it owns.
func unwritablePinFile(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode bits do not prevent writes")
	}
	path := filepath.Join(dir, "known_devices.json")
	if err := os.WriteFile(path, []byte("{}"), 0o400); err != nil {
		t.Fatalf("seeding read-only pin file: %v", err)
	}
	// t.TempDir's cleanup needs the file removable again.
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

// TestCheckAndUpdate_PersistenceFailureIsNotBlocking is the regression this
// pairs with on the verifier side: an unwritable pin store must report its
// failure WITHOUT claiming the device was rejected. The verifier decides
// whether to drop the connection purely on certs.BlockingPinError, so if this
// error carried that marker, a read-only ~/.wendy would make every enrolled
// device unreachable.
func TestCheckAndUpdate_PersistenceFailureIsNotBlocking(t *testing.T) {
	dir := t.TempDir()
	unwritablePinFile(t, dir)

	s, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cert := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
	err = s.CheckAndUpdate(cert, "thor")
	if err == nil {
		t.Fatal("CheckAndUpdate = nil for an unwritable store; the failure must still be reported so a caller can log it")
	}

	var blocking certs.BlockingPinError
	if errors.As(err, &blocking) {
		t.Fatalf("CheckAndUpdate error = %v is marked blocking; a write failure is not a rejection of the device, and marking it one takes every device offline when ~/.wendy is read-only", err)
	}
	var mismatch *devicepin.PinMismatchError
	if errors.As(err, &mismatch) {
		t.Fatalf("CheckAndUpdate error = %v is a PinMismatchError; nothing about the peer's key changed", err)
	}
}

// TestCheckAndUpdate_UnchangedEntrySkipsTheWrite covers the common path: the
// same device, same key, same expiry, on every connection after the first.
// Rewriting the file each time is pure write amplification, and it invents a
// failure — a store that has nothing to record cannot fail to record it.
//
// Making the file unwritable after the pin is established is what makes the
// claim observable: a write would surface as the (non-blocking) persistence
// error, so nil proves none was attempted.
func TestCheckAndUpdate_UnchangedEntrySkipsTheWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cert := assetCert(t, 7, "42", time.Now().Add(24*time.Hour))
	if err := s.CheckAndUpdate(cert, "thor"); err != nil {
		t.Fatalf("first use: %v", err)
	}

	path := filepath.Join(dir, "known_devices.json")
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode bits do not prevent writes")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if err := s.CheckAndUpdate(cert, "thor"); err != nil {
		t.Fatalf("CheckAndUpdate = %v for an unchanged entry, want nil — nothing changed, so nothing should have been written", err)
	}

	// A genuinely changed entry must still be flushed. Only LastSeen is allowed
	// to go stale in the file; every other field is either read by
	// CheckAndUpdate itself (SPKIFingerprint, NotAfter) or shown to a human
	// (DisplayName), so "skip the write" must not become "skip every write".
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := s.CheckAndUpdate(cert, "thor-renamed"); err != nil {
		t.Fatalf("re-pin under a new display name: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading pin file: %v", err)
	}
	if !strings.Contains(string(data), "thor-renamed") {
		t.Fatalf("pin file %s did not pick up the changed display name; a changed entry must still be flushed", data)
	}
}

// Two CLIs open the store at once and each pins a different device. A flush
// that wrote its own snapshot would drop the other's pin (last writer wins).
func TestStore_FlushKeepsPinsWrittenByAnotherStore(t *testing.T) {
	dir := t.TempDir()
	a, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := b.CheckAndUpdate(assetCert(t, 7, "43", time.Now().Add(24*time.Hour)), "orin"); err != nil {
		t.Fatalf("b: %v", err)
	}
	fresh, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:43"} {
		if !fresh.Has(key) {
			t.Errorf("pin %s lost: a concurrent store's flush overwrote it", key)
		}
	}
}

// An unpin in one process must not be undone by another process that opened
// the store before the unpin and later pinned a different device.
func TestStore_RemoveSurvivesAnotherStoresFlush(t *testing.T) {
	dir := t.TempDir()
	seed, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
		t.Fatal(err)
	}
	stale, err := devicepin.Open(dir) // still holds asset 42 in memory
	if err != nil {
		t.Fatal(err)
	}
	unpin, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := unpin.Remove("urn:wendy:org:7:asset:42"); err != nil {
		t.Fatal(err)
	}
	if err := stale.CheckAndUpdate(assetCert(t, 7, "43", time.Now().Add(24*time.Hour)), "orin"); err != nil {
		t.Fatal(err)
	}
	fresh, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Has("urn:wendy:org:7:asset:42") {
		t.Error("a stale store's flush resurrected a pin another process removed")
	}
	if !fresh.Has("urn:wendy:org:7:asset:43") {
		t.Error("the stale store's own new pin was not written")
	}
}

func TestStore_FlushWritesAPrivateFileAndNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "known_devices.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("known_devices.json mode = %v, want 0600", got)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "known_devices.json" && e.Name() != "known_devices.json.lock" {
			t.Errorf("flush left %q behind", e.Name())
		}
	}
}

// TestStore_FlushDoesNotReplaceAReadOnlyPinFile is the devicepin analogue of
// config's read-only config.json protection (human decision F3/R2): a
// known_devices.json the user made read-only (e.g. chmod 400 to freeze it)
// must not be silently replaced via rename just because rename only cares
// about the containing directory's permissions. flush must probe the file
// and fail before writing anything.
func TestStore_FlushDoesNotReplaceAReadOnlyPinFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode bits do not prevent writes")
	}
	dir := t.TempDir()
	seed, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known_devices.json")
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.CheckAndUpdate(assetCert(t, 7, "43", time.Now().Add(24*time.Hour)), "orin")
	if err == nil {
		t.Fatal("CheckAndUpdate = nil pinning a new device against a read-only known_devices.json, want an error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("known_devices.json content changed:\nbefore: %s\nafter:  %s", before, after)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Errorf("known_devices.json mode changed: before %v, after %v", beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
	}
}

// R26: a flush that finds known_devices.json unparseable — torn by a
// non-atomic writer (an older wendy still running), or truncated — must not
// start the merge from an empty map. This Store's own view is the best record
// left of every other pin, exactly what the pre-merge flush wrote.
func TestStore_FlushOverACorruptFileKeepsThisStoresPins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"truncated", ""},
		{"torn", `{"urn:wendy:org:7:asset:42": {"spkiFingerpr`},
		{"null", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "known_devices.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.CheckAndUpdate(assetCert(t, 7, "43", time.Now().Add(24*time.Hour)), "orin"); err != nil {
				t.Fatal(err)
			}
			fresh, err := devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:43"} {
				if !fresh.Has(key) {
					t.Errorf("pin %s lost when flushing over a %s known_devices.json", key, tc.name)
				}
			}
		})
	}
}

// Open still starts fresh over a corrupt file rather than blocking every
// connection on it.
func TestStore_OpenTreatsACorruptFileAsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "known_devices.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := devicepin.Open(dir)
	if err != nil {
		t.Fatalf("Open over a corrupt pin file: %v", err)
	}
	if s.Has("urn:wendy:org:7:asset:42") {
		t.Fatal("a corrupt pin file produced a pin")
	}
	if err := s.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
		t.Fatalf("first pin over a corrupt file: %v", err)
	}
	fresh, err := devicepin.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Has("urn:wendy:org:7:asset:42") {
		t.Fatal("the pin written over a corrupt file was not persisted")
	}
}

// A known_devices.json kept behind a symlink (a dotfiles repo, like
// config.json — see config's TestSaveWritesThroughASymlinkedConfig) must be
// written through its link: renaming over the link itself would silently turn
// it into a regular file. That holds for a dangling link too, whose target
// flush creates, as os.WriteFile would. The lock stays beside the link.
func TestStore_FlushWritesThroughASymlinkedPinFile(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(fmt.Sprintf("dangling=%v", dangling), func(t *testing.T) {
			dir := t.TempDir()
			targetDir := t.TempDir()
			target := filepath.Join(targetDir, "dotfiles-known_devices.json")
			if !dangling {
				if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(dir, "known_devices.json")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable here: %v", err)
			}
			s, err := devicepin.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckAndUpdate(assetCert(t, 7, "42", time.Now().Add(24*time.Hour)), "thor"); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("flush replaced the known_devices.json symlink with a regular file")
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("symlink target: %v", err)
			}
			if !strings.Contains(string(data), "thor") {
				t.Fatalf("symlink target does not hold the new pin: %s", data)
			}
			if _, err := os.Stat(link + ".lock"); err != nil {
				t.Errorf("lock file not beside the link: %v", err)
			}
			for _, d := range []string{dir, targetDir} {
				entries, err := os.ReadDir(d)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					switch e.Name() {
					case "known_devices.json", "known_devices.json.lock", "dotfiles-known_devices.json":
					default:
						t.Errorf("flush left %q behind in %s", e.Name(), d)
					}
				}
			}
		})
	}
}
