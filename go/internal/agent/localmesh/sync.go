package localmesh

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const MaxControlMessage = 64 << 10

// Unsolicited/bulk polling leaves these slots for responses which let both
// sides of a slow full-duplex control stream continue making progress.
const directoryResponseReserve = 8

// ErrControlBackpressure consumes no input or transfer state. The actor keeps
// that one ingress message and retries only after output credit becomes free.
var ErrControlBackpressure = errors.New("directory response credit unavailable")

// These are immediate response obligations, not total transfer size. Full
// certificate chains and manifests are received normally; outgoing router
// bytes use bounded descriptors/chunks. Deferred WANT uploads need only their
// descriptor (or unavailable) now and leave DATA for separately credited Poll.
func directoryResponseDemand(m ControlMessage) int {
	switch m.Kind {
	case "gossip":
		if m.Gossip == nil || m.Gossip.FutureType() || !m.Gossip.Known() {
			return 0
		}
		switch m.Gossip.Op {
		case "gone", "unavailable":
			return 0
		case "data":
			if m.Gossip.Offset+len(m.Gossip.Data) < m.Gossip.Total {
				return 0
			}
		}
		return 1
	case "manifest-request":
		return 2
	case "certificate", "identity-request", "manifest", "bundle":
		return 1
	default:
		return 0
	}
}

// Control messages use one bounded reliable stream per authenticated link;
// routing and payload packets remain independent QUIC datagrams.
type ControlMessage struct {
	Kind        string              `json:"kind" cbor:"k"`
	Hello       *LinkHello          `json:"hello,omitempty" cbor:"h,omitempty"`
	Bundle      [][]byte            `json:"bundle,omitempty" cbor:"b,omitempty"`
	Manifest    *SignedManifest     `json:"manifest,omitempty" cbor:"m,omitempty"`
	Principal   string              `json:"principal,omitempty" cbor:"p,omitempty"`
	Certificate *CertificateMessage `json:"certificate,omitempty" cbor:"c,omitempty"`
	Gossip      *netgossip.Message  `json:"gossip,omitempty" cbor:"g,omitempty"`
	Time        *TimestampControl   `json:"time,omitempty" cbor:"t,omitempty"`
	deadline    time.Time           // Local queue ownership only; never serialized.
	canceled    *atomic.Bool
	origin      string // Immutable local export principal/instance; never serialized.
	pendingHave bool   // This carrier still owes the HAVE if identity repair blocks its write.
}

// Deadline is local scheduling metadata for this queued payload. Incoming wire
// bytes carry no deadline; their receiver must establish its own admission.
func (m ControlMessage) Deadline() time.Time {
	deadline := m.deadline
	if m.Gossip != nil {
		if other := m.Gossip.Deadline(); !other.IsZero() && (deadline.IsZero() || other.Before(deadline)) {
			deadline = other
		}
	}
	return deadline
}

func (m ControlMessage) Expired(now time.Time) bool {
	deadline := m.Deadline()
	return m.canceled != nil && m.canceled.Load() || !deadline.IsZero() && !deadline.After(now)
}

func knownControlKind(kind string) bool {
	switch kind {
	case "time", "gossip", "certificate", "hello", "bundle", "manifest", "identity-request", "manifest-request", "identity-missing", "manifest-missing":
		return true
	}
	return false
}

func WriteControl(w io.Writer, m ControlMessage) error {
	if m.Kind == "time" && m.Time != nil && m.Time.Version == 0 {
		timestamp := *m.Time
		timestamp.Version = timestampControlVersion
		m.Time = &timestamp
	}
	data, err := netwire.Marshal(m)
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
	var head struct {
		Kind string             `json:"kind" cbor:"k"`
		Time netwire.RawMessage `json:"time" cbor:"t"`
	}
	if err := netwire.Unmarshal(data, &head); err != nil {
		return m, err
	}
	if head.Kind != "" && !knownControlKind(head.Kind) {
		return ControlMessage{Kind: head.Kind}, nil
	}
	if head.Kind == "time" && len(head.Time) != 0 {
		var timestampVersion struct {
			Version uint32 `json:"v" cbor:"v"`
		}
		if err := netwire.Unmarshal(head.Time, &timestampVersion); err != nil {
			return m, err
		}
		// A future timestamp version owns even the operation's schema.
		if timestampVersion.Version > timestampControlVersion {
			return ControlMessage{Kind: "time", Time: &TimestampControl{Version: timestampVersion.Version}}, nil
		}
		var timestampOp struct {
			Op string `json:"op" cbor:"o"`
		}
		if err := netwire.Unmarshal(head.Time, &timestampOp); err != nil {
			return m, err
		}
		// Unknown operations own their payload schema. Consume this bounded
		// frame without decoding their operation-specific fields.
		if timestampOp.Op != "" && !knownTimestampOp(timestampOp.Op) {
			return ControlMessage{Kind: "time", Time: &TimestampControl{Version: timestampVersion.Version, Op: timestampOp.Op}}, nil
		}
	}
	if err := netwire.Unmarshal(data, &m); err != nil {
		return m, err
	}

	if m.Kind != "time" && m.Time != nil {
		return m, errors.New("unexpected timestamp payload")
	}
	if m.Kind != "certificate" && m.Certificate != nil {
		return m, errors.New("unexpected certificate message")
	}
	if m.Kind != "gossip" && m.Gossip != nil {
		return m, errors.New("unexpected gossip payload")
	}
	switch m.Kind {
	case "time":
		if m.Hello != nil || m.Manifest != nil || m.Certificate != nil || m.Gossip != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid timestamp envelope")
		}
		if err := m.Time.validate(); err != nil {
			return m, err
		}
	case "gossip":
		if m.Gossip != nil && (m.Gossip.FutureType() || m.Gossip.Op != "" && !m.Gossip.Known()) {
			return m, nil
		}
		if m.Gossip == nil || m.Gossip.Type != netgossip.Manifest || m.Hello != nil || m.Manifest != nil || len(m.Bundle) != 0 || m.Principal != "" {
			return m, errors.New("invalid manifest gossip envelope")
		}
		if err := m.Gossip.Validate(); err != nil {
			return m, err
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
	case "identity-request", "manifest-request", "identity-missing", "manifest-missing":
		// Only a full device principal addresses an identity: no hash, no number.
		if _, err := meshid.PeerFromPrincipal(m.Principal); err != nil {
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

// Resource deadlines retain the original receiver monotonic clock component.
// Announcement absolute dates supply duration only, never receiver UTC freshness.
type pendingManifestRenewal struct {
	chain      CertificateChain
	expires    time.Time
	fetchUntil time.Time
}

// A marker owns no artifact bytes. Queued controls share its cancellation bit
// so supersession releases their payloads on the caller's next queue sweep.
type manifestArtifact struct {
	id       netgossip.ID
	deadline time.Time
	canceled *atomic.Bool
}

// Synchronizer is per-link, single-caller, and does not own networking. It
// retains the origins still owed to this carrier, starting with its inventory
// at attach. Bounded chain descriptions replace unsolicited bundles; certificate
// bytes are fetched individually only when missing. A bounded number of inbound
// manifests wait for independently verified identity completion.
type Synchronizer struct {
	gossip     *netgossip.Session
	gossipPool *netgossip.Pool
	directory  *Directory
	cache      *IdentityCache
	// Legacy leaf-fingerprint keys mean known bundles. Gossip keys are full
	// descriptor digests with freshness bounded by the certificate fetch lease.
	sent             map[string]time.Time
	pending          map[string]SignedManifest
	pendingDeadlines map[string]time.Time
	artifacts        map[string]manifestArtifact
	// Warm-cache retries are bound to the exact advertised chain and deadline.
	pendingRenewals      map[string]pendingManifestRenewal
	certificates         *CertificateExchange
	pendingAnnouncements []string // Origin keys, in announcement order.
}

func NewSynchronizer(d *Directory, c *IdentityCache, pools ...*netgossip.Pool) *Synchronizer {
	s := &Synchronizer{directory: d, cache: c, sent: map[string]time.Time{}, pending: map[string]SignedManifest{}, pendingDeadlines: map[string]time.Time{}, artifacts: map[string]manifestArtifact{}, pendingRenewals: map[string]pendingManifestRenewal{}, certificates: NewCertificateExchange(c, d.tenant)}
	if len(pools) > 0 && pools[0] != nil {
		s.gossipPool = pools[0]
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

// seedAnnouncements runs once on attach. Send our directly verifiable proof
// first, ahead of records whose keys the neighbour may need to fetch.
func (s *Synchronizer) seedAnnouncements(own string, now time.Time) {
	d := s.directory
	d.mu.Lock()
	d.expireLocked(now)
	owners := make([]string, 0, len(d.entries))
	_, ownPresent := d.entries[own]
	if ownPresent {
		owners = append(owners, own)
	}
	start := len(owners)
	for owner := range d.entries {
		if owner != own {
			owners = append(owners, owner)
		}
	}
	d.mu.Unlock()
	slices.Sort(owners[start:])
	s.pendingAnnouncements = owners
}

// markAnnouncement records a directory change even if replies or a full
// channel block this carrier. Cancel an older queued artifact immediately.
func (s *Synchronizer) markAnnouncement(w SignedManifest, now time.Time) {
	owner := w.ownerKey()
	s.retireSupersededArtifact(owner, now)
	s.enqueueAnnouncement(owner, now)
}

func (s *Synchronizer) retireSupersededArtifact(owner string, now time.Time) {
	if artifact, tracked := s.artifacts[owner]; tracked {
		current, live := s.directory.manifestRecord(owner, now)
		if !live || manifestGossipID(current) != artifact.id {
			s.retireArtifact(owner, artifact)
		}
	}
}

func (s *Synchronizer) enqueueAnnouncement(owner string, now time.Time) {
	if slices.Contains(s.pendingAnnouncements, owner) {
		return
	}
	d := s.directory
	if len(s.pendingAnnouncements) >= d.limit {
		// A stalled carrier can outlive directory entries. Reclaim their
		// keys before growing past the directory's live-origin limit.
		d.mu.Lock()
		d.expireLocked(now)
		s.pendingAnnouncements = slices.DeleteFunc(s.pendingAnnouncements, func(key string) bool {
			_, present := d.entries[key]
			return !present
		})
		d.mu.Unlock()
		if len(s.pendingAnnouncements) >= d.limit {
			return
		}
	}
	s.pendingAnnouncements = append(s.pendingAnnouncements, owner)
}

// Only requeue a skipped HAVE for the exact artifact still tracked by this
// carrier. A newer publication may already have queued its replacement.
func (s *Synchronizer) retryAnnouncement(owner string, marker *atomic.Bool, now time.Time) {
	if artifact, tracked := s.artifacts[owner]; tracked && artifact.canceled == marker && artifact.deadline.After(now) {
		s.enqueueAnnouncement(owner, now)
	}
}

func (s *Synchronizer) Record(w SignedManifest, now time.Time) []ControlMessage {
	messages, _ := s.recordBounded(w, now, 2)
	return messages
}

type announcementResult uint8

const (
	announcementDone announcementResult = iota
	announcementNoCredit
	announcementRetry
)

// The bundle hint is consumed only if the complete record fits. A caller must
// reserve this many queue slots before sending the returned messages.
func (s *Synchronizer) recordBounded(w SignedManifest, now time.Time, slots int) ([]ControlMessage, announcementResult) {
	if slots < 1 {
		return nil, announcementNoCredit
	}
	deadline, ok := s.directory.RecordDeadline(w, now)
	if !ok {
		// An exact retained body can become exportable again after identity
		// repair. Keep its marker for any notification from the writer.
		if current, live := s.directory.manifestRecord(w.ownerKey(), now); live && sameManifestWire(current, w) {
			return nil, announcementRetry
		}
		s.forgetManifest(w)
		return nil, announcementDone
	}
	if s.gossip != nil {
		return s.offerManifest(w, deadline, now), announcementDone
	}
	chain, ok := s.directory.CurrentRecordChain(w, now)
	if !ok {
		return nil, announcementRetry
	}
	s.pruneSupersededPending(now)
	artifact := s.trackArtifact(w, deadline)
	fp := Fingerprint(chain)
	_, known := s.sent[fp]
	bundleNeeded := !known
	needed := 1
	if bundleNeeded {
		needed++
	}
	if needed > slots {
		return nil, announcementNoCredit
	}
	var out []ControlMessage
	if bundleNeeded {
		if len(s.sent) >= 1024 {
			s.sent = map[string]time.Time{}
		}
		s.sent[fp] = time.Time{}
		identity := s.identityMessage(w.Principal, chain)
		identity.deadline, identity.canceled = deadline, artifact.canceled
		out = append(out, identity)
	}
	w.Body = append([]byte(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	return append(out, ControlMessage{Kind: "manifest", Manifest: &w, deadline: deadline, canceled: artifact.canceled}), announcementDone
}

// identityMessage is called only with a current independently verified cache
// entry. Router peers fetch public certificate bytes through bounded chunks.
func (s *Synchronizer) identityMessage(principal string, chain [][]byte) ControlMessage {
	return ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(principal, chain)}
}

// drainAnnouncements reads the latest revision for each pending origin. A
// missing signing identity leaves a live origin pending while others progress.
func (s *Synchronizer) drainAnnouncements(now time.Time, slots int) []ControlMessage {
	if slots <= 0 {
		return nil
	}
	var out []ControlMessage
	pending := s.pendingAnnouncements[:0]
	for i, owner := range s.pendingAnnouncements {
		if slots <= 0 {
			pending = append(pending, s.pendingAnnouncements[i:]...)
			break
		}
		w, live := s.directory.manifestRecord(owner, now)
		if !live {
			continue
		}
		messages, result := s.recordBounded(w, now, slots)
		if result == announcementNoCredit {
			pending = append(pending, s.pendingAnnouncements[i:]...)
			break
		}
		if result == announcementRetry {
			pending = append(pending, owner)
			continue
		}
		if s.gossip != nil && len(messages) != 0 {
			messages[0].pendingHave = true
		}
		out = append(out, messages...)
		slots -= len(messages)
	}
	clear(s.pendingAnnouncements[len(pending):])
	s.pendingAnnouncements = pending
	return out
}

// Availability is only reported for device principals in this directory's tenant.
func (s *Synchronizer) localDevicePrincipal(principal string) bool {
	device, err := meshid.PeerFromPrincipal(principal)
	return err == nil && device.Tenant() == s.directory.tenant
}

// Receive returns replies plus independently verified changed records to relay.
// Invalid signatures or lifetimes are fatal except for a bounded retry while
// an exact advertised renewed chain is in flight, or a signature or issue-date
// mismatch on a revision the origin has already replaced. Unverified data
// grants no trust.
func (s *Synchronizer) receive(m ControlMessage, now time.Time, slots ...int) (replies []ControlMessage, changed []SignedManifest, err error) {
	s.Sweep(now)
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
		missing := s.certificates.MissingPrincipals(m.Certificate.Missing)
		replies, completed, err := s.certificates.Receive(*m.Certificate, now, slots...)
		if err == nil {
			for _, principal := range missing {
				s.dropPendingPrincipal(principal)
			}
		}
		return s.certificateResult(replies, completed, err, now)
	case "identity-missing":
		// This synchronizer owns only this neighbour's outstanding claims.
		// Forget its unverified download without creating a shared receipt,
		// allowing a different announcing supplier to take over immediately.
		s.dropPendingPrincipal(m.Principal)
		s.certificates.forgetPrincipal(m.Principal, now)
	case "manifest-missing":
		// Go does not originate principal-based manifest lookups. There is no
		// outstanding request here to cancel; consume the availability reply.
	case "manifest-request":
		// Constrained peers can retain verified capabilities independently
		// of routing/key caches. Answer misses through ordinary HAVE/WANT,
		// preserving the existing bounded transfer and verification path.
		available := 2
		if len(slots) > 0 {
			available = slots[0]
		}
		found := false
		for _, w := range s.directory.Records(now) {
			if w.Principal == m.Principal {
				found = true
				replies, _ = s.recordBounded(w, now, available)
				break
			}
		}
		if !found && s.localDevicePrincipal(m.Principal) {
			replies = append(replies, ControlMessage{Kind: "manifest-missing", Principal: m.Principal})
		}
	case "identity-request":
		chain, _, ok := s.cache.Get(m.Principal, now)
		if ok {
			replies = append(replies, s.identityMessage(m.Principal, chain))
		} else if s.localDevicePrincipal(m.Principal) {
			replies = append(replies, ControlMessage{Kind: "identity-missing", Principal: m.Principal})
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
			delete(s.pendingDeadlines, principal)
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
		if errors.Is(e, ErrUnsupportedManifest) {
			// Future artifacts are unsupported claims, not broken carriers.
			// Discard their fetch slot without installing routes/replay state.
			s.discardManifest(*m.Manifest)
			return nil, nil, nil
		}
		if s.gossip != nil && (e == nil || errors.Is(e, ErrStaleManifest)) {
			s.resolveManifest(*m.Manifest, now, e == nil)
		}
		// A superseded envelope was never authenticated: release its claim
		// like an expired one, without the receipt a verified stale body gets.
		unverified := errors.Is(e, ErrExpiredManifest) || errors.Is(e, ErrSupersededManifest)
		if s.gossip != nil && unverified {
			s.discardManifest(*m.Manifest)
		}
		// Reordering across relays and leases expiring in transit are normal,
		// not grounds to disconnect an authenticated neighbour.
		if errors.Is(e, ErrStaleManifest) || unverified {
			return nil, nil, nil
		}
		signatureMismatch := errors.Is(e, ErrInvalidManifestSignature)
		lifetimeMismatch := errors.Is(e, ErrManifestOutlivesCertificate)
		if (signatureMismatch || lifetimeMismatch) && s.deferPendingRenewal(*m.Manifest, now, lifetimeMismatch) {
			return nil, nil, nil
		}
		if errors.Is(e, ErrIdentityNeeded) {
			// Do not disconnect a valid neighbour if many hints are stale after
			// a receiver restart. The request repairs its identity cache for
			// retained manifests and future origin publications.
			if _, exists := s.pending[m.Manifest.ownerKey()]; exists || len(s.pending) < 128 {
				if old, exists := s.pending[m.Manifest.ownerKey()]; exists && !sameManifestWire(old, *m.Manifest) {
					s.discardManifest(old)
				}
				s.retainPending(*m.Manifest, now)
			} else {
				s.discardManifest(*m.Manifest)
			}
			if body, err := DecodeManifest(m.Manifest.Body); err == nil && body.Owner != nil {
				description := CertificateChain{Principal: m.Manifest.Principal, Hashes: make([]string, len(body.Owner.Hashes))}
				for i, hash := range body.Owner.Hashes {
					description.Hashes[i] = hex.EncodeToString(hash)
				}
				messages, completed, fetchErr := s.certificates.Receive(CertificateMessage{Chain: &description}, now, slots...)
				return s.certificateResult(messages, completed, fetchErr, now)
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
		s.pruneSupersededPending(now)
		if ok {
			changed = append(changed, *m.Manifest)
		}
	default:
		if m.Kind != "" && !knownControlKind(m.Kind) {
			return nil, nil, nil
		}
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
		for key, w := range s.pending {
			if w.Principal != principal {
				continue
			}
			if pending, ok := s.pendingRenewals[key]; ok {
				delete(s.pendingRenewals, key)
				chain, _, valid := s.cache.Get(principal, now)
				if !pending.fetchUntil.After(now) || !pending.expires.After(now) || !valid || !matchesCertificateDescription(chain, pending.chain) {
					s.dropPendingManifest(key)
					continue
				}
			}
			delete(s.pending, key)
			delete(s.pendingDeadlines, key)
			retry = append(retry, w)
		}
	}
	for i, w := range retry {
		out, admitted, err := s.receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
		changed = append(changed, admitted...)
		if err != nil {
			// These candidates are detached already; Close cannot find them
			// through pending. Release every unprocessed pooled claim too.
			for _, remaining := range retry[i:] {
				s.discardManifest(remaining)
			}
			return nil, changed, err
		}
		replies = append(replies, out...)
	}
	return replies, changed, nil
}

// ContinueCertificates also advances bounded HAVE/WANT replies and downloads.
func (s *Synchronizer) ContinueCertificates(now time.Time, slots ...int) ([]ControlMessage, []SignedManifest, error) {
	s.Sweep(now)
	certificateBudget := slots
	if len(slots) > 0 {
		certificateBudget = []int{max(0, slots[0]-directoryResponseReserve)}
	}
	messages, completed, err := s.certificates.Poll(now, certificateBudget...)
	replies, changed, err := s.certificateResult(messages, completed, err, now)
	if err != nil {
		return nil, changed, err
	}
	if s.gossip != nil {
		budget := 12
		if len(slots) > 0 {
			budget = max(0, slots[0]-len(replies)-directoryResponseReserve)
		}
		for _, m := range s.gossip.Poll(now, budget) {
			replies = append(replies, s.gossipControl(m))
		}
	}
	return replies, changed, nil
}

// deferPendingRenewal retains no authority. A signature mismatch needs a
// different leaf; a lifetime mismatch can also await a renewed issuer behind
// the same leaf. Both require this link's active exact changed descriptor and
// must verify normally after completion.
func (s *Synchronizer) deferPendingRenewal(w SignedManifest, now time.Time, lifetimeMismatch bool) bool {
	if identity, err := meshid.PeerFromPrincipal(w.Principal); err == nil && identity.IsOperator() {
		return false
	}
	description, deadline, ok := s.certificates.PendingChain(w.Principal, now)
	if !ok {
		return false
	}
	// Accept already parsed and validated this body before reporting the
	// renewal mismatch. Bound untrusted bytes by that signed duration too.
	manifest, decodeErr := DecodeManifest(w.Body)
	if decodeErr != nil {
		return false
	}
	duration := time.UnixMilli(manifest.Expires).Sub(time.UnixMilli(manifest.Issued))
	if duration <= 0 || duration > MaxLease {
		return false
	}
	pending := pendingManifestRenewal{chain: description, expires: minTime(deadline, now.Add(duration)), fetchUntil: deadline}
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
		return true // bounded loss; a later origin publication can retry
	}
	if old, exists := s.pendingRenewals[w.Principal]; exists {
		pending.expires = minTime(pending.expires, old.expires)
		pending.fetchUntil = minTime(pending.fetchUntil, old.fetchUntil)
	}
	if old, exists := s.pending[w.Principal]; exists && !sameManifestWire(old, w) {
		s.discardManifest(old)
	}
	s.retainPending(w, now)
	pending.expires = minTime(pending.expires, s.pendingDeadlines[w.ownerKey()])
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

// retainPending owns untrusted bytes only for one fixed receive-local resource
// allowance. Repeated bodies, changed signatures and hints cannot renew it.
func (s *Synchronizer) retainPending(w SignedManifest, now time.Time) {
	key := w.ownerKey()
	if s.pendingDeadlines == nil {
		s.pendingDeadlines = make(map[string]time.Time)
	}
	if _, exists := s.pendingDeadlines[key]; !exists {
		duration := MaxLease
		if m, err := DecodeManifest(w.Body); err == nil {
			if candidate := time.UnixMilli(m.Expires).Sub(time.UnixMilli(m.Issued)); candidate > 0 && candidate <= MaxLease {
				duration = candidate
			}
		}
		deadline := now.Add(duration)
		if s.gossip != nil {
			data, _ := netwire.Marshal(w)
			if claim, ok := s.gossip.ClaimDeadline(netgossip.Identify(netgossip.Manifest, data)); ok {
				deadline = minTime(deadline, claim)
			}
		}
		s.pendingDeadlines[key] = deadline
	}
	if old, exists := s.pending[key]; exists && sameManifestWire(old, w) {
		return
	}
	s.pending[key] = cloneSignedManifest(w)
}

// Sweep physically releases buffered announcements and their unfinished repair
// work while idle. It starts no transfers and performs no certificate admission.
func (s *Synchronizer) Sweep(now time.Time) {
	for owner, artifact := range s.artifacts {
		if !artifact.deadline.After(now) {
			s.retireArtifact(owner, artifact)
		}
	}
	if s.gossip != nil {
		s.gossip.Sweep(now)
	}
	if s.certificates == nil {
		return
	}
	s.pruneSupersededPending(now)
	for key, until := range s.pendingDeadlines {
		if !until.After(now) {
			principal := s.pending[key].Principal
			s.dropPendingManifest(key)
			needed := false
			for _, other := range s.pending {
				needed = needed || other.Principal == principal
			}
			if !needed {
				s.certificates.forgetPrincipal(principal, now)
			}
		}
	}
	s.prunePendingRenewals(now)
	for key, pending := range s.certificates.pending {
		if !pending.expires.After(now) {
			s.certificates.forgetChain(key, now)
		}
	}
	parts := s.cache.parts
	parts.mu.Lock()
	parts.prune(now)
	parts.mu.Unlock()
}

// Supersession can arrive through another link using this shared directory.
// The retained current record/watermark, never an unverified incoming revision,
// decides whether an old buffered payload has any remaining admission work.
func (s *Synchronizer) pruneSupersededPending(now time.Time) {
	if s.directory == nil || len(s.pending) == 0 {
		return
	}
	d := s.directory
	d.mu.Lock()
	d.expireLocked(now)
	var obsolete []string
	for key, w := range s.pending {
		m, err := DecodeManifest(w.Body)
		if err != nil {
			continue
		}
		current, active := d.entries[key]
		receipt, seen := d.history[key]
		superseded := seen && (m.Revision < receipt.Revision || m.Revision == receipt.Revision && !active)
		if !superseded && active && m.Revision <= current.manifest.Revision {
			// A transient identity-cache miss still needs its existing repair
			// path. Do not discard that retry merely because bytes are cached.
			chain, verified := d.currentEntryChain(key, current, now)
			superseded = verified
			if verified && m.Revision == current.manifest.Revision && manifestHash(w.Body) == current.hash {
				// A current old-key signature does not supersede an exact
				// same-body re-sign awaiting its advertised replacement chain.
				// Retain only that original bounded renewal continuation.
				if renewal, waiting := s.pendingRenewals[key]; waiting && renewal.fetchUntil.After(now) && renewal.expires.After(now) && s.certificates != nil {
					active, _, fetching := s.certificates.PendingChain(w.Principal, now)
					if fetching && sameCertificateDescription(renewal.chain, active) && !matchesCertificateDescription(chain, renewal.chain) {
						superseded = false
					}
				}
			}
		}
		if superseded {
			obsolete = append(obsolete, key)
		}
	}
	d.mu.Unlock()
	// Release shared transfer claims only after dropping the directory lock.
	for _, key := range obsolete {
		w := s.pending[key]
		s.dropPendingManifest(key)
		needed := false
		for _, other := range s.pending {
			needed = needed || other.Principal == w.Principal
		}
		if !needed && s.certificates != nil {
			s.certificates.forgetPrincipal(w.Principal, now)
		}
	}
}

func (s *Synchronizer) Close() {
	for owner, artifact := range s.artifacts {
		artifact.canceled.Store(true)
		delete(s.artifacts, owner)
	}
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
	delete(s.pendingDeadlines, principal)
	delete(s.pendingRenewals, principal)
}

func (s *Synchronizer) dropPendingPrincipal(principal string) {
	for key, w := range s.pending {
		if w.Principal == principal {
			s.dropPendingManifest(key)
		}
	}
}
