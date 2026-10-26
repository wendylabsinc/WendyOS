package meshcatalog

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func (s *Synchronizer) offerService(w SignedRecord, now time.Time) []Message {
	var record Record
	if json.Unmarshal(w.Body, &record) != nil {
		return nil
	}
	if _, ok := s.catalog.CurrentRecordChain(w, now); !ok {
		return nil
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	catalog, key := s.catalog, record.Key
	m := s.gossip.Offer(data, time.UnixMilli(record.Expires), now, func(at time.Time) []byte {
		return catalog.servicePayload(key, at)
	})
	if m == nil {
		return nil
	}
	return []Message{{Kind: "gossip", Gossip: m}}
}
func (s *Synchronizer) resolveService(w SignedRecord, now time.Time, accepted bool) {
	var record Record
	_ = json.Unmarshal(w.Body, &record)
	data, _ := json.Marshal(w)
	s.gossip.Resolve(data, time.UnixMilli(record.Expires), now, accepted)
}
func (s *Synchronizer) Receive(m Message, now time.Time, slots ...int) ([]Message, []SignedRecord, error) {
	if m.Kind != "gossip" {
		if s.gossip != nil && m.Kind == "record" {
			return nil, nil, errors.New("unsolicited service record")
		}
		return s.receive(m, now)
	}
	if s.gossip == nil || m.Gossip == nil {
		return nil, nil, errors.New("service gossip not negotiated")
	}
	if err := m.validate(); err != nil {
		return nil, nil, err
	}
	if m.Gossip.Type != netgossip.Service {
		return nil, nil, errors.New("gossip session type mismatch")
	}
	var descriptor *localmesh.CertificateMessage
	var hintKey string
	if m.Gossip.Op == "want" {
		payload := s.gossip.Lookup(m.Gossip.ID(), now)
		if len(payload) > 0 {
			var w SignedRecord
			var r Record
			if json.Unmarshal(payload, &w) != nil || json.Unmarshal(w.Body, &r) != nil {
				return nil, nil, errors.New("invalid cached service artifact")
			}
			chain, ok := s.catalog.CurrentRecordChain(w, now)
			if !ok {
				return []Message{{Kind: "gossip", Gossip: netgossip.Unavailable(*m.Gossip)}}, nil, nil
			}
			descriptor = localmesh.DescribeCertificates(r.Key.Device, chain)
			if descriptor == nil {
				return nil, nil, errors.New("invalid current service chain")
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
	var replies []Message
	if reply != nil {
		if reply.Op == "data" && descriptor != nil {
			// HAVE stays one frame. The current exact chain travels only with
			// requested DATA, before a receiver may need to defer verification.
			// Refresh after the existing certificate-fetch lease so a timed-out
			// exchange can recover even for a longer-lived gateway record.
			if len(s.sent) >= 1024 {
				s.sent = map[string]time.Time{}
			}
			s.sent[hintKey] = now.Add(localmesh.MaxLease)
			replies = append(replies, Message{Kind: "certificate", Certificate: descriptor})
		}
		replies = append(replies, Message{Kind: "gossip", Gossip: reply})
	}
	if err == nil && m.Gossip.Op == "want" && len(slots) > 0 {
		for _, chunk := range s.gossip.Poll(now, max(0, slots[0]-len(replies))) {
			replies = append(replies, Message{Kind: "gossip", Gossip: chunk})
		}
	}
	if err != nil || len(data) == 0 {
		return replies, nil, err
	}
	var w SignedRecord
	if err = json.Unmarshal(data, &w); err != nil {
		return nil, nil, err
	}
	canonical, _ := json.Marshal(w)
	if string(canonical) != string(data) {
		return nil, nil, errors.New("noncanonical service artifact")
	}
	out, changed, err := s.receive(Message{Kind: "record", Record: &w}, now)
	return append(replies, out...), changed, err
}

// servicePayload recovers one current record without an attacker-driven scan.
func (c *Catalog) servicePayload(key Key, now time.Time) []byte {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	// Recheck the exact immutable wire outside c.mu. Current trust, effective
	// issuer lifetime and any replacement must still permit this same artifact.
	if _, ok := c.CurrentRecordChain(e.wire, now); !ok {
		return nil
	}
	data, _ := json.Marshal(e.wire)
	return data
}

func (s *Synchronizer) discardService(w SignedRecord) {
	if s.gossip == nil {
		return
	}
	data, _ := json.Marshal(w)
	s.gossip.Discard(data)
}
