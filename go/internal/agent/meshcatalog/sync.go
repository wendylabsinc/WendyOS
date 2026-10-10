package meshcatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

const MaxMessageBytes = 64 << 10

// Message is carried on a reliable, authenticated mesh control stream.
// Signed records retain their origin proof across relays. The bundle request
// path repairs reordered announcements and cache misses after restart.
type Message struct {
	Kind        string        `json:"kind"`
	Record      *SignedRecord `json:"record,omitempty"`
	Bundle      [][]byte      `json:"bundle,omitempty"`
	Fingerprint string        `json:"fingerprint,omitempty"`
}

func (m Message) validate() error {
	switch m.Kind {
	case "record":
		if m.Record == nil || len(m.Bundle) != 0 || m.Fingerprint != "" {
			return errors.New("invalid service record message")
		}
	case "bundle":
		if m.Record != nil || len(m.Bundle) == 0 || len(m.Bundle) > 16 || m.Fingerprint != "" {
			return errors.New("invalid service bundle message")
		}
	case "identity-request":
		if m.Record != nil || len(m.Bundle) != 0 || len(m.Fingerprint) != 64 {
			return errors.New("invalid service identity request")
		}
	case "snapshot-done":
		if m.Record != nil || len(m.Bundle) != 0 || m.Fingerprint != "" {
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
	catalog *Catalog
	cache   *localmesh.IdentityCache
	sent    map[string]bool
	// Hints describe bundles successfully written to this same peer during a
	// previous session. A restarted peer may have lost them, so after a small
	// number of record-only frames send a bundle proactively. Missing bundles
	// are also repaired by the authenticated identity-request path.
	hinted            map[string]bool
	hintedRecordCount int
	// One authenticated TCP session delivers records in order. Remember the
	// exact signed body last queued for each service so the periodic repair
	// pass sends only records that a dropped Broadcast never queued.
	sentRecords map[Key]sentRecordStamp
	pending     map[string][]SignedRecord
}

type sentRecordStamp struct {
	generation uint64
	digest     [32]byte
}

func NewSynchronizer(c *Catalog, cache *localmesh.IdentityCache) *Synchronizer {
	return &Synchronizer{catalog: c, cache: cache, sent: map[string]bool{},
		hinted: map[string]bool{}, sentRecords: map[Key]sentRecordStamp{}, pending: map[string][]SignedRecord{}}
}

// SeedKnownBundles must run before the first Reconcile. It changes only wire
// efficiency: every received record still needs a currently valid cached
// identity and a valid origin signature before Catalog.Accept admits it.
func (s *Synchronizer) SeedKnownBundles(fingerprints []string) {
	for _, fp := range fingerprints {
		if len(fp) == 64 {
			s.sent[fp] = true
			s.hinted[fp] = true
		}
	}
}

func (s *Synchronizer) Record(w SignedRecord, now time.Time) []Message {
	var record Record
	parsed := json.Unmarshal(w.Body, &record) == nil
	stamp := sha256.Sum256(append([]byte(w.Fingerprint), w.Body...))
	if parsed {
		if old, found := s.sentRecords[record.Key]; found &&
			(record.Generation < old.generation || record.Generation == old.generation && old.digest == stamp) {
			return nil
		}
	}
	// Suppressing an already-queued record does not consume its identity or
	// transmit data. Check the stamp first; every record actually transmitted
	// still needs a current-trust lookup below. Catalog.Records independently
	// validates origins before reconciliation/projection.
	chain, _, ok := s.cache.Get(w.Fingerprint, now)
	if !ok {
		return nil
	}
	var out []Message
	if s.hinted[w.Fingerprint] {
		if s.hintedRecordCount >= 4 {
			// Limit pending records on a cold restarted receiver. The receiver
			// currently permits at most eight unknown-identity records.
			delete(s.sent, w.Fingerprint)
			delete(s.hinted, w.Fingerprint)
		} else {
			s.hintedRecordCount++
		}
	}
	if !s.sent[w.Fingerprint] {
		if len(s.sent) >= 1024 {
			s.sent = map[string]bool{}
		}
		s.sent[w.Fingerprint] = true
		out = append(out, Message{Kind: "bundle", Bundle: chain})
	}
	w = cloneWire(w)
	if parsed {
		s.sentRecords[record.Key] = sentRecordStamp{record.Generation, stamp}
	}
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

// PendingIdentity reports whether a record from this peer still awaits its
// signed origin's certificate bundle. A snapshot is not safe to project until
// all records sent before its completion marker have been verified.
func (s *Synchronizer) PendingIdentity() bool { return len(s.pending) != 0 }

// Receive returns replies for this peer and newly admitted records to relay.
// Invalid signatures and identities fail the link; ordinary stale/expired
// records are ignored. Relaying never changes the signed absolute expiry.
func (s *Synchronizer) Receive(m Message, now time.Time) (replies []Message, changed []SignedRecord, err error) {
	if err := m.validate(); err != nil {
		return nil, nil, err
	}
	switch m.Kind {
	case "snapshot-done":
		// Runtime consumes this marker after all preceding records on the
		// ordered stream have been admitted or queued for identity repair.
	case "identity-request":
		if chain, _, ok := s.cache.Get(m.Fingerprint, now); ok {
			return []Message{{Kind: "bundle", Bundle: chain}}, nil, nil
		}
	case "bundle":
		fp, err := s.cache.Put(m.Bundle, now)
		if err != nil {
			return nil, nil, err
		}
		pending := s.pending[fp]
		delete(s.pending, fp)
		for _, w := range pending {
			_, admitted, err := s.Receive(Message{Kind: "record", Record: &w}, now)
			if err != nil {
				return nil, nil, err
			}
			changed = append(changed, admitted...)
		}
	case "record":
		ok, err := s.catalog.Accept(*m.Record, now)
		if errors.Is(err, ErrExpired) || errors.Is(err, ErrStale) {
			return nil, nil, nil
		}
		if errors.Is(err, ErrIdentityNeeded) {
			count := 0
			for _, entries := range s.pending {
				count += len(entries)
			}
			if count >= 8 {
				return nil, nil, errors.New("too many pending mesh service identities")
			}
			fp := m.Record.Fingerprint
			s.pending[fp] = append(s.pending[fp], cloneWire(*m.Record))
			return []Message{{Kind: "identity-request", Fingerprint: fp}}, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if ok {
			changed = append(changed, cloneWire(*m.Record))
		}
	}
	return replies, changed, nil
}
