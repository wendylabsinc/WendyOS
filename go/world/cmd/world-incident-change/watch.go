package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

const watchSnapshotLease = 750 * time.Millisecond

// snapshotWatch owns one bounded immutable replacement snapshot and one ongoing
// authenticated World Watch. It neither stamps captures nor extends their TTLs.
type snapshotWatch struct {
	mu       sync.Mutex
	snapshot client.Snapshot
	received time.Time
	failure  error
	ready    bool
	changed  chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
	now      func() time.Time
}

func startSnapshotWatch(ctx context.Context, api *client.Client, tenant, site string) *snapshotWatch {
	ctx, cancel := context.WithCancel(ctx)
	w := &snapshotWatch{changed: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), now: time.Now}
	go func() {
		defer close(w.done)
		cursor := ""
		for ctx.Err() == nil {
			started := time.Now()
			call, stop := context.WithTimeout(ctx, 700*time.Millisecond)
			var snapshot client.Snapshot
			err := api.Do(call, "read", "GET", "/v1/watch?timeout_ms=400&cursor="+url.QueryEscape(cursor), nil, &snapshot)
			stop()
			if ctx.Err() != nil {
				break
			}
			if err == nil {
				decoded, e := hex.DecodeString(snapshot.Cursor)
				if e != nil || len(decoded) != 32 || len(snapshot.Components) > 256 {
					err = errors.New("bounded watch snapshot required")
				}
				for _, c := range snapshot.Components {
					if c.Key.Tenant != tenant || c.Key.Site != site {
						err = errors.New("watch site scope differs")
					}
				}
			}
			w.mu.Lock()
			if err != nil {
				w.snapshot = client.Snapshot{}
				w.ready = false
				w.failure = err
				cursor = ""
			} else {
				w.snapshot = snapshot
				w.received = w.now()
				w.ready = true
				w.failure = nil
				cursor = snapshot.Cursor
			}
			w.mu.Unlock()
			select {
			case w.changed <- struct{}{}:
			default:
			}
			// A cursor may change as age metadata changes. Bound reconnect/response rate
			// rather than spinning on repeated immediate snapshots or denial responses.
			delay := 300*time.Millisecond - time.Since(started)
			if delay < 25*time.Millisecond {
				delay = 25 * time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		w.mu.Lock()
		w.snapshot = client.Snapshot{}
		w.ready = false
		w.failure = context.Canceled
		w.mu.Unlock()
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}()
	return w
}
func (w *snapshotWatch) stop() { w.cancel(); <-w.done }
func (w *snapshotWatch) current(ctx context.Context) (client.Snapshot, error) {
	for {
		w.mu.Lock()
		ready, err := w.ready, w.failure
		elapsed := w.now().Sub(w.received)
		if ready && elapsed >= 0 && elapsed < watchSnapshotLease {
			// Deep-copy RawMessages and nested provenance before handing ownership to
			// an evaluator. Monotonic residence ages every original, including repeats.
			raw, e := json.Marshal(w.snapshot)
			var snapshot client.Snapshot
			if e == nil {
				e = json.Unmarshal(raw, &snapshot)
			}
			w.mu.Unlock()
			if e != nil {
				return snapshot, e
			}
			for i := range snapshot.Components {
				snapshot.Components[i].Timing.AgeHint += elapsed
			}
			return snapshot, nil
		}
		w.mu.Unlock()
		if ready {
			return client.Snapshot{}, errors.New("authenticated watch snapshot lease expired")
		}
		if err != nil {
			return client.Snapshot{}, err
		}
		select {
		case <-ctx.Done():
			return client.Snapshot{}, ctx.Err()
		case <-w.changed:
		}
	}
}
func (r *runtime) startWatch(ctx context.Context) {
	if r.cfg.Watch && r.watch == nil {
		g := r.cfg.Public.Spatial.Gas
		r.watch = startSnapshotWatch(ctx, r.p.api, g.Tenant, g.Field.Site)
	}
}
func (r *runtime) stopWatch() {
	if r.watch != nil {
		r.watch.stop()
	}
}
func (r *runtime) snapshot(ctx context.Context) (client.Snapshot, error) {
	if r.watch != nil {
		return r.watch.current(ctx)
	}
	return r.p.api.Query(ctx)
}
