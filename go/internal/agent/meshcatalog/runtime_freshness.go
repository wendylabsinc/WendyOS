package meshcatalog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type freshRequestState struct {
	query      FreshRequest
	upstream   babel.LinkID
	downstream babel.LinkID
	expires    time.Time
	data       []byte
	next       int
	total      int
}
type freshTransfer struct {
	nonce   string
	peer    babel.LinkID
	data    []byte
	offset  int
	expires time.Time
}

func (r *Runtime) nextHop(origin meshid.Device, exclude babel.LinkID) babel.LinkID {
	view := r.snapshot()
	prefix := netip.PrefixFrom(origin.MeshAddress(), 128)
	for _, route := range view.Routes {
		if route.Prefix == prefix && !route.Unreachable && !route.Local && route.Link != exclude && r.peers[route.Link] != nil {
			return route.Link
		}
	}
	for id, p := range r.peers {
		if p.device == origin && id != exclude {
			return id
		}
	}
	return 0
}
func (r *Runtime) checkFreshness(now time.Time) {
	r.repairKnownIdentities(now)
	for principal, repair := range r.identityRepairs {
		if !repair.expires.After(now) {
			delete(r.identityRepairs, principal)
		}
	}
	for _, p := range r.peers {
		if (!p.inventory.done && now.Sub(p.started) > 30*time.Second) || (!p.pendingSince.IsZero() && now.Sub(p.pendingSince) >= inventoryRepairTimeout && !p.syncer.certificates.Pending()) {
			r.hub.ClosePeer(p.id)
		}
	}
	for nonce, state := range r.requests {
		if !state.expires.After(now) {
			delete(r.requests, nonce)
			if state.upstream == 0 {
				origin, _ := meshid.DeviceFromPrincipal(state.query.Origin)
				r.mu.Lock()
				if r.pendingFresh[origin] == nonce {
					delete(r.pendingFresh, origin)
				}
				r.mu.Unlock()
			}
		}
	}
	for nonce, state := range r.requests {
		if state.upstream == 0 && state.total > 0 && state.next == state.total {
			if p := r.peers[state.downstream]; p != nil {
				if err := r.finishFresh(p, nonce, state, now); err != nil && r.hub != nil {
					r.hub.ClosePeer(p.id)
				}
			}
		}
	}
	view := r.snapshot()
	r.mu.Lock()
	for origin := range r.pendingFresh {
		if !eligibleSnapshot(view, origin, now) {
			delete(r.pendingFresh, origin)
		}
	}
	for origin := range r.fresh {
		if !eligibleSnapshot(view, origin, now) {
			delete(r.fresh, origin)
			delete(r.pendingFresh, origin)
		}
	}
	r.mu.Unlock()
	issued := 0
	for _, manifest := range view.Devices {
		origin := meshid.NewDevice(r.catalog.device.Tenant(), manifest.Device)
		if origin == r.catalog.device || !eligibleSnapshot(view, origin, now) {
			continue
		}
		r.mu.Lock()
		ready, pending := r.fresh[origin], r.pendingFresh[origin]
		r.mu.Unlock()
		if ready || pending != "" {
			continue
		}
		if issued >= 8 || len(r.requests) >= 256 {
			return
		}
		next := r.nextHop(origin, 0)
		if next == 0 {
			continue
		}
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return
		}
		q := FreshRequest{Origin: origin.Principal(), Requester: r.catalog.principal, Nonce: hex.EncodeToString(nonce), Hops: 32}
		if !r.queue(r.peers[next], inventoryMessage{Message: Message{Kind: "fresh-request"}, Request: &q}) {
			continue
		}
		r.requests[q.Nonce] = &freshRequestState{query: q, downstream: next, expires: now.Add(freshLifetime)}
		r.mu.Lock()
		r.pendingFresh[origin] = q.Nonce
		r.mu.Unlock()
		issued++
	}
}
func (r *Runtime) receiveFresh(p *catalogPeer, m inventoryMessage, now time.Time) error {
	if m.Request != nil {
		q := *m.Request
		origin, _ := meshid.DeviceFromPrincipal(q.Origin)
		if origin.Tenant() != r.catalog.device.Tenant() {
			return errors.New("foreign freshness request")
		}
		if _, seen := r.requests[q.Nonce]; seen {
			return nil
		}
		if len(r.requests) >= 256 {
			return nil
		}
		if q.Origin == r.catalog.principal {
			// Bound signing work and retained snapshots, including hostile requesters.
			if len(r.transfers) >= 4 {
				return nil
			}
			size := 0
			for _, x := range r.transfers {
				size += len(x.data)
			}
			if size >= 4<<20 {
				return nil
			}
			data, err := r.catalog.signInventory(q, now)
			if err != nil {
				return err
			}
			if size+len(data) > 8<<20 {
				return nil
			}
			r.requests[q.Nonce] = &freshRequestState{query: q, upstream: p.id, expires: now.Add(freshLifetime)}
			r.transfers = append(r.transfers, &freshTransfer{nonce: q.Nonce, peer: p.id, data: data, expires: now.Add(freshLifetime)})
			return nil
		}
		if q.Hops <= 1 {
			return nil
		}
		next := r.nextHop(origin, p.id)
		if next == 0 {
			return nil
		}
		q.Hops--
		if !r.queue(r.peers[next], inventoryMessage{Message: Message{Kind: "fresh-request"}, Request: &q}) {
			return nil
		}
		r.requests[q.Nonce] = &freshRequestState{query: q, upstream: p.id, downstream: next, expires: now.Add(freshLifetime)}
		return nil
	}
	chunk := m.Chunk
	state := r.requests[chunk.Nonce]
	if state == nil || state.downstream != p.id || !state.expires.After(now) {
		return nil
	}
	if state.upstream != 0 {
		if peer := r.peers[state.upstream]; peer == nil || !r.queue(peer, m) {
			// Never forward a later chunk after losing part of this response:
			// the requester would treat the hole as a protocol violation and
			// close its healthy neighbour. Abandon this nonce; its existing
			// freshness deadline leaves projection unready and triggers a new
			// request, without resetting the shared routing/data connection.
			delete(r.requests, chunk.Nonce)
		}
		return nil
	}
	origin, _ := meshid.DeviceFromPrincipal(state.query.Origin)
	r.mu.Lock()
	expected := r.pendingFresh[origin] == chunk.Nonce
	r.mu.Unlock()
	if !expected {
		return nil
	}
	if chunk.Index != state.next || (state.total != 0 && state.total != chunk.Total) {
		return errors.New("unordered freshness response")
	}
	bytes := len(chunk.Data)
	for _, s := range r.requests {
		bytes += len(s.data)
	}
	if bytes > 8<<20 {
		return errors.New("freshness assembly capacity reached")
	}
	if len(state.data)+len(chunk.Data) > maxFreshBytes {
		return errors.New("oversized freshness response")
	}
	state.data = append(state.data, chunk.Data...)
	state.next++
	state.total = chunk.Total
	if state.next == state.total {
		return r.finishFresh(p, chunk.Nonce, state, now)
	}
	return nil
}

func (r *Runtime) finishFresh(p *catalogPeer, nonce string, state *freshRequestState, now time.Time) error {
	if !state.expires.After(now) {
		return nil
	}
	origin, _ := meshid.DeviceFromPrincipal(state.query.Origin)
	r.mu.Lock()
	expected := r.pendingFresh[origin] == nonce
	r.mu.Unlock()
	if !expected {
		delete(r.requests, nonce)
		return nil
	}
	if err := r.catalog.acceptInventory(state.data, state.query, now); err != nil {
		if !errors.Is(err, ErrIdentityNeeded) {
			return err
		}
		// The proof already passed requester/nonce/expiry/origin binding.
		// Fetch missing elements from the neighbour that supplied it, using
		// the same coalesced pool as directory and service identity repair.
		var signed signedInventory
		if err := json.Unmarshal(state.data, &signed); err != nil || signed.Chain == nil {
			return errors.New("invalid origin certificate description")
		}
		replies, changed, err := p.syncer.Receive(Message{Kind: "certificate", Certificate: &localmesh.CertificateMessage{Chain: signed.Chain}}, now)
		if err != nil {
			return err
		}
		if !r.queueReplies(p, replies) {
			return errors.New("certificate reply backlog full")
		}
		for _, w := range changed {
			r.broadcastExcept(w, p.id)
		}
		return nil
	}
	r.mu.Lock()
	if r.pendingFresh[origin] == nonce {
		r.fresh[origin] = true
		delete(r.pendingFresh, origin)
	}
	r.mu.Unlock()
	delete(r.requests, nonce)
	return nil
}
func (r *Runtime) pumpTransfers() {
	keep := r.transfers[:0]
	now := time.Now()
	for _, x := range r.transfers {
		p := r.peers[x.peer]
		if p == nil || !x.expires.After(now) {
			continue
		}
		for x.offset < len(x.data) {
			end := min(len(x.data), x.offset+freshChunkBytes)
			m := inventoryMessage{Message: Message{Kind: "fresh-chunk"}, Chunk: &FreshChunk{Nonce: x.nonce, Index: x.offset / freshChunkBytes, Total: (len(x.data) + freshChunkBytes - 1) / freshChunkBytes, Data: x.data[x.offset:end]}}
			if !r.queue(p, m) {
				break
			}
			x.offset = end
		}
		if x.offset < len(x.data) {
			keep = append(keep, x)
		}
	}
	r.transfers = keep
}
