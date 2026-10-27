package localmesh

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const MaxControlMessage = 64 << 10

// Control messages use one bounded reliable stream per authenticated link;
// routing and payload packets remain independent QUIC datagrams.
type ControlMessage struct {
	Kind        string              `json:"kind"`
	Hello       *LinkHello          `json:"hello,omitempty"`
	Bundle      [][]byte            `json:"bundle,omitempty"`
	Manifest    *SignedManifest     `json:"manifest,omitempty"`
	Principal   string              `json:"principal,omitempty"`
	Certificate *CertificateMessage `json:"certificate,omitempty"`
	Gossip      *netgossip.Message  `json:"gossip,omitempty"`
	Catalog     []byte              `json:"catalog,omitempty"`
}

func WriteControl(w io.Writer, m ControlMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxControlMessage {
		return errors.New("oversized wendy net control message")
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
	if m.Kind != "catalog" && len(m.Catalog) != 0 {
		return m, errors.New("unexpected catalogue payload")
	}
	if m.Kind != "certificate" && m.Certificate != nil {
		return m, errors.New("unexpected certificate message")
	}
	if m.Kind != "gossip" && m.Gossip != nil {
		return m, errors.New("unexpected gossip payload")
	}
	switch m.Kind {
	case "gossip":
		if m.Gossip == nil || m.Gossip.Type != netgossip.Manifest || m.Hello != nil || m.Manifest != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid manifest gossip envelope")
		}
		if err := m.Gossip.Validate(); err != nil {
			return m, err
		}
	case "catalog":
		if len(m.Catalog) == 0 || len(m.Catalog) > MaxCatalogFrame || m.Hello != nil || m.Manifest != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid catalogue frame")
		}
	case "certificate":
		if m.Certificate == nil || m.Hello != nil || m.Manifest != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid certificate envelope")
		}
		if err := m.Certificate.validate(); err != nil {
			return m, err
		}
	case "hello":
		if m.Hello == nil || len(m.Bundle) != 0 || m.Manifest != nil || m.Principal != "" {
			return m, errors.New("invalid hello message")
		}
	case "bundle":
		if len(m.Bundle) == 0 || len(m.Bundle) > 16 || m.Manifest != nil || m.Principal != "" {
			return m, errors.New("invalid bundle message")
		}
	case "manifest":
		if m.Manifest == nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid manifest message")
		}
	case "identity-request":
		// Only a full device principal addresses an identity: no hash, no number.
		if _, err := meshid.DeviceFromPrincipal(m.Principal); err != nil {
			return m, errors.New("invalid identity request")
		}
		if m.Manifest != nil || len(m.Bundle) != 0 {
			return m, errors.New("invalid identity request")
		}
	default:
		return m, errors.New("unknown wendy net control message")
	}
	if m.Kind != "hello" && m.Hello != nil {
		return m, errors.New("unexpected hello payload")
	}
	return m, nil
}

// The original fetch allowance must retain its monotonic clock component even
// when the earlier signed expiry has only an absolute wall-clock timestamp.
type pendingManifestRenewal struct {
	chain      CertificateChain
	expires    time.Time
	fetchUntil time.Time
}

// Synchronizer is per-link, single-caller, and does not own networking. Changed
// records are broadcast immediately by its caller; periodic Reconcile repairs
// loss/reconnect. Bounded chain descriptions replace unsolicited bundles;
// certificate bytes are fetched individually only when missing. A bounded
// number of manifests wait for independently verified identity completion.
type Synchronizer struct {
	gossip    *netgossip.Session
	directory *Directory
	cache     *IdentityCache
	// leafBundles selects the negotiated Lite bundle role; routers use descriptors.
	leafBundles bool
	// Legacy leaf-fingerprint keys mean known bundles. Gossip keys are full
	// descriptor digests with freshness bounded by the certificate fetch lease.
	sent    map[string]time.Time
	pending map[string]SignedManifest
	// Warm-cache retries are bound to the exact advertised chain and deadline.
	pendingRenewals map[string]pendingManifestRenewal
	certificates    *CertificateExchange
	// A repair pass resumes after the last admitted principal, rather than
	// repeatedly filling a bounded neighbour queue with its sorted prefix.
	reconcileAfter     string
	reconcileRemaining int
}

func NewSynchronizer(d *Directory, c *IdentityCache, pools ...*netgossip.Pool) *Synchronizer {
	s := &Synchronizer{directory: d, cache: c, sent: map[string]time.Time{}, pending: map[string]SignedManifest{}, pendingRenewals: map[string]pendingManifestRenewal{}, certificates: NewCertificateExchange(c, d.tenant)}
	if len(pools) > 0 && pools[0] != nil {
		s.gossip = pools[0].NewSession(netgossip.Manifest)
	}
	return s
}

// SeedKnownBundles uses only the sender's per-peer record of completed writes.
// It does not trust the hint for authentication: the receiving directory still
// verifies each signed manifest against its own validated identity cache.
func (s *Synchronizer) SeedKnownBundles(fingerprints []string) {
	for _, fp := range fingerprints {
		if len(fp) == 64 {
			s.sent[fp] = time.Time{}
		}
	}
}

func (s *Synchronizer) Record(w SignedManifest, now time.Time) []ControlMessage {
	messages, _ := s.recordBounded(w, now, 2)
	return messages
}

// The bundle hint is consumed only if the complete record fits. A caller must
// reserve this many queue slots before sending the returned messages.
func (s *Synchronizer) recordBounded(w SignedManifest, now time.Time, slots int) ([]ControlMessage, bool) {
	if s.gossip != nil {
		if slots < 1 {
			return nil, false
		}
		return s.offerManifest(w, now), true
	}
	chain, ok := s.directory.CurrentRecordChain(w, now)
	if !ok {
		return nil, true
	}
	fp := Fingerprint(chain)
	_, known := s.sent[fp]
	bundleNeeded := !known
	needed := 1
	if bundleNeeded {
		needed++
	}
	if needed > slots {
		return nil, false
	}
	var out []ControlMessage
	if bundleNeeded {
		if len(s.sent) >= 1024 {
			s.sent = map[string]time.Time{}
		}
		s.sent[fp] = time.Time{}
		out = append(out, s.identityMessage(w.Principal, chain))
	}
	w.Body = append(json.RawMessage(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	return append(out, ControlMessage{Kind: "manifest", Manifest: &w}), true
}

// identityMessage is called only with a current independently verified cache
// entry. The leaf protocol cannot fetch descriptors or certificate chunks.
func (s *Synchronizer) identityMessage(principal string, chain [][]byte) ControlMessage {
	if s.leafBundles {
		return ControlMessage{Kind: "bundle", Bundle: chain}
	}
	return ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(principal, chain)}
}

func (s *Synchronizer) Reconcile(now time.Time) []ControlMessage {
	var out []ControlMessage
	for _, w := range s.directory.Records(now) {
		out = append(out, s.Record(w, now)...)
	}
	return out
}

// ReconcileBounded starts a repair pass using only the caller's free queue
// slots. ContinueReconcile drains the rest without restarting or resending the
// already admitted prefix. The normal repair interval may start a new pass;
// keeping the cursor ensures progress even if a slow link has not finished.
func (s *Synchronizer) ReconcileBounded(now time.Time, slots int) []ControlMessage {
	records := s.directory.Records(now)
	s.reconcileRemaining = len(records)
	return s.reconcileBatch(records, now, slots)
}

func (s *Synchronizer) ContinueReconcile(now time.Time, slots int) []ControlMessage {
	if s.reconcileRemaining == 0 || slots <= 0 {
		return nil
	}
	return s.reconcileBatch(s.directory.Records(now), now, slots)
}

func (s *Synchronizer) reconcileBatch(records []SignedManifest, now time.Time, slots int) []ControlMessage {
	s.reconcileRemaining = min(s.reconcileRemaining, len(records))
	if s.reconcileRemaining == 0 || slots <= 0 {
		return nil
	}
	start := sort.Search(len(records), func(i int) bool { return records[i].Principal > s.reconcileAfter })
	var out []ControlMessage
	for i := 0; s.reconcileRemaining > 0 && slots > 0; i++ {
		w := records[(start+i)%len(records)]
		messages, fits := s.recordBounded(w, now, slots)
		if !fits {
			break
		}
		out = append(out, messages...)
		slots -= len(messages)
		s.reconcileRemaining--
		s.reconcileAfter = w.Principal
	}
	return out
}

// Receive returns replies plus independently verified changed records to relay.
// Invalid signatures or lifetimes are fatal except for a bounded retry while
// an exact advertised renewed chain is in flight. Unverified data grants no trust.
func (s *Synchronizer) receive(m ControlMessage, now time.Time) (replies []ControlMessage, changed []SignedManifest, err error) {
	s.prunePendingRenewals(now)
	switch m.Kind {
	case "certificate":
		if m.Certificate == nil {
			return nil, nil, errors.New("missing certificate message")
		}
		if c := m.Certificate.Chain; c != nil {
			if pending, ok := s.pendingRenewals[c.Principal]; ok && !sameCertificateDescription(pending.chain, *c) {
				s.dropPendingManifest(c.Principal)
			}
		}
		replies, completed, err := s.certificates.Receive(*m.Certificate, now)
		return s.certificateResult(replies, completed, err, now)
	case "identity-request":
		chain, _, ok := s.cache.Get(m.Principal, now)
		if ok {
			replies = append(replies, s.identityMessage(m.Principal, chain))
		}
	case "bundle":
		principal, e := s.cache.Put(m.Bundle, now)
		if e != nil {
			return nil, nil, e
		}
		// A warm retry belongs to its advertised certificate exchange, never
		// to an unrelated whole-bundle response for the same principal.
		if w, ok := s.pending[principal]; ok && s.pendingRenewals[principal].chain.Principal == "" {
			delete(s.pending, principal)
			out, changed, err := s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
			if err != nil {
				s.discardManifest(w)
			}
			return out, changed, err
		}
	case "manifest":
		if m.Manifest == nil {
			return nil, nil, errors.New("missing manifest")
		}
		ok, e := s.directory.Accept(*m.Manifest, now)
		if s.gossip != nil && (e == nil || errors.Is(e, ErrStaleManifest)) {
			s.resolveManifest(*m.Manifest, now, e == nil)
		}
		if s.gossip != nil && errors.Is(e, ErrExpiredManifest) {
			s.discardManifest(*m.Manifest)
		}
		// Reordering across relays and leases expiring in transit are normal,
		// not grounds to disconnect an authenticated neighbour.
		if errors.Is(e, ErrStaleManifest) || errors.Is(e, ErrExpiredManifest) {
			return nil, nil, nil
		}
		signatureMismatch := errors.Is(e, ErrInvalidManifestSignature)
		lifetimeMismatch := errors.Is(e, ErrManifestOutlivesCertificate)
		if (signatureMismatch || lifetimeMismatch) && s.deferPendingRenewal(*m.Manifest, now, lifetimeMismatch) {
			return nil, nil, nil
		}
		if errors.Is(e, ErrIdentityNeeded) {
			// Do not disconnect a valid neighbour if many hints are stale after
			// a receiver restart. The request repairs its identity cache, and
			// the periodic reconciliation repeats any manifest we cannot hold.
			if _, exists := s.pending[m.Manifest.Principal]; exists || len(s.pending) < 128 {
				if old, exists := s.pending[m.Manifest.Principal]; exists && !sameManifestWire(old, *m.Manifest) {
					s.discardManifest(old)
				}
				s.pending[m.Manifest.Principal] = cloneSignedManifest(*m.Manifest)
			} else {
				s.discardManifest(*m.Manifest)
			}
			if s.gossip != nil {
				if _, _, active := s.certificates.PendingChain(m.Manifest.Principal, now); active {
					return nil, nil, nil
				}
			}
			return []ControlMessage{{Kind: "identity-request", Principal: m.Manifest.Principal}}, nil, nil
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

func (s *Synchronizer) certificateResult(messages []*CertificateMessage, completed []string, err error, now time.Time) (replies []ControlMessage, changed []SignedManifest, result error) {
	if err != nil {
		return nil, nil, err
	}
	for _, m := range messages {
		replies = append(replies, ControlMessage{Kind: "certificate", Certificate: m})
	}
	// Detach all completed retries before invoking Receive again: Poll can
	// complete several principals at once and removes all their active hints.
	var retry []SignedManifest
	for _, principal := range completed {
		if pending, ok := s.pendingRenewals[principal]; ok {
			delete(s.pendingRenewals, principal)
			chain, _, valid := s.cache.Get(principal, now)
			if !pending.fetchUntil.After(now) || !pending.expires.After(now) || !valid || !matchesCertificateDescription(chain, pending.chain) {
				s.dropPendingManifest(principal)
				continue
			}
		}
		if w, ok := s.pending[principal]; ok {
			delete(s.pending, principal)
			retry = append(retry, w)
		}
	}
	for i, w := range retry {
		out, admitted, err := s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
		if err != nil {
			// These candidates are detached already; Close cannot find them
			// through pending. Release every unprocessed pooled claim too.
			for _, remaining := range retry[i:] {
				s.discardManifest(remaining)
			}
			return nil, nil, err
		}
		replies = append(replies, out...)
		changed = append(changed, admitted...)
	}
	return replies, changed, nil
}

// ContinueCertificates also advances bounded HAVE/WANT replies and downloads.
func (s *Synchronizer) ContinueCertificates(now time.Time, slots ...int) ([]ControlMessage, []SignedManifest, error) {
	s.prunePendingRenewals(now)
	if s.gossip != nil {
		for principal, w := range s.pending {
			var m Manifest
			if json.Unmarshal(w.Body, &m) != nil || !time.UnixMilli(m.Expires).After(now) {
				s.dropPendingManifest(principal)
			}
		}
	}
	messages, completed, err := s.certificates.Poll(now)
	replies, changed, err := s.certificateResult(messages, completed, err, now)
	if s.gossip != nil {
		budget := 12
		if len(slots) > 0 {
			budget = max(0, slots[0]-len(replies))
		}
		for _, m := range s.gossip.Poll(now, budget) {
			replies = append(replies, ControlMessage{Kind: "gossip", Gossip: m})
		}
	}
	return replies, changed, err
}

// deferPendingRenewal retains no authority. A signature mismatch needs a
// different leaf; a lifetime mismatch can also await a renewed issuer behind
// the same leaf. Both require this link's active exact changed descriptor and
// must verify normally after completion.
func (s *Synchronizer) deferPendingRenewal(w SignedManifest, now time.Time, lifetimeMismatch bool) bool {
	description, deadline, ok := s.certificates.PendingChain(w.Principal, now)
	if !ok {
		return false
	}
	// Accept already parsed and validated this body before reporting the
	// renewal mismatch. Bound retained state by that signed expiry too.
	var manifest Manifest
	if json.Unmarshal(w.Body, &manifest) != nil {
		return false
	}
	pending := pendingManifestRenewal{chain: description, expires: minTime(deadline, time.UnixMilli(manifest.Expires)), fetchUntil: deadline}
	chain, _, ok := s.cache.Get(w.Principal, now)
	if !ok {
		return false
	}
	differentLeaf := certificateHash(chain[0]) != pending.chain.Hashes[0]
	if !differentLeaf && (!lifetimeMismatch || matchesCertificateDescription(chain, pending.chain)) {
		return false
	}
	if _, exists := s.pending[w.Principal]; !exists && len(s.pending) >= 128 {
		s.discardManifest(w)
		return true // bounded loss; periodic reconciliation can retry later
	}
	if old, exists := s.pendingRenewals[w.Principal]; exists {
		pending.expires = minTime(pending.expires, old.expires)
		pending.fetchUntil = minTime(pending.fetchUntil, old.fetchUntil)
	}
	if old, exists := s.pending[w.Principal]; exists && !sameManifestWire(old, w) {
		s.discardManifest(old)
	}
	s.pending[w.Principal] = cloneSignedManifest(w)
	s.pendingRenewals[w.Principal] = pending
	return true
}

func (s *Synchronizer) prunePendingRenewals(now time.Time) {
	for principal, pending := range s.pendingRenewals {
		active, _, ok := s.certificates.PendingChain(principal, now)
		if !pending.fetchUntil.After(now) || !pending.expires.After(now) || !ok || !sameCertificateDescription(pending.chain, active) {
			s.dropPendingManifest(principal)
		}
	}
}

func (s *Synchronizer) Close() {
	for principal := range s.pending {
		s.dropPendingManifest(principal)
	}
	s.certificates.Close()
	if s.gossip != nil {
		s.gossip.Close()
	}
}

func sameManifestWire(a, b SignedManifest) bool {
	return a.Principal == b.Principal && bytes.Equal(a.Body, b.Body) && bytes.Equal(a.Signature, b.Signature)
}

func (s *Synchronizer) dropPendingManifest(principal string) {
	if w, ok := s.pending[principal]; ok {
		s.discardManifest(w)
	}
	delete(s.pending, principal)
	delete(s.pendingRenewals, principal)
}
