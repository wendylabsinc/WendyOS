package localmesh

import (
	"errors"
	"time"
)

const (
	clockRecoveryWindow    = 30 * time.Second
	maxClockPendingPerPeer = 2
)

func (s *Synchronizer) rememberClockPending(w SignedManifest) {
	key := w.ownerKey()
	if s.pendingClock == nil {
		s.pendingClock = make(map[string]time.Time)
	}
	if _, exists := s.pendingClock[key]; !exists && len(s.pendingClock) >= maxClockPendingPerPeer {
		s.discardManifest(w)
		return
	}
	if _, exists := s.pending[key]; !exists && len(s.pending) >= 128 {
		s.discardManifest(w)
		return
	}
	if old, exists := s.pending[key]; exists && !sameManifestWire(old, w) {
		s.discardManifest(old)
	}
	if _, exists := s.pendingClock[key]; !exists {
		s.pendingClock[key] = time.Now().Add(clockRecoveryWindow)
	}
	s.pending[key] = cloneSignedManifest(w)
	delete(s.pendingRenewals, key)
}

// ContinueClockPending retries through exactly the ordinary admission path
// after UTC catches up. The monotonic window is never extended by a duplicate
// or a wall-clock adjustment. At most two bounded bodies exist per neighbour.
func (s *Synchronizer) ContinueClockPending(now time.Time, slots int) (replies []ControlMessage, changed []SignedManifest, timeReady bool, err error) {
	for key, deadline := range s.pendingClock {
		w, ok := s.pending[key]
		if !ok {
			delete(s.pendingClock, key)
			continue
		}
		manifest, decodeErr := DecodeManifest(w.Body)
		if decodeErr != nil {
			s.dropPendingManifest(key)
			return nil, nil, false, decodeErr
		}
		if time.UnixMilli(manifest.Issued).After(now.Add(5 * time.Second)) {
			if !deadline.After(time.Now()) {
				s.dropPendingManifest(key)
			}
			continue
		}
		if slots-len(replies) < 1 {
			if !deadline.After(time.Now()) {
				s.dropPendingManifest(key)
			}
			break
		}
		delete(s.pendingClock, key)
		delete(s.pending, key)
		out, admitted, receiveErr := s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now, slots-len(replies))
		if errors.Is(receiveErr, ErrManifestClockSkew) {
			if deadline.After(time.Now()) {
				s.pendingClock[key] = deadline
			} else {
				s.dropPendingManifest(key)
			}
			continue
		}
		if receiveErr != nil {
			s.discardManifest(w)
			return nil, nil, false, receiveErr
		}
		// UTC's future gate has cleared. This does not assert admission:
		// normal receive still rejects stale/expired/certificate-invalid claims.
		timeReady = true
		replies = append(replies, out...)
		changed = append(changed, admitted...)
	}
	return replies, changed, timeReady, nil
}
