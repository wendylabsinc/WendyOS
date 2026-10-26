package meshcatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

const MaxMessageBytes = 64 << 10

const maxPendingRecords = 128
const maxPendingRecordBytes = 128 << 10

var errPendingIdentities = errors.New("too many pending wendy net service identities")

// Message is carried on a reliable, authenticated mesh control stream.
// Signed records retain their origin proof across relays. Missing certificate
// elements are requested after reordered announcements or cache misses.
type Message struct {
	Gossip      *netgossip.Message            `json:"gossip,omitempty"`
	Kind        string                        `json:"kind"`
	Record      *SignedRecord                 `json:"record,omitempty"`
	Bundle      [][]byte                      `json:"bundle,omitempty"`
	Principal   string                        `json:"principal,omitempty"`
	Certificate *localmesh.CertificateMessage `json:"certificate,omitempty"`
}

func (m Message) validate() error {
	if m.Kind != "certificate" && m.Certificate != nil {
		return errors.New("unexpected service certificate message")
	}
	if m.Kind != "gossip" && m.Gossip != nil {
		return errors.New("unexpected service gossip payload")
	}
	switch m.Kind {
	case "gossip":
		if m.Gossip == nil || m.Gossip.Type != netgossip.Service || m.Record != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return errors.New("invalid service gossip envelope")
		}
		return m.Gossip.Validate()
	case "certificate":
		if m.Certificate == nil || m.Record != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return errors.New("invalid service certificate envelope")
		}
		return m.Certificate.Validate()
	case "record":
		if m.Record == nil || len(m.Bundle) != 0 || m.Principal != "" {
			return errors.New("invalid service record message")
		}
	case "bundle":
		if m.Record != nil || len(m.Bundle) == 0 || len(m.Bundle) > 16 || m.Principal != "" {
			return errors.New("invalid service bundle message")
		}
	case "identity-request":
		if m.Record != nil || len(m.Bundle) != 0 ||
			m.Principal == "" || len(m.Principal) > 512 {
			return errors.New("invalid service identity request")
		}
	case "snapshot-done":
		if m.Record != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return errors.New("invalid service snapshot completion")
		}
	default:
		return errors.New("unknown service message")
	}
	return nil
}

func WriteMessage(w io.Writer, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > MaxMessageBytes {
		return errors.New("oversized service message")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if err := writeFull(w, hdr[:]); err != nil {
		return err
	}
	return writeFull(w, data)
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func ReadMessage(r io.Reader) (Message, error) {
	var m Message
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxMessageBytes {
		return m, errors.New("invalid service message length")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, err
	}
	return m, m.validate()
}

// Synchronizer is owned by one peer link. Callers fan out changed signed
// records and periodically Reconcile to repair a partition or dropped frame.
type Synchronizer struct {
	gossip  *netgossip.Session
	catalog *Catalog
	cache   *localmesh.IdentityCache
	// Legacy leaf-fingerprint keys mean known bundles. Gossip keys are full
	// descriptor digests with freshness bounded by the certificate fetch lease.
	sent map[string]time.Time
	// One authenticated neighbour control stream delivers records in order. Remember the
	// exact signed body last queued for each service so the periodic repair
	// pass sends only records that a dropped Broadcast never queued.
	sentRecords  map[Key]sentRecordStamp
	pending      map[string][]pendingServiceRecord
	certificates *localmesh.CertificateExchange
}

type pendingServiceRecord struct {
	wire    SignedRecord
	renewal *pendingServiceRenewal
}

type pendingServiceRenewal struct {
	chain    localmesh.CertificateChain
	deadline time.Time
}

type sentRecordStamp struct {
	generation uint64
	digest     [32]byte
}

func NewSynchronizer(c *Catalog, cache *localmesh.IdentityCache, pools ...*netgossip.Pool) *Synchronizer {
	s := &Synchronizer{catalog: c, cache: cache, sent: map[string]time.Time{},
		sentRecords: map[Key]sentRecordStamp{}, pending: map[string][]pendingServiceRecord{}, certificates: localmesh.NewCertificateExchange(cache, c.device.Tenant())}
	if len(pools) > 0 && pools[0] != nil {
		s.gossip = pools[0].NewSession(netgossip.Service)
	}
	return s
}

func (s *Synchronizer) Record(w SignedRecord, now time.Time) []Message {
	if s.gossip != nil {
		return s.offerService(w, now)
	}
	var record Record
	if json.Unmarshal(w.Body, &record) != nil {
		return nil
	}
	stamp := sha256.Sum256(append([]byte(w.Fingerprint), w.Body...))
	if old, found := s.sentRecords[record.Key]; found &&
		(record.Generation < old.generation || record.Generation == old.generation && old.digest == stamp) {
		return nil
	}
	// Suppressing an already-queued record does not consume its identity or
	// transmit data. Check the stamp first; every record actually transmitted
	// still needs a current-trust lookup below. Catalog.Records independently
	// validates origins before reconciliation/projection.
	chain, _, ok := s.cache.Get(record.Key.Device, now)
	if !ok || localmesh.Fingerprint(chain) != w.Fingerprint {
		return nil
	}
	var out []Message
	if _, known := s.sent[w.Fingerprint]; !known {
		if len(s.sent) >= 1024 {
			s.sent = map[string]time.Time{}
		}
		s.sent[w.Fingerprint] = time.Time{}
		// Use this exact verified chain. A second principal lookup could race
		// renewal and describe a different certificate than the record names.
		out = append(out, Message{Kind: "certificate", Certificate: localmesh.DescribeCertificates(record.Key.Device, chain)})
	}
	w = cloneWire(w)
	s.sentRecords[record.Key] = sentRecordStamp{record.Generation, stamp}
	return append(out, Message{Kind: "record", Record: &w})
}

func (s *Synchronizer) Reconcile(now time.Time) []Message {
	var out []Message
	live := make(map[Key]bool)
	for _, w := range s.catalog.Records(now) {
		var record Record
		if json.Unmarshal(w.Body, &record) == nil {
			live[record.Key] = true
		}
		out = append(out, s.Record(w, now)...)
	}
	for key := range s.sentRecords {
		if !live[key] {
			delete(s.sentRecords, key)
		}
	}
	return out
}

// snapshotRecords retains only stamps for keys still present in the bounded
// catalog. The runtime feeds this snapshot incrementally to its bounded writer.
func (s *Synchronizer) snapshotRecords(now time.Time) []SignedRecord {
	records := s.catalog.Records(now)
	live := make(map[Key]bool, len(records))
	for _, w := range records {
		var record Record
		if json.Unmarshal(w.Body, &record) == nil {
			live[record.Key] = true
		}
	}
	for key := range s.sentRecords {
		if !live[key] {
			delete(s.sentRecords, key)
		}
	}
	return records
}

// PendingIdentity reports whether a record from this peer still awaits its
// signed origin's certificate chain. Each record is admitted only after
// verification; projection additionally requires its independent origin proof.
func (s *Synchronizer) PendingIdentity() bool { return len(s.pending) != 0 }
func (s *Synchronizer) PendingAnnouncements(now time.Time) bool {
	return s.PendingIdentity() || (s.gossip != nil && s.gossip.Pending(now))
}

// Receive returns replies for this peer and newly admitted records to relay.
// Invalid signatures and identities fail the link; ordinary stale/expired
// records are ignored. Relaying never changes the signed absolute expiry.
func (s *Synchronizer) receive(m Message, now time.Time) (replies []Message, changed []SignedRecord, err error) {
	if err := m.validate(); err != nil {
		return nil, nil, err
	}
	switch m.Kind {
	case "certificate":
		replies, completed, err := s.certificates.Receive(*m.Certificate, now)
		return s.certificateResult(replies, completed, err, now)
	case "snapshot-done":
		// Runtime consumes this marker after all preceding records on the
		// ordered stream have been admitted or queued for identity repair.
	case "identity-request":
		if descriptor := s.certificates.Describe(m.Principal, now); descriptor != nil {
			return []Message{{Kind: "certificate", Certificate: descriptor}}, nil, nil
		}
	case "bundle":
		principal, err := s.cache.Put(m.Bundle, now)
		if err != nil {
			return nil, nil, err
		}
		pending := s.pending[principal]
		delete(s.pending, principal)
		// Warm data belongs to its advertised exchange. A legacy bundle may
		// populate public bytes, but only matching exchange completion retries it.
		for _, entry := range pending {
			if entry.renewal != nil {
				s.pending[principal] = append(s.pending[principal], entry)
			}
		}
		for i, entry := range pending {
			if entry.renewal != nil {
				continue
			}
			_, admitted, err := s.receive(Message{Kind: "record", Record: &entry.wire}, now)
			if err != nil {
				for _, remaining := range pending[i:] {
					if remaining.renewal == nil {
						s.discardService(remaining.wire)
					}
				}
				return nil, nil, err
			}
			changed = append(changed, admitted...)
		}
	case "record":
		s.prunePending(now)
		ok, err := s.catalog.Accept(*m.Record, now)
		if s.gossip != nil && (err == nil || errors.Is(err, ErrStale)) {
			s.resolveService(*m.Record, now, err == nil)
		}
		if s.gossip != nil && errors.Is(err, ErrExpired) {
			s.discardService(*m.Record)
		}
		if errors.Is(err, ErrExpired) || errors.Is(err, ErrStale) {
			return nil, nil, nil
		}
		var renewal *pendingServiceRenewal
		var lifetime *serviceCertificateLifetimeError
		lifetimeMismatch := errors.As(err, &lifetime)
		if errors.Is(err, errCertificateMismatch) || lifetimeMismatch {
			var record Record
			if json.Unmarshal(m.Record.Body, &record) == nil && record.Key.Mesh == s.catalog.mesh {
				chain, deadline, active := s.certificates.PendingChain(record.Key.Device, now)
				if active && len(chain.Hashes) > 0 && chain.Hashes[0] == m.Record.Fingerprint {
					// A reissued peer intermediate can extend effective lifetime while
					// retaining the exact leaf. Only a different advertised full chain
					// can repair that typed bound; normal verification still owns trust.
					if !lifetimeMismatch || !sameServiceChain(lifetime.chain, chain) {
						renewal = &pendingServiceRenewal{chain: chain, deadline: deadline}
					}
				}
			}
		}
		if errors.Is(err, ErrIdentityNeeded) || renewal != nil {
			// All records already waiting on this exact origin/descriptor share
			// its first fetch deadline, including new records arriving after the
			// old identity expires. Descriptor/data churn cannot renew that wait.
			var record Record
			if json.Unmarshal(m.Record.Body, &record) == nil {
				for _, entry := range s.pending[record.Key.Device] {
					if entry.renewal == nil || entry.wire.Fingerprint != m.Record.Fingerprint {
						continue
					}
					if renewal == nil {
						chain, _, active := s.certificates.PendingChain(record.Key.Device, now)
						if active && sameServiceChain(entry.renewal.chain, chain) {
							renewal = &pendingServiceRenewal{chain: chain, deadline: entry.renewal.deadline}
						}
					} else if sameServiceChain(entry.renewal.chain, renewal.chain) && entry.renewal.deadline.Before(renewal.deadline) {
						renewal.deadline = entry.renewal.deadline
					}
				}
			}
		}
		if errors.Is(err, ErrIdentityNeeded) || renewal != nil {
			// Repeated hints/data cannot refresh a deferred artifact's first
			// deadline, even if expiry/eviction of the old cached certificate now
			// makes verification report an ordinary cold-cache miss.
			for _, entries := range s.pending {
				for _, entry := range entries {
					if entry.renewal != nil && sameServiceWire(entry.wire, *m.Record) {
						return nil, nil, nil
					}
				}
			}
			count, retainedBytes := 0, 0
			for _, entries := range s.pending {
				count += len(entries)
				for _, wire := range entries {
					retainedBytes += pendingServiceBytes(wire)
				}
			}
			pending := pendingServiceRecord{wire: cloneWire(*m.Record), renewal: renewal}
			incomingBytes := pendingServiceBytes(pending)
			if count >= maxPendingRecords || retainedBytes+incomingBytes > maxPendingRecordBytes {
				s.discardService(*m.Record)
				return nil, nil, errPendingIdentities
			}
			var record Record
			if json.Unmarshal(m.Record.Body, &record) != nil {
				return nil, nil, errors.New("invalid wendy net service record body")
			}
			first := len(s.pending[record.Key.Device]) == 0
			s.pending[record.Key.Device] = append(s.pending[record.Key.Device], pending)
			if first && renewal == nil {
				if s.gossip != nil {
					if _, _, active := s.certificates.PendingChain(record.Key.Device, now); active {
						return nil, nil, nil
					}
				}
				return []Message{{Kind: "identity-request", Principal: record.Key.Device}}, nil, nil
			}
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		s.noteReceived(*m.Record)
		if ok {
			changed = append(changed, cloneWire(*m.Record))
		}
	}
	return replies, changed, nil
}

func (s *Synchronizer) certificateResult(messages []*localmesh.CertificateMessage, completed []string, err error, now time.Time) (replies []Message, changed []SignedRecord, result error) {
	if err != nil {
		return nil, nil, err
	}
	for _, m := range messages {
		replies = append(replies, Message{Kind: "certificate", Certificate: m})
	}
	// Poll can complete several principals at once. Detach all their data
	// before recursive Receive prunes descriptors already removed by Poll.
	ready := make(map[string][]pendingServiceRecord, len(completed))
	for _, principal := range completed {
		ready[principal] = s.pending[principal]
		delete(s.pending, principal)
	}
	for _, principal := range completed {
		entries := ready[principal]
		delete(ready, principal)
		for i, w := range entries {
			if !s.pendingReady(principal, w, now) {
				s.discardService(w.wire)
				continue
			}
			out, admitted, err := s.receive(Message{Kind: "record", Record: &w.wire}, now)
			if err != nil {
				// All completions were detached before replay. Release the
				// remaining unverified claims even when the first retry fails.
				for _, remaining := range entries[i:] {
					s.discardService(remaining.wire)
				}
				for _, unprocessed := range ready {
					for _, remaining := range unprocessed {
						s.discardService(remaining.wire)
					}
				}
				return nil, nil, err
			}
			replies = append(replies, out...)
			changed = append(changed, admitted...)
		}
	}
	s.prunePending(now)
	return replies, changed, nil
}

func (s *Synchronizer) ContinueCertificates(now time.Time, slots ...int) ([]Message, []SignedRecord, error) {
	s.prunePending(now)
	messages, completed, err := s.certificates.Poll(now)
	replies, changed, err := s.certificateResult(messages, completed, err, now)
	if s.gossip != nil {
		budget := 12
		if len(slots) > 0 {
			budget = max(0, slots[0]-len(replies))
		}
		for _, m := range s.gossip.Poll(now, budget) {
			replies = append(replies, Message{Kind: "gossip", Gossip: m})
		}
	}
	return replies, changed, err
}

func (s *Synchronizer) Close() {
	for _, entries := range s.pending {
		for _, entry := range entries {
			s.discardService(entry.wire)
		}
	}
	s.certificates.Close()
	clear(s.pending)
	if s.gossip != nil {
		s.gossip.Close()
	}
}

func sameServiceWire(a, b SignedRecord) bool {
	return a.Fingerprint == b.Fingerprint && bytes.Equal(a.Body, b.Body) && bytes.Equal(a.Signature, b.Signature)
}
func sameServiceChain(a, b localmesh.CertificateChain) bool {
	return a.Principal == b.Principal && slices.Equal(a.Hashes, b.Hashes)
}
func pendingServiceBytes(p pendingServiceRecord) int {
	n := len(p.wire.Body) + len(p.wire.Signature) + len(p.wire.Fingerprint)
	if p.renewal != nil {
		n += len(p.renewal.chain.Principal)
		for _, hash := range p.renewal.chain.Hashes {
			n += len(hash)
		}
	}
	return n
}

// A pending renewal is retried only with its advertised full chain already
// admitted by the unchanged verifier.
func (s *Synchronizer) pendingReady(principal string, p pendingServiceRecord, now time.Time) bool {
	if p.renewal == nil {
		return true
	}
	if !p.renewal.deadline.After(now) {
		return false
	}
	chain, _, ok := s.cache.Get(principal, now)
	if !ok {
		return false
	}
	description := localmesh.DescribeCertificates(principal, chain)
	return description != nil && sameServiceChain(p.renewal.chain, *description.Chain)
}

func (s *Synchronizer) prunePending(now time.Time) {
	for principal, records := range s.pending {
		keep := records[:0]
		for _, entry := range records {
			var record Record
			if json.Unmarshal(entry.wire.Body, &record) != nil || !time.UnixMilli(record.Expires).After(now) {
				if s.gossip != nil {
					s.discardService(entry.wire)
				}
				continue
			}
			if entry.renewal != nil {
				chain, _, active := s.certificates.PendingChain(principal, now)
				if !entry.renewal.deadline.After(now) || !active || !sameServiceChain(entry.renewal.chain, chain) {
					s.discardService(entry.wire)
					continue
				}
			}
			keep = append(keep, entry)
		}
		clear(records[len(keep):])
		if len(keep) == 0 {
			delete(s.pending, principal)
		} else {
			s.pending[principal] = keep
		}
	}
}

// Only called after Catalog.Accept has verified the artifact, including duplicates.
// A supplier already possesses its exact body and signing certificate.
func (s *Synchronizer) noteReceived(w SignedRecord) {
	var r Record
	if json.Unmarshal(w.Body, &r) != nil {
		return
	}
	stamp := sentRecordStamp{r.Generation, sha256.Sum256(append([]byte(w.Fingerprint), w.Body...))}
	if old, ok := s.sentRecords[r.Key]; !ok || old.generation < r.Generation || (old.generation == r.Generation && old.digest == stamp.digest) {
		s.sentRecords[r.Key] = stamp
	}
	if len(s.sent) >= 1024 {
		s.sent = make(map[string]time.Time)
	}
	s.sent[w.Fingerprint] = time.Time{}
}
