package localmesh

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

const maxManifestHaves = 8

// Keep ordinary inventory frames short enough to yield to data on slow links.
const manifestHavesPerFrame = 2
const manifestDigestSize = 32

// Keep compile-time proof beside the batch limit: a valid input can never owe
// more immediate replies than the credit reserved against bulk producers.
// The larger retained response budget alone cannot prove duplex progress.
var _ [maxManifestHaves - manifestHavesPerFrame]struct{}
var _ [directoryResponseReserve - maxManifestHaves]struct{}
var _ [maxDirectoryReplyFrames - maxManifestHaves]struct{}
var _ [maxDirectoryReplyBytes/directoryResponseBytes - maxManifestHaves]struct{}

// validateHaveBatch runs before any hint or download state is consumed. The
// concatenated full digests are availability hints, never signed authority.
func validateHaveBatch(m ControlMessage) error {
	if len(m.Haves) < 2*manifestDigestSize || len(m.Haves) > maxManifestHaves*manifestDigestSize || len(m.Haves)%manifestDigestSize != 0 ||
		m.Hello != nil || m.Bundle != nil || m.Manifest != nil || m.Principal != "" || m.Certificate != nil || m.Gossip != nil || m.Time != nil {
		return errors.New("invalid manifest HAVE batch")
	}
	var seen [maxManifestHaves][manifestDigestSize]byte
	for i := 0; i < len(m.Haves)/manifestDigestSize; i++ {
		copy(seen[i][:], m.Haves[i*manifestDigestSize:(i+1)*manifestDigestSize])
		for _, previous := range seen[:i] {
			if previous == seen[i] {
				return errors.New("duplicate manifest HAVE digest")
			}
		}
	}
	return nil
}

// packHaveBatch retains the individual queue owners. Only the compact digest
// vector crosses the wire. A surviving singleton keeps the original format.
func packHaveBatch(entries []ControlMessage) ControlMessage {
	if len(entries) == 1 {
		return entries[0]
	}
	m := ControlMessage{Kind: "manifest-haves", Haves: make([]byte, 0, manifestDigestSize*len(entries)), haveEntries: entries}
	for _, entry := range entries {
		m.Haves = append(m.Haves, entry.Gossip.Digest...)
	}
	return m
}

func (s *Synchronizer) receiveHaveBatch(m ControlMessage, now time.Time, slots ...int) ([]ControlMessage, []SignedManifest, error) {
	if !s.haveBatches || s.gossip == nil {
		return nil, nil, errors.New("manifest HAVE batches were not negotiated")
	}
	if err := validateHaveBatch(m); err != nil {
		return nil, nil, err
	}
	count := len(m.Haves) / manifestDigestSize
	if len(slots) > 0 && slots[0] < count {
		return nil, nil, ErrControlBackpressure
	}
	// Reserve the worst case before consuming the first digest. Every ordinary
	// HAVE owes at most one immediate reply, including identity-only repair.
	// A blocked batch stays as the actor's single ingress frame and resumes
	// unchanged after output credit is available; it never repeats a prefix.
	var replies []ControlMessage
	var changed []SignedManifest
	for offset := 0; offset < len(m.Haves); offset += manifestDigestSize {
		have := ControlMessage{Kind: "gossip", Gossip: &netgossip.Message{Version: netgossip.Version, Op: "have", Type: netgossip.Manifest, Digest: m.Haves[offset : offset+manifestDigestSize]}}
		out, records, err := s.Receive(have, now, count-len(replies))
		replies = append(replies, out...)
		changed = append(changed, records...)
		if errors.Is(err, ErrControlBackpressure) {
			// Never let the actor retry a consumed prefix if a future HAVE
			// handler violates the one-reply reservation contract.
			return replies, changed, errors.New("manifest HAVE exceeded reserved reply credit")
		}
		if err != nil {
			return replies, changed, err
		}
	}
	return replies, changed, nil
}

// prepareHaveBatch applies ordinary final-export checks to every origin. A
// stale or temporarily unverifiable entry must not discard healthy siblings.
// skipped runs in the caller's actor or writer ownership domain.
func (n *Node) prepareHaveBatch(p *nodePeer, m ControlMessage, now time.Time, skipped func(ControlMessage)) (ControlMessage, bool) {
	if m.Kind != "manifest-haves" {
		return m, true
	}
	if !p.haveBatches || len(m.haveEntries) == 0 || len(m.haveEntries) > maxManifestHaves {
		return ControlMessage{}, false
	}
	var retained []ControlMessage
	for i, entry := range m.haveEntries {
		if n.controlEligible(p, entry, now) {
			if retained != nil {
				retained = append(retained, entry)
			}
			continue
		}
		if retained == nil {
			retained = make([]ControlMessage, 0, len(m.haveEntries)-1)
			retained = append(retained, m.haveEntries[:i]...)
		}
		skipped(entry)
	}
	if retained == nil {
		return m, true
	}
	if len(retained) == 0 {
		return ControlMessage{}, false
	}
	return packHaveBatch(retained), true
}
