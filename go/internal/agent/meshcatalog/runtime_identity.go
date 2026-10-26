package meshcatalog

import (
	"encoding/json"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type identityRepair struct {
	waiters map[babel.LinkID]bool
	expires time.Time
}

// Missing identity requests are coalesced across links. Flood only the first
// request in a bounded repair window; a cycle cannot recursively reflood it.
func (r *Runtime) requestIdentity(from babel.LinkID, principal string, now time.Time) {
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil || device.Tenant() != r.catalog.device.Tenant() {
		return
	}
	if r.identityRepairs == nil {
		r.identityRepairs = make(map[string]*identityRepair)
	}
	if old := r.identityRepairs[principal]; old != nil && old.expires.After(now) {
		old.waiters[from] = true
		return
	}
	if len(r.identityRepairs) >= 128 {
		return
	}
	r.identityRepairs[principal] = &identityRepair{waiters: map[babel.LinkID]bool{from: true}, expires: now.Add(inventoryRepairTimeout)}
	for id, peer := range r.peers {
		if id != from && peer.inventory.done {
			r.queueMessages(peer, []Message{{Kind: "identity-request", Principal: principal}})
		}
	}
}
func (r *Runtime) deliverIdentity(principal string) {
	repair := r.identityRepairs[principal]
	if repair == nil {
		return
	}
	delete(r.identityRepairs, principal)
	for id := range repair.waiters {
		if p := r.peers[id]; p != nil {
			r.queueReplies(p, []Message{{Kind: "certificate", Certificate: p.syncer.certificates.Describe(principal, time.Now())}})
		}
	}
}
func (r *Runtime) repairIdentityMessage(p *catalogPeer, m Message, now time.Time) {
	if m.Kind == "identity-request" {
		if _, _, ok := r.catalog.cache.Get(m.Principal, now); !ok {
			r.requestIdentity(p.id, m.Principal, now)
		}
	}
	if m.Kind == "bundle" || m.Kind == "certificate" {
		// Only independently verified complete identities can satisfy waiters.
		r.repairKnownIdentities(now)
		for principal := range r.identityRepairs {
			chain, _, ok := r.catalog.cache.Get(principal, now)
			if ok && len(chain) > 0 {
				r.deliverIdentity(principal)
			}
		}
	}
	if m.Kind == "gossip" {
		for principal := range p.syncer.pending {
			if _, _, ok := r.catalog.cache.Get(principal, now); !ok {
				r.requestIdentity(p.id, principal, now)
			}
		}
	}
	if m.Kind == "record" && m.Record != nil {
		var rec Record
		if json.Unmarshal(m.Record.Body, &rec) == nil {
			if _, _, ok := r.catalog.cache.Get(rec.Key.Device, now); !ok {
				r.requestIdentity(p.id, rec.Key.Device, now)
			}
		}
	}
}

// An inventory proof can populate the shared cache too; pending records must
// not remain stuck merely because the bundle arrived on another protocol path.
func (r *Runtime) repairKnownIdentities(now time.Time) {
	for id, peer := range r.peers {
		replies, admitted, err := peer.syncer.ContinueCertificates(now, r.replySlots(peer))
		if err != nil {
			if r.hub != nil {
				r.hub.ClosePeer(id)
			}
			continue
		}
		if !r.queueReplies(peer, replies) {
			if r.hub != nil {
				r.hub.ClosePeer(id)
			}
			continue
		}
		for _, w := range admitted {
			r.broadcastExcept(w, id)
		}
		for principal := range peer.syncer.pending {
			chain, _, ok := r.catalog.cache.Get(principal, now)
			if !ok {
				continue
			}
			_, changed, err := peer.syncer.Receive(Message{Kind: "bundle", Bundle: chain}, now)
			if err != nil {
				if r.hub != nil {
					r.hub.ClosePeer(id)
				}
				continue
			}
			for _, w := range changed {
				r.broadcastExcept(w, id)
			}
		}
		if !peer.syncer.PendingIdentity() {
			peer.pendingSince = time.Time{}
		}
	}
}
