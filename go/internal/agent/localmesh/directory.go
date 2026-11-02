package localmesh

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	MaxManifestBytes = 32768
	// Bound supports both ML-DSA-65 and ML-DSA-87 origin proofs.
	MaxManifestSignatureBytes = 8192
	MaxLease                  = 15 * time.Minute
	manifestDomain            = "wendy-local-mesh/manifest/4\x00"
	// ManifestVersionRoute binds the full principal and exact route origin to
	// the signed device lease. Earlier versions are rejected without fallback.
	ManifestVersionRoute = 4
)

// Manifest is a device's own signed advertisement. Version 4 authorizes one exact
// route origin for its lease and includes the full principal in the signed input.
type Manifest struct {
	Version int            `json:"version" cbor:"v"`
	Device  uuid.UUID      `json:"device" cbor:"d"`
	Owner   *OperatorOwner `json:"owner,omitempty" cbor:"o,omitempty"`
	// Revision is a persistently increasing origin counter, including restart.
	Revision       uint64 `json:"revision" cbor:"R"`
	Issued         int64  `json:"Issued" cbor:"i"`
	Expires        int64  `json:"Expires" cbor:"e"`
	Name           string `json:"name" cbor:"n"`
	AgentPort      uint16 `json:"agentPort" cbor:"P"`
	AgentTransport string `json:"agentTransport,omitempty" cbor:"T,omitempty"`
	// Native agent discovery travels with the signed router lease, without
	// a separate service publication or catalogue inventory refresh.
	Platform     string            `json:"platform,omitempty" cbor:"l,omitempty"`
	Caps         []string          `json:"caps,omitzero" cbor:"c,omitzero"`
	Requires     []string          `json:"requires,omitempty" cbor:"q,omitempty"`
	Withdraw     bool              `json:"withdraw,omitempty" cbor:"w,omitempty"`
	Route        *RouteOrigin      `json:"route,omitempty" cbor:"r,omitempty"`
	AppEndpoints []AppEndpoint     `json:"appEndpoints" cbor:"a"`
	Services     []ManifestService `json:"services" cbor:"s"`
	// AdmittedUntil is a receiver-local lease projection, never signed or sent.
	// The immutable Issued/Expires pair supplies only the bounded duration.
	AdmittedUntil time.Time `json:"-" cbor:"-"`
}

// LeaseDeadline is the receiver's admitted deadline. The UTC fallback supports
// snapshots built by legacy callers; running Nodes always populate AdmittedUntil.
func (m Manifest) LeaseDeadline() time.Time {
	if !m.AdmittedUntil.IsZero() {
		return m.AdmittedUntil
	}
	return time.UnixMilli(m.Expires)
}

func (m Manifest) LeaseRemaining(now time.Time) time.Duration {
	return max(0, m.LeaseDeadline().Sub(now))
}

// SignedManifest is a manifest plus the full device principal of the identity
// that signed it. The principal is both the lookup key and part of the signature
// preimage; the manifest body carries the UUID, and Accept requires them to agree.
type SignedManifest struct {
	Principal string `json:"principal" cbor:"p"`
	Body      []byte `json:"body" cbor:"b"`
	Signature []byte `json:"signature" cbor:"s"`

	envelope *manifestEnvelope
}

var ErrExpiredManifest = errors.New("expired wendy net manifest")
var ErrStaleManifest = errors.New("stale wendy net manifest")

// ErrInvalidManifestSignature distinguishes a failed signature from malformed
// or unauthorized manifests; certificate renewal may need an in-flight leaf.
var ErrInvalidManifestSignature = errors.New("invalid manifest signature")

// ErrManifestOutlivesCertificate can await an explicitly advertised renewed
// leaf, but normal admission still enforces the verified replacement lifetime.
var ErrManifestOutlivesCertificate = errors.New("manifest outlives certificate")

func (m Manifest) validate(_ time.Time) error {
	if m.Version != ManifestVersionRoute {
		return &UnsupportedManifestError{Version: m.Version}
	}
	if err := validateManifestRequirements(m.Requires); err != nil {
		return err
	}
	issued, expires := time.UnixMilli(m.Issued), time.UnixMilli(m.Expires)
	if m.Revision == 0 ||
		!expires.After(issued) || expires.Sub(issued) > MaxLease ||
		len(m.Name) > 255 || (!m.Withdraw && m.AgentPort == 0 && m.Owner == nil) {
		return errors.New("invalid or expired wendy net manifest")
	}
	if m.Owner != nil && (m.AgentPort != 0 || m.AgentTransport != "" || m.Platform != "" || len(m.Caps) != 0 || len(m.AppEndpoints) != 0 || len(m.Services) != 0) {
		return errors.New("operator origin does not advertise a device agent")
	}
	if m.Route == nil {
		return errors.New("manifest version does not match route authorization")
	}
	if len(m.AgentTransport) > 64 {
		return errors.New("agent transport exceeds bound")
	}
	if m.AgentTransport != "" && m.AgentPort == 0 {
		return errors.New("invalid agent service transport")
	}
	if err := validateNativeAgentMetadata(m.Platform, m.Caps); err != nil {
		return err
	}
	if err := validateServices(m.Services); err != nil {
		return err
	}
	if err := validateAppEndpoints(m.AppEndpoints); err != nil {
		return err
	}
	return nil
}

func signingInput(key crypto.PublicKey, principal string, body []byte) ([]byte, crypto.SignerOpts) {
	// Callers derive or validate the canonical full principal before signing.
	// The length prefix binds its boundary independently of the encoded body.
	input := make([]byte, 0, len(manifestDomain)+4+len(principal)+len(body))
	input = append(input, manifestDomain...)
	input = binary.BigEndian.AppendUint32(input, uint32(len(principal)))
	input = append(input, principal...)
	input = append(input, body...)
	switch key.(type) {
	case ed25519.PublicKey, *mldsa.PublicKey:
		return input, crypto.Hash(0)
	}
	h := sha256.Sum256(input)
	return h[:], crypto.SHA256
}

// SignManifest signs an advertisement with the provisioned device identity and
// records that identity's principal. The manifest's device UUID must be the
// signing leaf's own; an advertisement for another device is refused here rather
// than discovered later by a receiver.
func SignManifest(m Manifest, chain [][]byte, signer crypto.Signer, now time.Time) (SignedManifest, error) {
	var out SignedManifest
	m = cloneManifest(m)
	// Local publishers advertise only transports this implementation serves.
	if m.AgentTransport != "" && m.AgentTransport != AgentMTLS {
		return out, &UnsupportedManifestError{Version: m.Version, Transport: m.AgentTransport}
	}
	if m.Owner != nil {
		m.Owner.Hashes = make([][]byte, len(chain))
		for i, der := range chain {
			hash := sha256.Sum256(der)
			m.Owner.Hashes[i] = append([]byte(nil), hash[:]...)
		}
	}
	sort.Strings(m.Caps)
	sort.Strings(m.Requires)
	if err := m.validate(now); err != nil {
		return out, err
	}
	if signer == nil || len(chain) == 0 {
		return out, errors.New("missing manifest signer")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return out, err
	}
	identity, err := meshid.PeerFromCert(leaf)
	if err != nil {
		return out, err
	}
	device, err := m.Peer(identity.Tenant())
	if err != nil || device.Principal() != identity.Principal() {
		return out, errors.New("manifest identity mismatch")
	}
	if err := m.validateRoute(device); err != nil {
		return out, err
	}
	out.Principal = device.Principal()
	body, err := netwire.Marshal(m)
	if err != nil {
		return out, err
	}
	if len(body) > MaxManifestBytes {
		return out, errors.New("oversized manifest")
	}
	input, opts := signingInput(signer.Public(), out.Principal, body)
	sig, err := signer.Sign(rand.Reader, input, opts)
	if err != nil {
		return out, err
	}
	out.Body, out.Signature = body, sig
	return out, nil
}

// ErrIdentityNeeded lets the synchronizer request an independently validated
// bundle from its neighbour. Unverified announcements are not retained here.
var ErrIdentityNeeded = errors.New("wendy net identity bundle needed")

type directoryEntry struct {
	manifest  Manifest
	wire      SignedManifest
	hash      string
	signature manifestSignatureBinding
	// retainUntil mirrors the compact ten-duration watermark deadline; it
	// does not retain this payload past activeUntil.
	retainUntil time.Time
	// activeUntil retains now's monotonic clock reading when Accept receives one.
	// A wall-clock rollback must not extend a lease already accepted in-process.
	activeUntil         time.Time
	certificateDeadline time.Time // Original selected certificate-path cap in the receiver lease clock.
	certificateNotAfter time.Time // Original selected path, never replaced by a renewed path.
	signingNotBefore    time.Time
	expired             bool
}

type Directory struct {
	mu       sync.Mutex
	tenant   meshid.Tenant
	limit    int
	cache    *IdentityCache
	entries  map[string]directoryEntry
	handoffs map[string]routeHandoff
	history  map[string]DirectoryReceipt
	persist  func([]DirectoryReceipt) error
	// Runtime Nodes provide a monotonic clock independently of certificate UTC.
	// nil preserves the explicit time argument used by synthetic legacy callers.
	monotonicNow        func() time.Time
	certificateNow      func() time.Time // Local certificate BaseTime; nil preserves the verifier's supplied-now contract.
	certificateLeafTime func(time.Time, time.Time) time.Time
	// Keep observed wall time after replay tombstones are pruned. Otherwise a
	// later clock rollback could make a discarded signed lease appear current.
	wallHighWater time.Time
}

// DirectoryReceipt is compact revision/hash metadata, never cached presence.
// Runtime Until is a receiver-monotonic watermark deadline. The legacy persisted
// shape remains readable, but is not imported as a receive lease or watermark.
type DirectoryReceipt struct {
	Device   string    `json:"device" cbor:"d"` // full device principal
	Revision uint64    `json:"revision" cbor:"R"`
	Hash     string    `json:"hash" cbor:"h"`
	Until    time.Time `json:"until" cbor:"u"`
}

func (d *Directory) SetPersistence(receipts []DirectoryReceipt, persist func([]DirectoryReceipt) error, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if persist == nil {
		return errors.New("invalid directory persistence")
	}
	// Legacy persisted receipts are validated for format/scope only. The new
	// receive-lease watermark is RAM-local and never restores foreign presence
	// or an old monotonic deadline across a reboot.
	for _, r := range receipts {
		if len(r.Device) > maxPrincipalBytes+37 || r.Revision == 0 || len(r.Hash) != 64 {
			return errors.New("invalid directory receipt")
		}
		device, err := meshid.PeerFromKey(r.Device)
		if err != nil || device.Tenant() != d.tenant {
			return errors.New("invalid directory receipt")
		}
	}
	d.history = map[string]DirectoryReceipt{}
	d.persist = persist
	return nil
}

// NewDirectory scopes a directory to one tenant: the tenant half of every
// principal it will accept.
func NewDirectory(tenant meshid.Tenant, limit int, cache *IdentityCache) (*Directory, error) {
	return NewDirectoryWithClock(tenant, limit, cache, nil)
}

// NewDirectoryWithClock gives runtime leases their own monotonic time source.
// Tests may inject elapsed time without changing the host wall clock.
func NewDirectoryWithClock(tenant meshid.Tenant, limit int, cache *IdentityCache, monotonicNow func() time.Time) (*Directory, error) {
	if tenant == (meshid.Tenant{}) || limit < 1 || cache == nil {
		return nil, errors.New("invalid directory configuration")
	}
	return &Directory{tenant: tenant, limit: limit, cache: cache, entries: map[string]directoryEntry{}, monotonicNow: monotonicNow}, nil
}

func (d *Directory) leaseNow(now time.Time) time.Time {
	if d.monotonicNow != nil {
		return d.monotonicNow()
	}
	return now
}

// Accept validates the advertising device's own verified identity, never the
// relaying neighbour: a manifest is admitted because its signer is the device
// named in it, and that device was admitted by the authenticated link. Return
// true for a new version or a replacement of an invalid old signature by a
// current key. Duplicate delivery and re-signing never renew the receiver-local lease.
func (d *Directory) Accept(w SignedManifest, now time.Time) (bool, error) {
	if len(w.Body) > MaxManifestBytes || len(w.Signature) > MaxManifestSignatureBytes || w.Principal == "" || len(w.Principal) > maxPrincipalBytes {
		return false, errors.New("oversized manifest envelope")
	}
	identity, err := meshid.PeerFromPrincipal(w.Principal)
	if err != nil {
		return false, err
	}
	if w.Principal != identity.Principal() {
		return false, errors.New("manifest principal is not canonical")
	}
	if identity.Tenant() != d.tenant {
		return false, errors.New("manifest belongs to another tenant")
	}
	m, err := DecodeManifest(w.Body)
	if err != nil {
		return false, err
	}
	if err := m.validate(now); err != nil {
		return false, err
	}
	device, err := m.Peer(identity.Tenant())
	if err != nil || device.Principal() != w.Principal {
		return false, errors.New("manifest owner mismatch")
	}
	owner := device.Key()
	chain, id, ok := d.cache.manifestIdentity(w.Principal, m, now)
	if !ok {
		return false, ErrIdentityNeeded
	}
	if id.Device.Principal() != device.Principal() {
		return false, errors.New("manifest identity mismatch")
	}
	if err := m.validateRoute(device); err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return false, err
	}
	// The selected path is independently trusted at the local verification
	// clock. Static signed dates belong to the signing leaf's window; a newer
	// issuer's NotBefore does not date an already-signed announcement.
	if time.UnixMilli(m.Issued).Before(leaf.NotBefore) {
		return false, errors.New("manifest predates authenticating certificate")
	}
	if time.UnixMilli(m.Expires).After(leaf.NotAfter) || time.UnixMilli(m.Expires).After(id.NotAfter) {
		return false, ErrManifestOutlivesCertificate
	}
	if !validManifestSignature(w, leaf) {
		return false, ErrInvalidManifestSignature
	}
	binding := manifestSignatureBinding{leafHash: certificateHash(chain[0]), valid: true}
	hash := manifestHash(w.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	if d.history == nil {
		d.history = map[string]DirectoryReceipt{}
	}
	if receipt, ok := d.history[owner]; ok {
		if m.Revision < receipt.Revision {
			return false, ErrStaleManifest
		}
		if m.Revision == receipt.Revision {
			if hash != receipt.Hash {
				return false, errors.New("conflicting manifest revision")
			}
			// Compact history survives payload expiry and reconnect. An
			// already-expunged body cannot establish another local lease.
			if _, active := d.entries[owner]; !active {
				return false, ErrStaleManifest
			}
		}
	}
	if old, exists := d.entries[owner]; exists {
		if m.Revision < old.manifest.Revision {
			return false, ErrStaleManifest
		}
		if m.Revision == old.manifest.Revision {
			if hash != old.hash {
				return false, errors.New("conflicting manifest revision")
			}
			// An identical body can be re-signed by a renewed key. Replace only
			// an old signature that no longer verifies with the current leaf;
			// do not renew the lease, retention or durable replay receipt.
			if !old.signature.matches(binding.leafHash, func() bool { return validManifestSignature(old.wire, leaf) }) {
				delete(d.handoffs, owner)
				old.wire = cloneSignedManifest(w)
				old.signature = binding
				d.entries[owner] = old
				return true, nil
			}
			d.entries[owner] = old
			return false, nil
		}
	} else if len(d.entries) >= d.limit {
		return false, errors.New("directory capacity reached")
	}
	w = cloneSignedManifest(w)
	// Compact watermarks grow independently of the active payload budget.
	// They contain no wire body, signature or certificate. Their receive-local
	// retention is exactly ten times this newly admitted signed duration.
	duration := time.UnixMilli(m.Expires).Sub(time.UnixMilli(m.Issued))
	admittedAt := d.leaseNow(now)
	certificateDeadline := admittedAt.Add(id.NotAfter.Sub(d.certificateTime(now)))
	receipt := DirectoryReceipt{Device: owner, Revision: m.Revision, Hash: hash, Until: admittedAt.Add(10 * duration)}
	d.replaceRouteHandoff(owner, m, leaf, now)
	d.history[owner] = receipt
	// Do not force dynamic RAM watermarks into the legacy bounded persistent
	// state schema. Origin publishers retain their own durable revisions.
	d.entries[owner] = directoryEntry{
		manifest: m, wire: w, hash: hash, signature: binding,
		retainUntil: receipt.Until,
		activeUntil: admittedAt.Add(duration), certificateDeadline: certificateDeadline, certificateNotAfter: id.NotAfter, signingNotBefore: leaf.NotBefore,
	}
	return true, nil
}

// validManifestSignature is shared by admission and renewal revalidation so
// both always use the same signature preimage and supported key algorithms.
func validManifestSignature(w SignedManifest, leaf *x509.Certificate) bool {
	input, _ := signingInput(leaf.PublicKey, w.Principal, w.Body)
	switch key := leaf.PublicKey.(type) {
	case *mldsa.PublicKey:
		return mldsa.Verify(key, input, w.Signature, nil) == nil
	case ed25519.PublicKey:
		return ed25519.Verify(key, input, w.Signature)
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(key, input, w.Signature)
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, input, w.Signature) == nil
	}
	return false
}

// A directory entry owns immutable wire bytes. Its bounded memo applies only
// to that exact signature and leaf. Current chain trust and expiry are checked
// before reuse; a renewed leaf needs one signature check, even for the same key.
type manifestSignatureBinding struct {
	leafHash string
	valid    bool
}

func (b *manifestSignatureBinding) matches(leafHash string, verify func() bool) bool {
	if b.leafHash != leafHash {
		b.leafHash, b.valid = leafHash, verify()
	}
	return b.valid
}

func cloneSignedManifest(w SignedManifest) SignedManifest {
	w.Body = append([]byte(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	return w
}

// CurrentRecordChain returns an owned current trusted chain only for the exact
// latest admitted wire (including its signature), while its local lease and
// current certificate lifetime still permit export. It includes withdrawals.
// The chain is the one used for signature validation, so callers must use this
// result for a paired descriptor instead of looking the principal up again.
// It is a current validity snapshot, not a reservation against later renewal.
func (d *Directory) CurrentRecordChain(w SignedManifest, now time.Time) ([][]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	owner := w.ownerKey()
	e, ok := d.entries[owner]
	if !ok || !bytes.Equal(w.Body, e.wire.Body) || !bytes.Equal(w.Signature, e.wire.Signature) {
		return nil, false
	}
	return d.currentEntryChain(owner, e, now)
}

// currentEntryChain requires d.mu and updates only the entry's signature memo.
// It never removes a replay floor, extends a lease, or renews retention.
func (d *Directory) currentEntryChain(principal string, e directoryEntry, now time.Time) ([][]byte, bool) {
	chain, _, ok := d.currentEntryIdentity(principal, e, now)
	return chain, ok
}

func (d *Directory) currentEntryIdentity(principal string, e directoryEntry, now time.Time) ([][]byte, Identity, bool) {
	if !e.activeUntil.After(d.leaseNow(now)) {
		return nil, Identity{}, false
	}
	chain, id, ok := d.cache.manifestIdentity(e.wire.Principal, e.manifest, now)
	if !ok || id.Device.Principal() != e.wire.Principal || time.UnixMilli(e.manifest.Expires).After(id.NotAfter) {
		return nil, Identity{}, false
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil || time.UnixMilli(e.manifest.Issued).Before(leaf.NotBefore) || time.UnixMilli(e.manifest.Expires).After(leaf.NotAfter) {
		return nil, Identity{}, false
	}
	valid := e.signature.matches(certificateHash(chain[0]), func() bool {
		return validManifestSignature(e.wire, leaf)
	})
	d.entries[principal] = e
	if !valid {
		return nil, Identity{}, false
	}
	return chain, id, true
}

// expireLocked physically drops expired wire bodies and signatures. Only the
// compact revision/hash watermark survives for ten durations, independently
// of wall time.
func (d *Directory) expireLocked(now time.Time) bool {
	wallNow := now.Round(0)
	if wallNow.After(d.wallHighWater) {
		d.wallHighWater = wallNow
	}
	changed := false
	instant := d.leaseNow(now)
	for principal, e := range d.entries {
		if !e.activeUntil.After(instant) {
			delete(d.entries, principal)
			delete(d.handoffs, principal)
			changed = true
		}
	}
	for owner, held := range d.handoffs {
		if !held.deadline.After(instant) {
			delete(d.handoffs, owner)
			changed = true
		}
	}
	for owner, watermark := range d.history {
		if !watermark.Until.After(instant) {
			delete(d.history, owner)
			changed = true
		}
	}
	return changed
}

// Sweep runs from the node's idle maintenance tick, not only on incoming data.
// It releases full announcement allocations even when no neighbour sends more.
func (d *Directory) Sweep(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.expireLocked(now)
}

// RecordDeadline returns the receiver-local deadline for this exact current
// signed wire. It never changes the immutable announcement's signed dates.
// now is the receiver scheduling clock (normally time.Now with its monotonic
// component), never a certificate BaseTime, boot anchor or signed Issued time.
// The independent elapsed clock is projected into that scheduling domain.
func (d *Directory) RecordDeadline(w SignedManifest, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	owner := w.ownerKey()
	e, ok := d.entries[owner]
	if !ok || !sameManifestWire(e.wire, w) {
		return time.Time{}, false
	}
	if _, ok := d.currentEntryChain(owner, e, now); !ok {
		return time.Time{}, false
	}
	return now.Add(e.activeUntil.Sub(d.leaseNow(now))), true
}

// hasCurrentOwner is an export eligibility check, not a lease renewal. Missing
// repair material leaves the original body bounded by its original deadline.
func (d *Directory) hasCurrentOwner(owner string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	e, ok := d.entries[owner]
	if !ok {
		return false
	}
	_, ok = d.currentEntryChain(owner, e, now)
	return ok
}

func manifestHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Snapshot returns current origin records. Route reachability is a separate
// filter at the consumer, never inferred from having a directory entry. now is
// the receiver scheduling clock, never certificate BaseTime or a signed date.
func (d *Directory) Snapshot(now time.Time) []Manifest {
	manifests, _ := d.SnapshotWithRoutes(now)
	return manifests
}

// Records includes signed withdrawals for anti-entropy. Expired records are not
// retransmitted; compact revision tombstones still prevent local resurrection.
func (d *Directory) Records(now time.Time) []SignedManifest {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	out := []SignedManifest{}
	for principal, e := range d.entries {
		if _, ok := d.currentEntryChain(principal, e, now); !ok {
			continue
		}
		w := e.wire
		w.Body = append([]byte(nil), w.Body...)
		w.Signature = append([]byte(nil), w.Signature...)
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ownerKey() != out[j].ownerKey() {
			return out[i].ownerKey() < out[j].ownerKey()
		}
		return string(out[i].Body) < string(out[j].Body)
	})
	return out
}
