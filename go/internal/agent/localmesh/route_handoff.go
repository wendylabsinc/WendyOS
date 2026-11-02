package localmesh

import (
	"crypto/sha256"
	"crypto/x509"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// RouteProof is detached routing authority, not a transferable announcement.
// Deadline is the receiver's original one-duration lease; no signature, payload
// or certificate is retained or exported with this diagnostic projection.
type RouteProof struct {
	Principal string
	Deadline  time.Time
	Retained  bool
}

// One compact old proof may bridge a pure sequence handoff. Its authority was
// established by its own signature; a replacement never lends it authority.
// It is RAM-only and cannot renew a directory entry or replay watermark.
type routeHandoff struct {
	origin          RouteOrigin
	principal       string
	key             [32]byte
	continuity      [32]byte
	issued, expires int64
	deadline        time.Time
}

func routeContinuity(m Manifest) ([32]byte, bool) {
	if m.Version != ManifestVersionRoute || m.Withdraw || m.Route == nil {
		return [32]byte{}, false
	}
	m = cloneManifest(m)
	m.Revision, m.Issued, m.Expires = 0, 0, 0
	m.AdmittedUntil = time.Time{}
	m.Route.Seqno = 0
	b, err := netwire.Marshal(m)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(b), true
}
func laterRouteSequence(next, previous uint16) bool { d := next - previous; return d != 0 && d < 1<<15 }

// Caller holds d.mu. newLeaf independently verified the replacement signature.
// Capture only an already-live old signature that verifies under that same key.
func (d *Directory) replaceRouteHandoff(owner string, next Manifest, newLeaf *x509.Certificate, now time.Time) {
	old, exists := d.entries[owner]
	continuity, eligible := routeContinuity(next)
	if !exists || !eligible {
		delete(d.handoffs, owner)
		return
	}
	oldContinuity, oldEligible := routeContinuity(old.manifest)
	key := sha256.Sum256(newLeaf.RawSubjectPublicKeyInfo)
	if !oldEligible || oldContinuity != continuity || old.manifest.Route.RouterID != next.Route.RouterID || old.manifest.Route.Prefix != next.Route.Prefix {
		delete(d.handoffs, owner)
		return
	}
	if held, ok := d.handoffs[owner]; ok {
		// Do not chain later replacements into a refreshed old deadline.
		if held.continuity == continuity && held.key == key && held.principal == old.wire.Principal && held.deadline.After(d.leaseNow(now)) &&
			held.origin.RouterID == next.Route.RouterID && held.origin.Prefix == next.Route.Prefix && laterRouteSequence(next.Route.Seqno, held.origin.Seqno) {
			return
		}
		delete(d.handoffs, owner)
	}
	if !old.activeUntil.After(d.leaseNow(now)) || !laterRouteSequence(next.Route.Seqno, old.manifest.Route.Seqno) ||
		!validManifestSignature(old.wire, newLeaf) {
		return
	}
	if d.handoffs == nil {
		d.handoffs = make(map[string]routeHandoff)
	}
	// The map is bounded by current directory owners, never by revisions.
	d.handoffs[owner] = routeHandoff{origin: *old.manifest.Route, principal: old.wire.Principal, key: key, continuity: continuity,
		issued: old.manifest.Issued, expires: old.manifest.Expires, deadline: old.activeUntil}
}

// Caller already verified the current owner/path. Check that this compact old
// authorization still has its own original lease and the same trusted key.
func (d *Directory) validHandoff(owner string, held routeHandoff, current directoryEntry, chain [][]byte, id Identity, now time.Time) bool {
	continuity, ok := routeContinuity(current.manifest)
	if !ok || continuity != held.continuity || held.principal != current.wire.Principal || !held.deadline.After(d.leaseNow(now)) ||
		current.manifest.Route.Prefix != held.origin.Prefix || current.manifest.Route.RouterID != held.origin.RouterID ||
		!laterRouteSequence(current.manifest.Route.Seqno, held.origin.Seqno) || len(chain) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(chain[0])
	return err == nil && sha256.Sum256(leaf.RawSubjectPublicKeyInfo) == held.key &&
		!time.UnixMilli(held.issued).Before(leaf.NotBefore) && !time.UnixMilli(held.expires).After(leaf.NotAfter) &&
		!time.UnixMilli(held.expires).After(id.NotAfter)
}

// routeViewLocked performs one verification pass for both presence and route
// policy. A compact handoff never appears in Snapshot/Records as an old body.
func (d *Directory) routeViewLocked(now time.Time, detached bool) ([]Manifest, map[RouteOrigin]RouteProof) {
	manifests := make([]Manifest, 0, len(d.entries))
	chains := make(map[string][][]byte)
	identities := make(map[string]Identity)
	owners := make(map[string]directoryEntry)
	instant := d.leaseNow(now)
	for owner, e := range d.entries {
		if e.manifest.Withdraw {
			continue
		}
		chain, id, ok := d.currentEntryIdentity(owner, e, now)
		if !ok {
			delete(d.handoffs, owner)
			continue
		}
		m := e.manifest
		if detached {
			m = cloneManifest(m)
		}
		m.AdmittedUntil = now.Add(e.activeUntil.Sub(instant))
		if detached && m.AgentTransport != "" && m.AgentTransport != AgentMTLS {
			m.AgentPort = 0
		}
		manifests = append(manifests, m)
		owners[owner], chains[owner], identities[owner] = e, chain, id
	}
	authorized := authorizedOrigins(manifests, d.tenant)
	proofs := make(map[RouteOrigin]RouteProof, len(authorized)+len(d.handoffs))
	for _, m := range manifests {
		if m.Route == nil || !authorized[*m.Route] {
			continue
		}
		device, err := m.Peer(d.tenant)
		if err != nil {
			continue
		}
		proofs[*m.Route] = RouteProof{Principal: device.Principal(), Deadline: m.AdmittedUntil}
	}
	for owner, held := range d.handoffs {
		current, ok := owners[owner]
		if !ok || current.manifest.Route == nil || !authorized[*current.manifest.Route] || !d.validHandoff(owner, held, current, chains[owner], identities[owner], now) {
			delete(d.handoffs, owner)
			continue
		}
		until := held.deadline
		if current.activeUntil.Before(until) {
			until = current.activeUntil
		}
		proofs[held.origin] = RouteProof{Principal: held.principal, Deadline: now.Add(until.Sub(instant)), Retained: true}
	}
	if detached {
		sort.Slice(manifests, func(i, j int) bool { return manifests[i].Device.String() < manifests[j].Device.String() })
	}
	return manifests, proofs
}

func (d *Directory) SnapshotWithRoutes(now time.Time) ([]Manifest, map[RouteOrigin]RouteProof) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	return d.routeViewLocked(now, true)
}

// PendingRouteHandoffs names current signed tuples awaiting actual Babel state.
// It grants no routing authority beyond RouteAuthorizations.
func (d *Directory) PendingRouteHandoffs(now time.Time) []RouteOrigin {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	pending := make([]RouteOrigin, 0, len(d.handoffs))
	for owner := range d.handoffs {
		if e, ok := d.entries[owner]; ok && e.manifest.Route != nil {
			pending = append(pending, *e.manifest.Route)
		}
	}
	return pending
}
func (d *Directory) CompleteRouteHandoff(origin RouteOrigin, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	for owner := range d.handoffs {
		if e, ok := d.entries[owner]; ok && e.manifest.Route != nil && *e.manifest.Route == origin {
			delete(d.handoffs, owner)
			return true
		}
	}
	return false
}
