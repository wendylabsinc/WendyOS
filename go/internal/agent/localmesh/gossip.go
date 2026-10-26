package localmesh

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func (s *Synchronizer) offerManifest(w SignedManifest, now time.Time) []ControlMessage {
	if _, ok := s.directory.CurrentRecordChain(w, now); !ok {
		return nil
	}
	var manifest Manifest
	if json.Unmarshal(w.Body, &manifest) != nil {
		return nil
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	directory, principal := s.directory, w.Principal
	m := s.gossip.Offer(data, time.UnixMilli(manifest.Expires), now, func(at time.Time) []byte {
		return directory.manifestPayload(principal, at)
	})
	if m == nil {
		return nil
	}
	return []ControlMessage{{Kind: "gossip", Gossip: m}}
}
func (s *Synchronizer) resolveManifest(w SignedManifest, now time.Time, accepted bool) {
	var manifest Manifest
	_ = json.Unmarshal(w.Body, &manifest)
	data, _ := json.Marshal(w)
	s.gossip.Resolve(data, time.UnixMilli(manifest.Expires), now, accepted)
}

// Router links require HAVE/WANT; the legacy constructor is retained only for
// the separately negotiated endpoint-only leaf protocol and protocol fixtures.
func (s *Synchronizer) Receive(m ControlMessage, now time.Time, slots ...int) ([]ControlMessage, []SignedManifest, error) {
	if m.Kind != "gossip" {
		if s.gossip != nil && m.Kind == "manifest" {
			return nil, nil, errors.New("unsolicited router manifest")
		}
		return s.receive(m, now)
	}
	if s.gossip == nil || m.Gossip == nil {
		return nil, nil, errors.New("manifest gossip not negotiated")
	}
	if err := m.Gossip.Validate(); err != nil {
		return nil, nil, err
	}
	if m.Gossip.Type != netgossip.Manifest {
		return nil, nil, errors.New("gossip session type mismatch")
	}
	// A known digest need not download its body again to repair an expired
	// identity. Only cached verified bytes are eligible, and normal admission
	// after repair still enforces the original lease and replay floor.
	if m.Gossip.Op == "have" {
		if payload := s.gossip.Lookup(m.Gossip.ID(), now); len(payload) > 0 {
			var w SignedManifest
			if json.Unmarshal(payload, &w) != nil {
				return nil, nil, errors.New("invalid cached manifest artifact")
			}
			if _, _, trusted := s.cache.Get(w.Principal, now); !trusted {
				if _, waiting := s.pending[w.Principal]; waiting || len(s.pending) >= 128 {
					return nil, nil, nil
				}
				return s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
			}
		}
	}
	var descriptor *CertificateMessage
	var hintKey string
	if m.Gossip.Op == "want" {
		payload := s.gossip.Lookup(m.Gossip.ID(), now)
		if len(payload) > 0 {
			var w SignedManifest
			if err := json.Unmarshal(payload, &w); err != nil {
				return nil, nil, err
			}
			chain, ok := s.directory.CurrentRecordChain(w, now)
			if !ok {
				return []ControlMessage{{Kind: "gossip", Gossip: netgossip.Unavailable(*m.Gossip)}}, nil, nil
			}
			descriptor = DescribeCertificates(w.Principal, chain)
			if descriptor == nil {
				return nil, nil, errors.New("invalid current manifest chain")
			}
			encoded, _ := json.Marshal(descriptor.Chain)
			digest := sha256.Sum256(encoded)
			hintKey = string(digest[:])
			if s.sent[hintKey].After(now) {
				descriptor = nil
			}
		}
	}
	reply, data, err := s.gossip.Receive(*m.Gossip, now)
	var replies []ControlMessage
	if reply != nil {
		if reply.Op == "data" && descriptor != nil {
			// The exact verified chain precedes the first DATA, so a warm peer
			// can bind a bounded retry before its new leaf finishes fetching.
			// Callers atomically queue this batch or close the session.
			if len(s.sent) >= 1024 {
				s.sent = map[string]time.Time{}
			}
			s.sent[hintKey] = now.Add(MaxLease)
			replies = append(replies, ControlMessage{Kind: "certificate", Certificate: descriptor})
		}
		replies = append(replies, ControlMessage{Kind: "gossip", Gossip: reply})
	}
	if err == nil && m.Gossip.Op == "want" && len(slots) > 0 {
		for _, chunk := range s.gossip.Poll(now, max(0, slots[0]-len(replies))) {
			replies = append(replies, ControlMessage{Kind: "gossip", Gossip: chunk})
		}
	}
	if err != nil || len(data) == 0 {
		return replies, nil, err
	}
	var w SignedManifest
	if err = json.Unmarshal(data, &w); err != nil {
		return nil, nil, err
	}
	// Require the exact deterministic envelope used for hashing, including no
	// unknown fields. Otherwise re-marshalling could leave a claim unresolved.
	canonical, _ := json.Marshal(w)
	if string(canonical) != string(data) {
		return nil, nil, errors.New("noncanonical manifest artifact")
	}
	out, changed, err := s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
	return append(replies, out...), changed, err
}

// manifestPayload recovers one previously advertised record by its store key.
// The pool checks the exact digest so a replacement cannot serve different bytes.
func (d *Directory) manifestPayload(principal string, now time.Time) []byte {
	d.mu.Lock()
	d.expireLocked(now)
	e, ok := d.entries[principal]
	d.mu.Unlock()
	if !ok {
		return nil
	}
	// Recheck the exact immutable wire after unlocking. Calling the public
	// guard under d.mu would deadlock; copying before expiry could revive it.
	if _, ok := d.CurrentRecordChain(e.wire, now); !ok {
		return nil
	}
	data, _ := json.Marshal(e.wire)
	return data
}

func (s *Synchronizer) discardManifest(w SignedManifest) {
	if s.gossip == nil {
		return
	}
	data, _ := json.Marshal(w)
	s.gossip.Discard(data)
}
