package replication

import (
	"context"
	"errors"
	"sync"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// SyncPeer performs one bounded pull pass. Concurrent attempts for the same
// peer refuse immediately. It never retries an RPC or a mutation; unavailable
// entries are deferred to a later explicitly scheduled pass. Store callbacks
// finish before network calls, and no identity fence is held by this package.
func (s *Service) SyncPeer(ctx context.Context, device string) (result Result, err error) {
	if ctx == nil {
		return result, ErrDenied
	}
	tenant, e := serviceaccess.DeviceTenant(device)
	if e != nil || tenant != s.cfg.Endpoint.Tenant || device == s.cfg.Endpoint.Device {
		return result, ErrDenied
	}
	s.mu.Lock()
	if s.outbound[device] {
		s.mu.Unlock()
		return result, ErrBusy
	}
	if len(s.outbound) >= s.cfg.Limits.MaxPeers {
		s.mu.Unlock()
		return result, ErrCapacity
	}
	s.outbound[device] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.outbound, device); s.mu.Unlock() }()
	defer func() {
		if resetProgress(err) {
			s.clearProgress(device)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RoundTimeout)
	defer cancel()
	dialCtx, stopDial := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	connection, e := s.cfg.Dial(dialCtx, device)
	stopDial()
	if connection.Close != nil {
		defer func() {
			closeErr := connection.Close.Close()
			if err == nil && closeErr != nil {
				result.Complete = false
				err = closeErr
			}
		}()
	}
	if e != nil {
		return result, e
	}
	if connection.Client == nil || connection.Close == nil || connection.Identity.Device != device || !s.identity(connection.Identity) {
		return result, ErrDenied
	}
	p := connection.Identity
	a, e := s.authorize(ctx, p, Import)
	if e != nil {
		return result, e
	}
	if len(a.Sources) == 0 {
		// An explicitly export-only peer has no pull work. It still passes
		// authenticated identity/current-policy checks, never a store scan.
		s.clearProgress(device)
		result.Complete = true
		return result, nil
	}
	remote := s.cfg.Endpoint
	remote.Device = device
	saved := s.loadProgress(p, a.Generation)
	cursor := saved.Cursor
	generation := saved.RemoteGeneration
	last := saved.Last
	result.Deferred = saved.Deferred
	seenCursors := map[string]bool{}
	if len(cursor) > 0 {
		seenCursors[string(cursor)] = true
	}
	charge := func(message proto.Message) error {
		n := proto.Size(message)
		if n > s.cfg.Limits.MaxBytes-result.Bytes {
			return ErrCapacity
		}
		result.Bytes += n
		return nil
	}
	for result.Pages < s.cfg.Limits.MaxPages {
		if result.Entries == s.cfg.Limits.MaxEntries {
			return result, ErrCapacity
		}
		if e = s.current(ctx, p, Import, a.Generation); e != nil {
			return result, e
		}
		limit := min(MaxInventoryEntries, s.cfg.Limits.MaxEntries-result.Entries)
		request := &replicav1.InventoryRequest{Version: Version, ExpectedGeneration: generation, Cursor: append([]byte(nil), cursor...), Limit: uint32(limit)}
		if e = charge(request); e != nil {
			return result, e
		}
		rpcCtx, stop := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
		started := s.elapsed()
		page, e := connection.Client.Inventory(rpcCtx, request, grpc.MaxCallRecvMsgSize(MaxInventoryBytes), grpc.MaxCallSendMsgSize(MaxInventoryBytes))
		stop()
		if e != nil {
			return result, e
		}
		if page == nil || !known(page.ProtoReflect()) || page.Version != Version || !scopeEqual(page.Scope, remote) || page.Generation == 0 || generation != 0 && page.Generation != generation || len(page.Entries) > limit || proto.Size(page) > MaxInventoryBytes || len(page.NextCursor) > MaxCursorBytes || len(page.NextCursor) > 0 && (len(page.Entries) == 0 || seenCursors[string(page.NextCursor)]) {
			return result, ErrProtocol
		}
		if e = charge(page); e != nil {
			return result, e
		}
		generation = page.Generation
		result.Pages++
		if len(page.Entries) > s.cfg.Limits.MaxEntries-result.Entries {
			return result, ErrCapacity
		}
		entries := make([]Entry, 0, len(page.Entries))
		for _, wire := range page.Entries {
			entry, e := entryValue(wire, remote, device)
			if e != nil || !allowed(entry, a.Sources) || entry.OrderKey() <= last {
				return result, ErrProtocol
			}
			last = entry.OrderKey()
			entries = append(entries, entry)
		}
		result.Entries += len(entries)
		if e = s.current(ctx, p, Import, a.Generation); e != nil {
			return result, e
		}
		wanted := make([]Entry, 0, len(entries))
		for _, entry := range entries {
			need, e := s.cfg.Store.Need(ctx, entry)
			if e != nil {
				return result, e
			}
			if need {
				wanted = append(wanted, entry)
			}
		}
		for len(wanted) > 0 {
			n := min(MaxFetchEntries, len(wanted))
			batch := wanted[:n]
			wanted = wanted[n:]
			if e = s.current(ctx, p, Import, a.Generation); e != nil {
				return result, e
			}
			request := &replicav1.FetchRequest{Version: Version, ExpectedGeneration: generation}
			for _, entry := range batch {
				request.Entries = append(request.Entries, entryWire(entry))
			}
			if e = charge(request); e != nil {
				return result, e
			}
			rpcCtx, stop := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
			page, e := connection.Client.Fetch(rpcCtx, request, grpc.MaxCallRecvMsgSize(MaxPageBytes), grpc.MaxCallSendMsgSize(MaxInventoryBytes))
			stop()
			if e != nil {
				return result, e
			}
			if page == nil || !known(page.ProtoReflect()) || page.Version != Version || !scopeEqual(page.Scope, remote) || page.Generation != generation || len(page.Items)+len(page.Unavailable) != len(batch) || proto.Size(page) > MaxPageBytes {
				return result, ErrProtocol
			}
			if e = charge(page); e != nil {
				return result, e
			}
			requested := map[Entry]bool{}
			for _, entry := range batch {
				requested[entry] = true
			}
			items := map[Entry]Item{}
			unavailable := map[Entry]bool{}
			for _, wire := range page.Items {
				if wire == nil {
					return result, ErrProtocol
				}
				entry, e := entryValue(wire.Entry, remote, device)
				if e != nil || !requested[entry] {
					return result, ErrProtocol
				}
				if _, exists := items[entry]; exists {
					return result, ErrProtocol
				}
				item := cloneItem(Item{Entry: entry, Envelope: wire.Envelope})
				if ValidateItem(item, remote, device) != nil {
					return result, ErrProtocol
				}
				items[entry] = item
			}
			for _, wire := range page.Unavailable {
				entry, e := entryValue(wire, remote, device)
				if e != nil || !requested[entry] || unavailable[entry] {
					return result, ErrProtocol
				}
				if _, exists := items[entry]; exists {
					return result, ErrProtocol
				}
				unavailable[entry] = true
			}
			if len(items)+len(unavailable) != len(batch) {
				return result, ErrProtocol
			}
			if e = s.current(ctx, p, Import, a.Generation); e != nil {
				return result, e
			}
			result.Unavailable += len(unavailable)
			result.Deferred = result.Deferred || len(unavailable) > 0
			result.Fetched += len(items)
			// Apply in inventory order, regardless of response item order.
			for _, entry := range batch {
				item, found := items[entry]
				if !found {
					continue
				}
				if e = s.cfg.RemoteAuthority(ctx, cloneItem(item)); e != nil {
					if canceled := ctx.Err(); canceled != nil {
						return result, canceled
					}
					return result, ErrDenied
				}
				if e = s.admit(ctx, p, a.Generation, item); e != nil {
					if admissionDeferred(ctx, item, e) {
						result.Deferred = true
						return result, e
					}
					result.UnknownOutcome = true
					return result, errors.Join(ErrUncertain, e)
				}
				result.Admitted++
			}
		}
		if e = s.current(ctx, p, Import, a.Generation); e != nil {
			return result, e
		}
		if len(page.NextCursor) == 0 {
			s.clearProgress(device)
			result.Complete = !result.Deferred
			return result, nil
		}
		if e = s.saveProgress(p, a.Generation, generation, page.NextCursor, last, started, result.Deferred); e != nil {
			return result, e
		}
		cursor = append([]byte(nil), page.NextCursor...)
		seenCursors[string(cursor)] = true
	}
	return result, ErrCapacity
}

func (s *Service) admit(ctx context.Context, p Identity, g uint64, item Item) (failure error) {
	// Bounded synchronous pre/post-commit checks share one sticky failure.
	// The host must still place the precommit check inside its transaction.
	var mu sync.Mutex
	active := true
	calls := 0
	var checked error
	final := func(commitCtx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || calls >= MaxFinalChecks || commitCtx == nil {
			if checked == nil {
				checked = ErrDenied
			}
			return checked
		}
		calls++
		if checked != nil {
			return checked
		}
		if e := ctx.Err(); e != nil {
			checked = e
			return e
		}
		if e := s.cfg.RemoteAuthority(commitCtx, cloneItem(item)); e != nil {
			checked = ErrDenied
			return checked
		}
		checked = s.current(commitCtx, p, Import, g)
		if checked == nil {
			checked = ctx.Err()
		}
		return checked
	}
	err := s.cfg.Store.Admit(ctx, cloneItem(item), final)
	mu.Lock()
	active = false
	count, last := calls, checked
	mu.Unlock()
	defer func() {
		if failure != nil && s.cfg.AdmissionFailure != nil {
			s.cfg.AdmissionFailure(item.Entry.Kind, count, failure)
		}
	}()
	if err != nil {
		if IsAdmissionNotStarted(err) && (count != 0 || last != nil) {
			return errors.Join(ErrUncertain, err, last)
		}
		if errors.Is(err, ErrRecordWindowDeferred) && (count > 1 || last != nil || ctx.Err() != nil) {
			return errors.Join(ErrUncertain, err, last, ctx.Err())
		}
		return err
	}
	if count == 0 || last != nil {
		return ErrDenied
	}
	if err := s.current(ctx, p, Import, g); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}
