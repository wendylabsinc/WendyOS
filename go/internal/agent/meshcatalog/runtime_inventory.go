package meshcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

const (
	maxInventoryRetryPeers    = 1024
	inventoryRetryCooldown    = 30 * time.Second
	inventoryHandshakeTimeout = 30 * time.Second
	inventoryRepairTimeout    = 15 * time.Second
	inventorySnapshotTimeout  = 120 * time.Second
	maxInventoryQueueFrames   = 16
	maxInventoryQueueBytes    = 128 << 10
)

type inventoryRetry struct {
	cold  bool
	until time.Time
}

func (r *Runtime) sessionNegotiated(ctx context.Context, asset int32, conn net.Conn, protocol string) {
	if protocol == syncALPNv3 {
		r.sessionInventory(ctx, asset, conn)
		return
	}
	r.sessionWithProtocol(ctx, asset, conn, protocol == syncALPNv2)
}

// One immediate cold retry is allowed after a stale current inventory. A failed
// cold retry then waits a cooldown. This bounded state is not a trust cache.
func (r *Runtime) inventoryAttempt(asset int32, now time.Time) (cold, allowed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Before(r.inventoryOverflowUntil) {
		return false, false
	}
	for peer, state := range r.inventoryRetries {
		if !now.Before(state.until) {
			delete(r.inventoryRetries, peer)
		}
	}
	state, ok := r.inventoryRetries[asset]
	if !ok {
		return false, true
	}
	return state.cold, state.cold
}

// reserveInventoryAttempt consumes a cold retry before TCP/TLS starts. The
// caller passes the reservation into /3 after authentication; failed dials or
// handshakes cannot leave an unlimited supply of unconsumed cold retries.
func (r *Runtime) reserveInventoryAttempt(asset int32, now time.Time) (cold, allowed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Before(r.inventoryOverflowUntil) {
		return false, false
	}
	state, exists := r.inventoryRetries[asset]
	if exists && !now.Before(state.until) {
		delete(r.inventoryRetries, asset)
		exists = false
	}
	if !exists {
		return false, true
	}
	if !state.cold {
		return false, false
	}
	r.inventoryRetries[asset] = inventoryRetry{cold: false, until: now.Add(inventoryRetryCooldown)}
	return true, true
}

func (r *Runtime) inventoryFailed(asset int32, cold bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inventoryRetries == nil {
		r.inventoryRetries = make(map[int32]inventoryRetry)
	}
	if _, ok := r.inventoryRetries[asset]; !ok && len(r.inventoryRetries) >= maxInventoryRetryPeers {
		r.inventoryOverflowUntil = now.Add(inventoryRetryCooldown)
		return
	}
	r.inventoryRetries[asset] = inventoryRetry{cold: !cold, until: now.Add(inventoryRetryCooldown)}
}
func (r *Runtime) inventoryReady(asset int32) {
	r.mu.Lock()
	delete(r.inventoryRetries, asset)
	r.mu.Unlock()
}

type inventoryQueue struct {
	messages []inventoryMessage
	bytes    int
}

func (q *inventoryQueue) add(messages ...inventoryMessage) error {
	size := 0
	for _, m := range messages {
		b, err := inventoryWire(m)
		if err != nil {
			return err
		}
		size += len(b) + 4
	}
	if len(q.messages)+len(messages) > maxInventoryQueueFrames || q.bytes+size > maxInventoryQueueBytes {
		return errors.New("catalog transmit queue full")
	}
	q.messages = append(q.messages, messages...)
	q.bytes += size
	return nil
}
func (q *inventoryQueue) pop() {
	b, _ := inventoryWire(q.messages[0])
	q.bytes -= len(b) + 4
	q.messages[0] = inventoryMessage{}
	q.messages = q.messages[1:]
}

func (r *Runtime) sessionInventory(parent context.Context, asset int32, conn net.Conn) {
	// Inbound peers cannot be identified until authentication. At this point a
	// cooling-down peer is rejected before inventory allocation/application work.
	cold, allowed := r.reserveInventoryAttempt(asset, time.Now())
	if !allowed {
		_ = conn.Close()
		return
	}
	r.sessionInventoryReserved(parent, asset, conn, cold)
}

func (r *Runtime) sessionInventoryReserved(parent context.Context, asset int32, conn net.Conn, cold bool) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	peer := &catalogPeer{conn: conn, out: make(chan SignedRecord, 128)}
	r.mu.Lock()
	if r.closed || r.peers[asset] != nil {
		r.mu.Unlock()
		return
	}
	r.peers[asset] = peer
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.peers[asset] == peer {
			delete(r.peers, asset)
		}
		r.mu.Unlock()
	}()
	ready := false
	defer func() {
		if cold && !ready {
			r.inventoryFailed(asset, true, time.Now())
		}
	}()
	syncer := NewSynchronizer(r.catalog, r.catalog.cache)
	var fingerprints []string
	if !cold {
		fingerprints = r.catalog.cache.Fingerprints(time.Now(), maxInventoryEntries)
	}
	advertised := len(fingerprints) > 0
	var queue inventoryQueue
	if queue.add(inventoryMessages(fingerprints)...) != nil {
		return
	}
	in := make(chan inventoryMessage, 16)
	readErr := make(chan error, 1)
	out := make(chan inventoryMessage)
	writeErr := make(chan error, 1)
	readDone, writeDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			m, err := readInventoryMessage(conn)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case in <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(writeDone)
		for m := range out {
			_ = conn.SetWriteDeadline(time.Now().Add(8 * time.Second))
			if err := writeInventoryMessage(conn, m); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	defer func() { cancel(); _ = conn.Close(); close(out); <-readDone; <-writeDone }()
	var inventory receivedInventory
	var initial []SignedRecord
	initialQueued, snapshotComplete := false, false
	started := time.Now()
	var pendingSince time.Time
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	reconcile := time.NewTicker(15 * time.Second)
	defer reconcile.Stop()
	failRepair := func() {
		if advertised || cold {
			r.inventoryFailed(asset, cold, time.Now())
		}
	}
	addMessages := func(messages []Message) bool {
		for _, m := range messages {
			if queue.add(inventoryMessage{Message: m}) != nil {
				return false
			}
		}
		return true
	}
	for {
		// Pull initial snapshot records into a small wire queue. In particular a cold
		// snapshot never queues one certificate per origin ahead of the writer.
		if inventory.done && len(queue.messages) == 0 {
			// Skip already-sent stamps without one select/timer wait per record.
			// Idle reconciliation must emit neither records nor completion frames.
			for len(initial) > 0 && len(queue.messages) == 0 {
				w := initial[0]
				initial[0] = SignedRecord{}
				initial = initial[1:]
				if !addMessages(syncer.Record(w, time.Now())) {
					return
				}
			}
			if len(initial) == 0 && len(queue.messages) == 0 && !initialQueued {
				if queue.add(inventoryMessage{Message: Message{Kind: "snapshot-done"}}) != nil {
					return
				}
				initialQueued = true
			}
		}
		var next chan inventoryMessage
		var first inventoryMessage
		if len(queue.messages) > 0 {
			next = out
			first = queue.messages[0]
		}
		var changes <-chan SignedRecord
		if len(queue.messages) == 0 {
			changes = peer.out
		}
		select {
		case <-ctx.Done():
			return
		case <-readErr:
			if syncer.PendingIdentity() {
				failRepair()
			}
			return
		case <-writeErr:
			if syncer.PendingIdentity() {
				failRepair()
			}
			return
		case next <- first:
			queue.pop()
		case <-tick.C:
			now := time.Now()
			if !inventory.done && now.Sub(started) >= inventoryHandshakeTimeout {
				return
			}
			if !ready && now.Sub(started) >= inventorySnapshotTimeout {
				if syncer.PendingIdentity() {
					failRepair()
				}
				return
			}
			if !pendingSince.IsZero() && now.Sub(pendingSince) >= inventoryRepairTimeout {
				failRepair()
				return
			}
		case <-reconcile.C:
			if !r.eligible(asset, time.Now()) {
				return
			}
			// Reconciliation is a bounded catalog snapshot, fed through the same cursor.
			// No periodic complete []Message can grow the transmit queue behind a slow peer.
			if inventory.done && initialQueued && len(initial) == 0 {
				initial = syncer.snapshotRecords(time.Now())
			}
		case w := <-changes:
			if !inventory.done {
				// A fresh snapshot is taken at inventory completion; it covers these changes.
				continue
			}
			if !addMessages(syncer.Record(w, time.Now())) {
				return
			}
		case m := <-in:
			if !inventory.done {
				if err := inventory.receive(m); err != nil {
					return
				}
				if inventory.done {
					syncer.useCurrentInventory(inventory)
					initial = syncer.snapshotRecords(time.Now())
				}
				continue
			}
			if m.Kind == "identity-cache" || m.Kind == "identity-cache-done" {
				return
			}
			replies, changed, err := syncer.Receive(m.Message, time.Now())
			if err != nil {
				if errors.Is(err, errPendingIdentities) {
					failRepair()
				}
				return
			}
			if m.Kind == "snapshot-done" {
				snapshotComplete = true
			}
			if syncer.PendingIdentity() {
				if pendingSince.IsZero() {
					pendingSince = time.Now()
				}
			} else {
				pendingSince = time.Time{}
			}
			if snapshotComplete && !syncer.PendingIdentity() && !ready {
				r.mu.Lock()
				if r.peers[asset] == peer {
					peer.ready = true
				}
				r.mu.Unlock()
				ready = true
				r.inventoryReady(asset)
			}
			if !addMessages(replies) {
				return
			}
			for _, w := range changed {
				r.Broadcast(w)
				if r.onGatewayChange != nil {
					var record Record
					if json.Unmarshal(w.Body, &record) == nil && IsGatewayOffer(record) {
						r.onGatewayChange()
					}
				}
			}
		}
	}
}
