package replication

import (
	"context"
	"errors"
	"sort"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// Latest pushes have independent bounded slots: a historical inventory pass
// must not occupy the live forwarding slot. The host stores still serialize
// their existing authority and commit checks.
func (s *Service) enterLatest(ctx context.Context) (Identity, func(), error) {
	select {
	case s.latestSlots <- struct{}{}:
	default:
		return Identity{}, nil, ErrCapacity
	}
	p, err := s.cfg.IncomingPeer(ctx)
	if err != nil || !s.identity(p) {
		<-s.latestSlots
		return Identity{}, nil, ErrDenied
	}
	s.mu.Lock()
	busy := s.latestInbound[p.Device]
	if !busy {
		s.latestInbound[p.Device] = true
	}
	s.mu.Unlock()
	if busy {
		<-s.latestSlots
		return Identity{}, nil, ErrBusy
	}
	return p, func() {
		s.mu.Lock()
		delete(s.latestInbound, p.Device)
		s.mu.Unlock()
		<-s.latestSlots
	}, nil
}

// PushLatest receives a bounded batch of exact original Items over the native
// authenticated peer service. This is not a datagram plane or a fresh clock.
// Floors precede leases and records; each item uses the same current authority
// and durable admission contract as repair, without an inventory/fetch pass.
func (s *Service) PushLatest(ctx context.Context, req *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error) {
	if ctx == nil || req == nil || !known(req.ProtoReflect()) || req.Version != Version || len(req.Items) < 1 || len(req.Items) > MaxLatestItems || proto.Size(req) > MaxLatestBytes {
		return nil, rpcError(ErrProtocol)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	peer, leave, err := s.enterLatest(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	defer leave()
	origin := s.cfg.Endpoint
	origin.Device = peer.Device
	if !scopeEqual(req.Scope, origin) {
		return nil, rpcError(ErrProtocol)
	}
	access, err := s.authorize(ctx, peer, Import)
	if err != nil {
		return nil, rpcError(err)
	}
	items := make([]Item, 0, len(req.Items))
	last := ""
	for _, wire := range req.Items {
		if wire == nil {
			return nil, rpcError(ErrProtocol)
		}
		entry, e := entryValue(wire.Entry, origin, peer.Device)
		item := cloneItem(Item{Entry: entry, Envelope: wire.Envelope})
		if e != nil || ValidateItem(item, origin, peer.Device) != nil || entry.OrderKey() <= last {
			return nil, rpcError(ErrProtocol)
		}
		if !allowed(entry, access.Sources) {
			return nil, rpcError(ErrDenied)
		}
		last = entry.OrderKey()
		items = append(items, item)
	}
	reply := &replicav1.PushLatestReply{Version: Version, Scope: scopeWire(s.cfg.Endpoint), Generation: access.Generation}
	for _, item := range items {
		if err = s.current(ctx, peer, Import, access.Generation); err != nil {
			return nil, rpcError(err)
		}
		need, e := s.cfg.Store.Need(ctx, item.Entry)
		if e != nil {
			return nil, rpcError(e)
		}
		if !need {
			reply.Skipped++
			continue
		}
		if e = s.cfg.RemoteAuthority(ctx, cloneItem(item)); e != nil {
			return nil, rpcError(ErrDenied)
		}
		if e = s.admit(ctx, peer, access.Generation, item); e != nil {
			if admissionDeferred(ctx, item, e) {
				reply.Deferred = true
			} else {
				// The host dispatcher applies its existing incoming-admission
				// fault handling. A sender seeing this never resends the batch.
				reply.UnknownOutcome = true
			}
			return reply, nil
		}
		reply.Admitted++
	}
	if err = s.current(ctx, peer, Import, access.Generation); err != nil {
		return nil, rpcError(err)
	}
	reply.Complete = true
	return reply, nil
}

// SendLatest performs one native peer push using a caller-owned authenticated
// connection. It never dials, closes, retries, inventories or fetches. The host
// owns bounded coalescing and wakes it only with committed local originals.
// UnknownOutcome here can mean a lost remote reply, not a local store fault:
// retire that batch, retain the diagnostic, and allow fresh values and repair.
func (s *Service) SendLatest(ctx context.Context, connection Connection, items []Item) (result Result, err error) {
	if ctx == nil || connection.Client == nil || connection.Close == nil || !s.identity(connection.Identity) {
		return result, ErrDenied
	}
	if len(items) < 1 || len(items) > MaxLatestItems {
		return result, ErrCapacity
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	owned := make([]Item, len(items))
	for i, item := range items {
		owned[i] = cloneItem(item)
		if ValidateItem(owned[i], s.cfg.Endpoint, s.cfg.Endpoint.Device) != nil {
			return result, ErrProtocol
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Entry.OrderKey() < owned[j].Entry.OrderKey() })
	peer := connection.Identity
	s.mu.Lock()
	if s.latestOutbound[peer.Device] {
		s.mu.Unlock()
		return result, ErrBusy
	}
	if len(s.latestOutbound) >= s.cfg.Limits.MaxPeers {
		s.mu.Unlock()
		return result, ErrCapacity
	}
	s.latestOutbound[peer.Device] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.latestOutbound, peer.Device); s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	access, err := s.authorize(ctx, peer, Export)
	if err != nil {
		return result, err
	}
	req := &replicav1.PushLatestRequest{Version: Version, Scope: scopeWire(s.cfg.Endpoint)}
	last := ""
	for _, item := range owned {
		if item.Entry.OrderKey() <= last {
			return result, ErrProtocol
		}
		if !allowed(item.Entry, access.Sources) {
			return result, ErrDenied
		}
		last = item.Entry.OrderKey()
		req.Items = append(req.Items, &replicav1.Item{Entry: entryWire(item.Entry), Envelope: item.Envelope})
	}
	if proto.Size(req) > MaxLatestBytes {
		return result, ErrCapacity
	}
	result.Entries, result.Bytes = len(owned), proto.Size(req)
	if err = s.current(ctx, peer, Export, access.Generation); err != nil {
		return result, err
	}
	settled := false
	defer func() {
		if !settled {
			result.UnknownOutcome = true
			err = errors.Join(ErrUncertain, err)
		}
	}()
	reply, err := connection.Client.PushLatest(ctx, req, grpc.MaxCallRecvMsgSize(MaxInventoryBytes), grpc.MaxCallSendMsgSize(MaxLatestBytes), grpc.MaxRetryRPCBufferSize(0))
	if err != nil {
		return result, err
	}
	remote := s.cfg.Endpoint
	remote.Device = peer.Device
	if reply == nil || !known(reply.ProtoReflect()) || reply.Version != Version || reply.Generation == 0 || !scopeEqual(reply.Scope, remote) || proto.Size(reply) > MaxInventoryBytes {
		return result, ErrProtocol
	}
	flags := 0
	for _, value := range []bool{reply.Complete, reply.Deferred, reply.UnknownOutcome} {
		if value {
			flags++
		}
	}
	processed := uint64(reply.Admitted) + uint64(reply.Skipped)
	if flags != 1 || processed > uint64(len(owned)) || reply.Complete != (processed == uint64(len(owned))) {
		return result, ErrProtocol
	}
	settled = true
	result.Admitted, result.Skipped = int(reply.Admitted), int(reply.Skipped)
	result.UnknownOutcome, result.Deferred = reply.UnknownOutcome, reply.Deferred
	if reply.UnknownOutcome {
		return result, ErrUncertain
	}
	if err = s.current(ctx, peer, Export, access.Generation); err != nil {
		return result, err
	}
	if reply.Deferred {
		return result, ErrAdmissionDeferred
	}
	result.Complete = true
	return result, nil
}
