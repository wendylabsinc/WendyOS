package meshcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Runtime uses only the existing authenticated, link-local control channels.
// There is no service catalogue listener, dialer, or end-to-end TLS session.
// The Run goroutine owns peers and synchronization; projection queries use mu.
type Runtime struct {
	catalog         *Catalog
	snapshot        func() localmesh.NodeSnapshot
	mu              sync.Mutex
	fresh           map[meshid.Device]bool
	pendingFresh    map[meshid.Device]string
	wake            chan struct{}
	changes         chan SignedRecord
	peers           map[babel.LinkID]*catalogPeer
	requests        map[string]*freshRequestState
	transfers       []*freshTransfer
	identityRepairs map[string]*identityRepair
	hub             *localmesh.ControlHub
}

func NewRuntime(catalog *Catalog, snapshot func() localmesh.NodeSnapshot) (*Runtime, error) {
	if catalog == nil || snapshot == nil {
		return nil, errors.New("missing wendy net catalog runtime dependency")
	}
	return &Runtime{catalog: catalog, snapshot: snapshot, fresh: make(map[meshid.Device]bool), pendingFresh: make(map[meshid.Device]string), wake: make(chan struct{}, 1), changes: make(chan SignedRecord, 128), peers: make(map[babel.LinkID]*catalogPeer), requests: make(map[string]*freshRequestState)}, nil
}
func (r *Runtime) Broadcast(w SignedRecord) {
	select {
	case r.changes <- cloneWire(w):
	default: /* periodic reconciliation repairs unqueued publications */
	}
}
func (r *Runtime) Run(ctx context.Context, hubs ...*localmesh.ControlHub) error {
	if ctx == nil || len(hubs) != 1 || hubs[0] == nil {
		return errors.New("catalogue requires neighbour control transport")
	}
	r.hub = hubs[0]
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	reconcile := time.NewTicker(15 * time.Second)
	defer reconcile.Stop()
	defer func() {
		for _, p := range r.peers {
			p.cancel()
			p.syncer.Close()
		}
		r.mu.Lock()
		clear(r.fresh)
		clear(r.pendingFresh)
		r.mu.Unlock()
	}()
	for {
		r.syncPeers(ctx)
		for _, p := range r.peers {
			r.pump(p)
		}
		r.pumpTransfers()
		select {
		case <-ctx.Done():
			return nil
		case e := <-r.hub.Events():
			r.syncPeers(ctx)
			if len(e.Data) != 0 {
				if p := r.peers[e.Peer.ID]; p != nil {
					if err := r.receive(p, e.Data); err != nil {
						r.hub.ClosePeer(p.id)
					}
				}
			}
		case w := <-r.changes:
			r.broadcastExcept(w, 0)
		case <-r.wake:
		case <-tick.C:
			r.checkFreshness(time.Now())
		case <-reconcile.C:
			for _, p := range r.peers {
				if len(p.records) == 0 {
					p.records = p.syncer.snapshotRecords(time.Now())
				}
			}
		}
	}
}
func (r *Runtime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *Runtime) broadcastExcept(w SignedRecord, from babel.LinkID) {
	for id, p := range r.peers {
		if id != from && p.inventory.done {
			r.sendRecord(p, w)
		}
	}
}
func eligibleSnapshot(view localmesh.NodeSnapshot, device meshid.Device, now time.Time) bool {
	manifestOK := false
	for _, m := range view.Devices {
		if m.Device == device.UUID() && !m.Withdraw && time.UnixMilli(m.Expires).After(now) {
			manifestOK = true
			break
		}
	}
	if !manifestOK {
		return false
	}
	prefix := netip.PrefixFrom(device.MeshAddress(), 128)
	for _, route := range view.Routes {
		if route.Prefix == prefix && !route.Local && !route.Unreachable {
			return true
		}
	}
	return false
}
func (r *Runtime) ProjectionReady(device meshid.Device) bool {
	if !eligibleSnapshot(r.snapshot(), device, time.Now()) {
		r.InvalidateProjection(device)
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The origin-signed freshness proof establishes this device's durable
	// generation floors independently of any neighbour's snapshot progress.
	return r.fresh[device]
}
func (r *Runtime) InvalidateProjection(device meshid.Device) {
	r.mu.Lock()
	delete(r.fresh, device)
	delete(r.pendingFresh, device)
	r.mu.Unlock()
	r.signal()
}
func (r *Runtime) receive(p *catalogPeer, data []byte) error {
	var m inventoryMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	if m.Kind == "fresh-request" || m.Kind == "fresh-chunk" {
		return r.receiveFresh(p, m, time.Now())
	}
	if !p.inventory.done && m.Kind != "certificate" {
		if err := p.inventory.receive(m); err != nil {
			return err
		}
		if p.inventory.done {
			p.syncer.useCurrentInventory(p.inventory)
			p.records = p.syncer.snapshotRecords(time.Now())
		}
		return nil
	}
	if m.Kind == "identity-cache" || m.Kind == "identity-cache-done" {
		return errors.New("duplicate catalogue inventory")
	}
	replies, changed, err := p.syncer.Receive(m.Message, time.Now(), r.replySlots(p))
	if err != nil {
		return err
	}
	r.repairIdentityMessage(p, m.Message, time.Now())
	if !r.queueReplies(p, replies) {
		return errors.New("catalogue reply queue full")
	}
	if p.syncer.PendingIdentity() {
		if p.pendingSince.IsZero() {
			p.pendingSince = time.Now()
		}
	} else {
		p.pendingSince = time.Time{}
	}
	for _, w := range changed {
		// Receiving a verified copy proves this supplier has it, without an echo.
		r.broadcastExcept(w, p.id)
	}
	return nil
}
