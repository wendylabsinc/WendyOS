package localmesh

import (
	"crypto/sha256"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func (s *Synchronizer) offerManifest(w SignedManifest, now time.Time) []ControlMessage {
	deadline, ok := s.directory.RecordDeadline(w, now)
	if !ok {
		s.forgetManifest(w)
		return nil
	}
	data, err := netwire.Marshal(w)
	if err != nil {
		return nil
	}
	s.trackArtifact(w, deadline)
	directory, principal := s.directory, w.ownerKey()
	m := s.gossip.Offer(data, deadline, now, func(at time.Time) []byte {
		return directory.manifestPayload(principal, at)
	})
	if m == nil {
		return nil
	}
	return []ControlMessage{s.gossipControl(m)}
}
func (s *Synchronizer) resolveManifest(w SignedManifest, now time.Time, accepted bool) {
	deadline, current := s.directory.RecordDeadline(w, now)
	if accepted && !current {
		return
	}
	if accepted {
		s.trackArtifact(w, deadline)
	}
	data, _ := netwire.Marshal(w)
	if !accepted {
		s.gossipPool.Forget(netgossip.Identify(netgossip.Manifest, data))
	}
	s.gossip.Resolve(data, deadline, now, accepted)
}

// Router links require HAVE/WANT. Direct-message fixtures exercise the same
// directory and certificate admission without opening a network carrier.
func (s *Synchronizer) Receive(m ControlMessage, now time.Time, slots ...int) ([]ControlMessage, []SignedManifest, error) {
	s.Sweep(now)
	if len(slots) > 0 && slots[0] < directoryResponseDemand(m) {
		return nil, nil, ErrControlBackpressure
	}
	if m.Kind != "gossip" {
		if s.gossip != nil && m.Kind == "manifest" {
			return nil, nil, errors.New("unsolicited router manifest")
		}
		return s.receive(m, now, slots...)
	}
	if m.Gossip != nil && (m.Gossip.FutureType() || m.Gossip.Op != "" && !m.Gossip.Known()) {
		return nil, nil, nil
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
			if netwire.Unmarshal(payload, &w) != nil {
				return nil, nil, errors.New("invalid cached manifest artifact")
			}
			_, _, trusted := s.cache.Get(w.Principal, now)
			if owner, err := w.peer(); err == nil && owner.IsOperator() {
				_, trusted = s.directory.CurrentRecordChain(w, now)
			}
			if !trusted {
				if _, waiting := s.pending[w.ownerKey()]; waiting || len(s.pending) >= 128 {
					return nil, nil, nil
				}
				return s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
			}
		}
	}
	var descriptor *CertificateMessage
	var descriptorDeadline time.Time
	var hintKey string
	if m.Gossip.Op == "want" {
		payload := s.gossip.Lookup(m.Gossip.ID(), now)
		if len(payload) > 0 {
			var w SignedManifest
			if err := netwire.Unmarshal(payload, &w); err != nil {
				return nil, nil, err
			}
			chain, ok := s.directory.CurrentRecordChain(w, now)
			if !ok {
				s.gossipPool.Forget(m.Gossip.ID())
				return []ControlMessage{{Kind: "gossip", Gossip: netgossip.Unavailable(*m.Gossip)}}, nil, nil
			}
			descriptorDeadline, _ = s.directory.RecordDeadline(w, now)
			s.trackArtifact(w, descriptorDeadline)
			descriptor = DescribeCertificates(w.Principal, chain)
			if descriptor == nil {
				return nil, nil, errors.New("invalid current manifest chain")
			}
			encoded, _ := netwire.Marshal(descriptor.Chain)
			digest := sha256.Sum256(encoded)
			hintKey = string(digest[:])
			if s.sent[hintKey].After(now) {
				descriptor = nil
			}
		}
	}
	bounded := len(slots) > 0
	var reply *netgossip.Message
	var data []byte
	var err error
	if bounded {
		reply, data, err = s.gossip.ReceiveDeferred(*m.Gossip, now)
	} else {
		reply, data, err = s.gossip.Receive(*m.Gossip, now)
	}
	var replies []ControlMessage
	if descriptor != nil && (reply != nil && reply.Op == "data" || bounded && reply == nil && m.Gossip.Op == "want" && err == nil) {
		// The exact verified chain precedes the first DATA, so a warm peer
		// can bind a bounded retry before its new leaf finishes fetching.
		// Callers atomically queue this batch or close the session.
		if len(s.sent) >= 1024 {
			s.sent = map[string]time.Time{}
		}
		s.sent[hintKey] = now.Add(MaxLease)
		replies = append(replies, ControlMessage{Kind: "certificate", Certificate: descriptor, deadline: descriptorDeadline, canceled: s.markerFor(m.Gossip.ID())})
	}
	if reply != nil {
		replies = append(replies, s.gossipControl(reply))
	}
	if err == nil && m.Gossip.Op == "want" && len(slots) > 0 {
		for _, chunk := range s.gossip.Poll(now, max(0, slots[0]-len(replies)-directoryResponseReserve)) {
			replies = append(replies, s.gossipControl(chunk))
		}
	}
	if err != nil || len(data) == 0 {
		return replies, nil, err
	}
	var w SignedManifest
	if err = netwire.Unmarshal(data, &w); err != nil {
		return nil, nil, err
	}
	// Retain the exact envelope used for hashing, including optional metadata,
	// so subsequent admission resolves this artifact's original claim.
	encoded, _ := netwire.Marshal(w)
	if string(encoded) != string(data) {
		return nil, nil, errors.New("manifest artifact encoding changed")
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
	data, _ := netwire.Marshal(e.wire)
	return data
}

func (s *Synchronizer) discardManifest(w SignedManifest) {
	if s.gossip == nil {
		return
	}
	data, _ := netwire.Marshal(w)
	s.gossip.Discard(data)
}

// forgetManifest removes pool bytes as soon as the authoritative directory
// no longer exports this exact artifact, including already queued uploads.
func (s *Synchronizer) forgetManifest(w SignedManifest) {
	if s.gossipPool == nil {
		return
	}
	data, _ := netwire.Marshal(w)
	id := netgossip.Identify(netgossip.Manifest, data)
	s.gossipPool.Forget(id)
	if artifact, ok := s.artifacts[w.ownerKey()]; ok && artifact.id == id {
		artifact.canceled.Store(true)
		delete(s.artifacts, w.ownerKey())
	}
}

// trackArtifact keeps only compact local queue ownership. Directory admission
// supplies the deadline; repeating/re-signing a body never starts a new lease.
func (s *Synchronizer) trackArtifact(w SignedManifest, deadline time.Time) manifestArtifact {
	data, _ := netwire.Marshal(w)
	id := netgossip.Identify(netgossip.Manifest, data)
	key := w.ownerKey()
	if old, ok := s.artifacts[key]; ok {
		if old.id == id {
			return old
		}
		old.canceled.Store(true)
		if s.gossipPool != nil {
			s.gossipPool.Forget(old.id)
		}
	}
	artifact := manifestArtifact{id: id, deadline: deadline, canceled: &atomic.Bool{}}
	s.artifacts[key] = artifact
	return artifact
}

func (s *Synchronizer) markerFor(id netgossip.ID) *atomic.Bool {
	for _, artifact := range s.artifacts {
		if artifact.id == id {
			return artifact.canceled
		}
	}
	return nil
}

func (s *Synchronizer) gossipControl(m *netgossip.Message) ControlMessage {
	return ControlMessage{Kind: "gossip", Gossip: m, canceled: s.markerFor(m.ID())}
}
