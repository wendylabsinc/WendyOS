package world

import (
	"errors"
	"time"
)

// ClampExpiry applies an unsigned absolute transfer cap to an exact retained
// key/version. It can only shorten lifetime; it neither changes component
// identity nor grants authority. A different retained version is unaffected.
// This relies on the configured clock domain, just like checkpoint recovery.
func (r *Replica) ClampExpiry(key Key, version Version, cap time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cap.IsZero() {
		return errors.New("expiry cap required")
	}
	now, err := r.now()
	if err != nil {
		return err
	}
	entry, ok := r.entries[key]
	if !ok {
		return errors.New("expiry cap has no retained key")
	}
	if entry.component.Version != version {
		return nil
	}
	// Restore this process's monotonic clock relation without treating transit
	// or proof-persistence delay as additional lifetime.
	deadline := now.Add(cap.Round(0).Sub(now.Round(0)))
	if deadline.Before(entry.expires) {
		entry.expires = deadline
		r.entries[key] = entry
	}
	return nil
}
