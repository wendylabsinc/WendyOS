// Package devicepin records the latest certificate key for verified Wendy
// device identities. Callers authenticate the certificate chain and expected
// device identity before updating the store.
package devicepin

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
type PinnedDevice struct {
	SPKIFingerprint string `json:"spkiFingerprint"` // "sha256:<hex>"
	DisplayName     string `json:"displayName"`
	LastSeen        string `json:"lastSeen"`           // RFC3339
	NotAfter        string `json:"notAfter,omitempty"` // RFC3339; pinned leaf's expiry
}

// PinMismatchError represents a public-key pin rejection. It remains available
// to callers that report pin failures, but Store.CheckAndUpdate no longer emits
// it: a verified device identity can rotate keys before its certificate expires.
type PinMismatchError struct {
	Key         string
	DisplayName string
	Want        string
	Got         string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("device %q (%s) presented a different certificate key than pinned (pinned %s, now %s)",
		e.DisplayName, e.Key, e.Want, e.Got)
}

// BlockingPinRejection distinguishes a peer rejection from a local write error.
func (e *PinMismatchError) BlockingPinRejection() {}

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

// CheckAndUpdate records the latest certificate for leaf's Wendy asset identity.
// The caller must first verify the certificate chain and any expected device
// identity. A matching display name or hostname alone does not authenticate a
// device; those bindings belong to the caller's identity checks.
//
// Key changes for an authenticated identity are accepted even while the previous
// certificate is valid. Renewals normally overlap the previous validity window,
// and certificate rotation may also replace the key. The fingerprint and expiry
// remain diagnostic metadata, not an additional trust anchor.
//
// Non-asset certificates are skipped. Write errors do not implement
// certs.BlockingPinError and must not reject an otherwise verified connection.
// Unchanged entries refresh LastSeen in memory without writing the file; changes
// to the key, expiry, or display name are persisted.
func (s *Store) CheckAndUpdate(leaf *x509.Certificate, displayName string) error {
	identity, ok, err := certs.IdentityFromCert(leaf)
	if err != nil || !ok || identity.EntityType != certs.EntityAsset {
		return nil
	}

	key := identity.IdentityKey()
	// A verified transitional leaf carries both the legacy URN and the SPIFFE
	// principal. Move the record to the principal so an unpin still removes it.
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
	fingerprint := spkiFingerprint(leaf)
	notAfter := leaf.NotAfter.UTC().Format(time.RFC3339)

	existing, pinned := s.devices[key]

	unchanged := pinned && !renamed &&
		existing.SPKIFingerprint == fingerprint &&
		existing.NotAfter == notAfter &&
		existing.DisplayName == displayName

	s.devices[key] = PinnedDevice{
		SPKIFingerprint: fingerprint,
		DisplayName:     displayName,
		LastSeen:        time.Now().UTC().Format(time.RFC3339),
		NotAfter:        notAfter,
	}
	if unchanged {
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
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}
