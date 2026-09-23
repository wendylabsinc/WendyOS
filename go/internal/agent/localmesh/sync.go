package localmesh

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const MaxControlMessage = 64 << 10

// Control messages use one bounded reliable stream per authenticated link;
// routing and payload packets remain independent QUIC datagrams.
type ControlMessage struct {
	Kind        string          `json:"kind"`
	Hello       *LinkHello      `json:"hello,omitempty"`
	Bundle      [][]byte        `json:"bundle,omitempty"`
	Manifest    *SignedManifest `json:"manifest,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
}

func WriteControl(w io.Writer, m ControlMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxControlMessage {
		return errors.New("oversized local-mesh control message")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err = writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, data)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func ReadControl(r io.Reader) (ControlMessage, error) {
	var m ControlMessage
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxControlMessage {
		return m, errors.New("invalid control message length")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	switch m.Kind {
	case "hello":
		if m.Hello == nil || len(m.Bundle) != 0 || m.Manifest != nil || m.Fingerprint != "" {
			return m, errors.New("invalid hello message")
		}
	case "bundle":
		if len(m.Bundle) == 0 || len(m.Bundle) > 16 || m.Manifest != nil || m.Fingerprint != "" {
			return m, errors.New("invalid bundle message")
		}
	case "manifest":
		if m.Manifest == nil || len(m.Bundle) != 0 || m.Fingerprint != "" {
			return m, errors.New("invalid manifest message")
		}
	case "identity-request":
		if len(m.Fingerprint) != 64 || m.Manifest != nil || len(m.Bundle) != 0 {
			return m, errors.New("invalid identity request")
		}
	default:
		return m, errors.New("unknown local-mesh control message")
	}
	if m.Kind != "hello" && m.Hello != nil {
		return m, errors.New("unexpected hello payload")
	}
	return m, nil
}

// Synchronizer is per-link, single-caller, and does not own networking. Changed
// records are broadcast immediately by its caller; periodic Reconcile repairs
// loss/reconnect. Certificates are sent once per bounded link cache, not once per
// service. A missing bundle holds at most eight manifests until a validated reply.
type Synchronizer struct {
	directory *Directory
	cache     *IdentityCache
	sent      map[string]bool
	pending   map[string]SignedManifest
}

func NewSynchronizer(d *Directory, c *IdentityCache) *Synchronizer {
	return &Synchronizer{directory: d, cache: c, sent: map[string]bool{}, pending: map[string]SignedManifest{}}
}

func (s *Synchronizer) Record(w SignedManifest, now time.Time) []ControlMessage {
	var out []ControlMessage
	if !s.sent[w.Fingerprint] {
		chain, _, ok := s.cache.Get(w.Fingerprint, now)
		if !ok {
			return nil
		}
		if len(s.sent) >= 1024 {
			s.sent = map[string]bool{}
		}
		s.sent[w.Fingerprint] = true
		out = append(out, ControlMessage{Kind: "bundle", Bundle: chain})
	}
	w.Body = append(json.RawMessage(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	return append(out, ControlMessage{Kind: "manifest", Manifest: &w})
}

func (s *Synchronizer) Reconcile(now time.Time) []ControlMessage {
	var out []ControlMessage
	for _, w := range s.directory.Records(now) {
		out = append(out, s.Record(w, now)...)
	}
	return out
}

// Receive returns replies plus independently verified changed records to relay.
// Invalid signatures/identities are fatal to this link, not admitted as hints.
func (s *Synchronizer) Receive(m ControlMessage, now time.Time) (replies []ControlMessage, changed []SignedManifest, err error) {
	switch m.Kind {
	case "identity-request":
		chain, _, ok := s.cache.Get(m.Fingerprint, now)
		if ok {
			replies = append(replies, ControlMessage{Kind: "bundle", Bundle: chain})
		}
	case "bundle":
		fp, e := s.cache.Put(m.Bundle, now)
		if e != nil {
			return nil, nil, e
		}
		if w, ok := s.pending[fp]; ok {
			delete(s.pending, fp)
			return s.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
		}
	case "manifest":
		if m.Manifest == nil {
			return nil, nil, errors.New("missing manifest")
		}
		ok, e := s.directory.Accept(*m.Manifest, now)
		// Reordering across relays and leases expiring in transit are normal,
		// not grounds to disconnect an authenticated neighbour.
		if errors.Is(e, ErrStaleManifest) || errors.Is(e, ErrExpiredManifest) {
			return nil, nil, nil
		}
		if errors.Is(e, ErrIdentityNeeded) {
			if len(s.pending) >= 8 {
				return nil, nil, errors.New("too many unresolved identities")
			}
			s.pending[m.Manifest.Fingerprint] = *m.Manifest
			return []ControlMessage{{Kind: "identity-request", Fingerprint: m.Manifest.Fingerprint}}, nil, nil
		}
		if e != nil {
			return nil, nil, e
		}
		if ok {
			changed = append(changed, *m.Manifest)
		}
	default:
		return nil, nil, errors.New("unknown directory message")
	}
	return replies, changed, nil
}
