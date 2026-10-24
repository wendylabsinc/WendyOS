// Package devicepin persists and verifies SPKI fingerprints for known Wendy
// devices, providing TOFU (trust-on-first-use) protection against MITM.
package devicepin

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy-go/devices"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

const pinFileName = "known_devices.json"

// storeLockTimeout bounds how long a flush waits for another process's flush.
// Flushes run inside TLS handshakes, so this stays short; a timeout is a
// non-blocking persistence error like any other failed write.
const storeLockTimeout = 2 * time.Second

// PinnedDevice records the last-seen SPKI fingerprint for a device identity.
type PinnedDevice = devices.PinnedDevice

// PinMismatchError retains Core's blocking peer rejection type.
type PinMismatchError = devices.PinMismatchError

var _ certs.BlockingPinError = (*PinMismatchError)(nil)

// Store is a file-backed map from device identity key to PinnedDevice.
//
// Several processes may share the file: flush re-reads it under a lock and
// overlays only the keys this Store changed, so each process's pins and
// removals survive the others'. A Store itself is not safe for concurrent use
// by multiple goroutines.
type Store struct {
	path    string
	devices map[string]PinnedDevice
	// dirty holds the keys this Store set or removed since it last flushed.
	dirty map[string]bool
}

// Open loads the pin store from dir/known_devices.json, creating it if absent.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, pinFileName)
	devices, _, err := readPinFile(path)
	if err != nil {
		return nil, err
	}
	// A corrupt file opens as empty (readPinFile's nil map): start fresh
	// rather than block all connections.
	if devices == nil {
		devices = make(map[string]PinnedDevice)
	}
	return &Store{path: path, devices: devices, dirty: make(map[string]bool)}, nil
}

// readPinFile returns the pins stored at path. A missing file is empty
// (parsed, no pins). A file that does not parse — torn by a writer that
// predates atomic writes, or truncated — returns a nil map and parsed false,
// so each caller chooses what stands in for it: Open starts fresh, and flush
// keeps this Store's own view rather than every pin being dropped.
func readPinFile(path string) (devices map[string]PinnedDevice, parsed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]PinnedDevice), true, nil
		}
		return nil, false, fmt.Errorf("reading pin store: %w", err)
	}
	if err := json.Unmarshal(data, &devices); err != nil || devices == nil {
		return nil, false, nil
	}
	return devices, true, nil
}

// CheckAndUpdate checks the stored pin for the device identified by leaf's
// Wendy identity, creating or updating it as needed.
//
//   - Not an asset cert: skip (user certs and certs with no identity are not pinned)
//   - Not previously pinned: store pin, return nil
//   - Pinned, SPKI match: refresh LastSeen in memory, return nil
//   - Pinned, SPKI differs, pinned cert still valid: hard fail with PinMismatchError
//   - Pinned, SPKI differs, pinned cert expired (or predates NotAfter tracking):
//     rotation by definition — update pin, return nil
//
// Only the PinMismatchError is a rejection of the device. A returned write
// failure means the verdict above stands but could not be recorded; it is
// reported so a caller may log it, and deliberately does NOT implement
// certs.BlockingPinError, so the TLS verifier lets the connection through. The
// alternative — every mTLS connection failing because ~/.wendy is read-only —
// is an outage with no security question behind it.
//
// The write is skipped entirely when the entry would come back identical apart
// from LastSeen, which is the common path: a device whose key and expiry are
// unchanged is every connection after the first. Nothing reads LastSeen (it is
// there for a human reading known_devices.json), so it is refreshed in memory
// and rides along on the next write that has a real reason to happen. NotAfter
// is a different matter — CheckAndUpdate itself reads it to tell rotation from
// mismatch — so any change to it still forces the write through.
func (s *Store) CheckAndUpdate(leaf *x509.Certificate, displayName string) error {
	identity, ok, err := certs.IdentityFromCert(leaf)
	if err != nil || !ok || identity.EntityType != certs.EntityAsset {
		return nil
	}

	key := identity.IdentityKey()
	// A device that moves onto a pki-core chain keeps its pin. Its leaf is filed
	// under the SPIFFE principal now, but the entry recorded before the cutover
	// is filed under the urn:wendy URN — and a transitional leaf carries both, so
	// this is the one moment the two names are provably the same device. Carry
	// the entry over under the new name instead of silently starting a fresh
	// TOFU, which is what an attacker substituting the device would look like.
	renamed := false
	if legacy := identity.LegacyURN(); key != legacy && legacy != "" {
		if _, pinnedNow := s.devices[key]; !pinnedNow {
			if prior, hadLegacy := s.devices[legacy]; hadLegacy {
				s.devices[key] = prior
				delete(s.devices, legacy)
				s.dirty[legacy] = true
				renamed = true
			}
		}
	}
	var prior *PinnedDevice
	if existing, ok := s.devices[key]; ok {
		prior = &existing
	}
	next, persist, err := devices.ComparePin(key, displayName, leaf.RawSubjectPublicKeyInfo, leaf.NotAfter, prior, time.Now())
	if err != nil {
		return err
	}
	s.devices[key] = next
	if !persist && !renamed {
		return nil
	}
	s.dirty[key] = true
	return s.flush()
}

// Has reports whether a pin is recorded for an identity key. Remove treats an
// absent key as success, which is right for the caller's contract but leaves it
// unable to say what it actually removed — and an unpin that reports clearing
// entries it never held is exactly the kind of vague reporting that let
// over-broad clearing go unnoticed.
func (s *Store) Has(key string) bool {
	_, ok := s.devices[key]
	return ok
}

// Remove drops the pin for an identity key, so the next connection is a first
// use. Removing an absent key is not an error.
func (s *Store) Remove(key string) error {
	if _, ok := s.devices[key]; !ok {
		return nil
	}
	delete(s.devices, key)
	s.dirty[key] = true
	return s.flush()
}

// flush merges this Store's changes into the file under a lock shared by every
// wendy process, then replaces the file atomically (0600). Keys this Store did
// not change keep whatever is on disk now, so another process's new pin or
// unpin is never reverted by a snapshot this Store loaded earlier; the merged
// result becomes this Store's view. LastSeen refreshes of unchanged entries
// are in-memory only and may be dropped here — nothing reads them. A file that
// does not parse contributes nothing to merge, so the merge then starts from
// this Store's own view instead (see readPinFile).
//
// Before writing, it probes the file for write permission (like
// config.writeConfigFile) and fails without touching it if that probe fails.
// A rename replaces a file regardless of that file's own permissions — only
// the containing directory's permissions matter — so without this probe a
// flush would silently override a known_devices.json the user deliberately
// made read-only.
func (s *Store) flush() error {
	release, err := flock.Acquire(s.path+".lock", storeLockTimeout)
	if err != nil {
		return fmt.Errorf("locking pin store: %w", err)
	}
	defer release()

	// A symlinked known_devices.json (a dotfiles repo) is written through to
	// its target, so the rename below does not replace the link.
	path, err := atomicfile.ResolveWritePath(s.path)
	if err != nil {
		return fmt.Errorf("writing pin store: %w", err)
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("writing pin store: %w", err)
		}
	} else {
		_ = f.Close()
	}

	merged, parsed, err := readPinFile(path)
	if err != nil {
		return err
	}
	if !parsed {
		// Nothing on disk can be merged, so this Store's view is the best
		// record left of everyone's pins — what flush wrote before it
		// merged. Starting from empty would drop every pin but this Store's
		// changes.
		merged = make(map[string]PinnedDevice, len(s.devices))
		for key, d := range s.devices {
			merged[key] = d
		}
	}
	for key := range s.dirty {
		if d, ok := s.devices[key]; ok {
			merged[key] = d
		} else {
			delete(merged, key)
		}
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling pin store: %w", err)
	}
	if err := atomicfile.WritePreservingOwner(path, data, 0o600); err != nil {
		return fmt.Errorf("writing pin store: %w", err)
	}
	s.devices = merged
	s.dirty = make(map[string]bool)
	return nil
}

func spkiFingerprint(cert *x509.Certificate) string {
	return devices.SPKIFingerprint(cert.RawSubjectPublicKeyInfo)
}
