// Package devicepin persists and verifies SPKI fingerprints for known Wendy
// devices, providing TOFU (trust-on-first-use) protection against MITM.
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

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const pinFileName = "known_devices.json"

// PinnedDevice records the last-seen SPKI fingerprint for a device identity.
type PinnedDevice struct {
	SPKIFingerprint string `json:"spkiFingerprint"` // "sha256:<hex>"
	DisplayName     string `json:"displayName"`
	LastSeen        string `json:"lastSeen"`           // RFC3339
	NotAfter        string `json:"notAfter,omitempty"` // RFC3339; pinned leaf's expiry
}

// PinMismatchError reports that a device identity presented a different public
// key than the one pinned for it, while the pinned certificate was still valid.
// A rotation that happens after the pinned cert expires is not an error — it is
// renewal — so this only fires inside the pinned cert's validity window, where
// a new key is unexplained.
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

// BlockingPinRejection marks this as the one CheckAndUpdate failure the TLS
// verifier must drop a connection over: it says the PEER is not the one we
// pinned. Every other way CheckAndUpdate can fail is a write to local
// bookkeeping, which cannot make a verified device untrustworthy. See
// certs.BlockingPinError.
func (e *PinMismatchError) BlockingPinRejection() {}

// Compile-time proof that the marker above really does satisfy the interface
// the verifier tests for. Without it, dropping or renaming the method would
// silently downgrade every key-change rejection to best-effort — the verifier
// would just stop failing, and nothing in this package would notice.
var _ certs.BlockingPinError = (*PinMismatchError)(nil)

// Store is a file-backed map from device identity key to PinnedDevice.
// It is not safe for concurrent use across multiple processes.
type Store struct {
	path    string
	devices map[string]PinnedDevice
}

// Open loads the pin store from dir/known_devices.json, creating it if absent.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, pinFileName)
	s := &Store{path: path, devices: make(map[string]PinnedDevice)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading pin store: %w", err)
	}
	if err := json.Unmarshal(data, &s.devices); err != nil {
		// Corrupt file: start fresh rather than block all connections.
		s.devices = make(map[string]PinnedDevice)
	}
	return s, nil
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
				renamed = true
			}
		}
	}
	fingerprint := spkiFingerprint(leaf)
	notAfter := leaf.NotAfter.UTC().Format(time.RFC3339)

	existing, pinned := s.devices[key]
	if pinned && existing.SPKIFingerprint != fingerprint {
		// A key change while the pinned cert is still valid is unexplained: a
		// renewal replaces an expiring cert, it does not race a live one.
		if existing.NotAfter != "" {
			if exp, parseErr := time.Parse(time.RFC3339, existing.NotAfter); parseErr == nil && time.Now().Before(exp) {
				return &PinMismatchError{Key: key, DisplayName: displayName, Want: existing.SPKIFingerprint, Got: fingerprint}
			}
		}
	}

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
	return s.flush()
}

func (s *Store) flush() error {
	data, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling pin store: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("writing pin store: %w", err)
	}
	return nil
}

func spkiFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}
