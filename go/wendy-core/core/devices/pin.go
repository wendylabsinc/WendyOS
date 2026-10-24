package devices

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// PinnedDevice is the CLI's persisted known-device entry. Storage and locking
// stay with the host; this shape preserves its existing JSON representation.
type PinnedDevice struct {
	SPKIFingerprint string `json:"spkiFingerprint"`
	DisplayName     string `json:"displayName"`
	LastSeen        string `json:"lastSeen"`
	NotAfter        string `json:"notAfter,omitempty"`
}

// PinMismatchError reports a new SPKI inside the existing pin's validity window.
type PinMismatchError struct{ Key, DisplayName, Want, Got string }

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("device %q (%s) presented a different certificate key than pinned (pinned %s, now %s)", e.DisplayName, e.Key, e.Want, e.Got)
}

// BlockingPinRejection distinguishes a peer rejection from a persistence error.
func (e *PinMismatchError) BlockingPinRejection() {}

// SPKIFingerprint hashes the DER SubjectPublicKeyInfo supplied by the host.
func SPKIFingerprint(spki []byte) string {
	sum := sha256.Sum256(spki)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ComparePin applies the CLI's TOFU/rotation policy to a verified asset peer.
// The host parses and verifies the certificate and supplies its identity key,
// SPKI and expiry. User/non-asset certificates must be excluded by the host.
// A differing key is rejected only while the recorded leaf is still valid.
// Missing or unparseable historical expiry retains the existing renewal rule.
// persist is false when only LastSeen changes; the host may refresh that value
// in memory and include it in the next meaningful write. It must keep the old
// pin on rejection and treat storage errors separately from peer rejections.
func ComparePin(key, displayName string, spki []byte, notAfter time.Time, previous *PinnedDevice, now time.Time) (next PinnedDevice, persist bool, err error) {
	fingerprint := SPKIFingerprint(spki)
	expiry := notAfter.UTC().Format(time.RFC3339)
	if previous != nil && previous.SPKIFingerprint != fingerprint && previous.NotAfter != "" {
		if exp, parseErr := time.Parse(time.RFC3339, previous.NotAfter); parseErr == nil && now.Before(exp) {
			return PinnedDevice{}, false, &PinMismatchError{Key: key, DisplayName: displayName, Want: previous.SPKIFingerprint, Got: fingerprint}
		}
	}
	persist = previous == nil || previous.SPKIFingerprint != fingerprint || previous.NotAfter != expiry || previous.DisplayName != displayName
	return PinnedDevice{fingerprint, displayName, now.UTC().Format(time.RFC3339), expiry}, persist, nil
}
