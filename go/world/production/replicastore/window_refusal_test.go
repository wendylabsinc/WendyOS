package replicastore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

func TestRecordWindowRefusalRealRepairRevisitsUnchangedOriginal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	if r, err := f.syncService(t).SyncPeer(ctx, deviceA); err != nil || r.Admitted != 1 {
		t.Fatal(r, err)
	}
	// Retain the already accepted lease and open the same receiver using its
	// own clock. Only the origin advances before signing the new record.
	receiver := &testClock{w: f.clock.w}
	if err := f.b.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.b.cfg.Authority.Close(); err != nil {
		t.Fatal(err)
	}
	f.b.authorityCfg.Clock, f.b.cfg.Clock = receiver, receiver
	var err error
	f.b.cfg.Authority, err = sourceauthority.Open(f.b.authorityCfg)
	if err != nil {
		t.Fatal(err)
	}
	f.b.store, err = Open(f.b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(500 * time.Millisecond)
	raw := f.publish(t)
	syncer := f.syncService(t)
	r, err := syncer.SyncPeer(ctx, deviceA)
	if !errors.Is(err, replication.ErrRecordWindowDeferred) || !errors.Is(err, serviceorigin.ErrExpired) ||
		r.UnknownOutcome || !r.Deferred || r.Complete || r.Admitted != 0 || r.Fetched != 1 || f.b.store.faulted.Load() {
		t.Fatal("future record did not defer without fault", r, err)
	}
	live, err := f.b.store.Snapshot(ctx)
	if err != nil || len(live) != 0 {
		t.Fatal("refused record became visible", live, err)
	}
	receiver.advance(500 * time.Millisecond)
	r, err = syncer.SyncPeer(ctx, deviceA)
	if err != nil || !r.Complete || r.Deferred || r.UnknownOutcome || r.Admitted != 1 {
		t.Fatal(r, err)
	}
	live, err = f.b.store.Snapshot(ctx)
	if err != nil || len(live) != 1 || !bytes.Equal(live[0].Envelope, raw) ||
		live[0].Descriptor.ExpiresAtUnixNS != f.request.Timing.SampleTime.Add(f.request.TTL).UnixNano() {
		t.Fatal("original bytes/deadline changed", live, err)
	}
}

func TestRecordWindowRefusalDoesNotClassifyCallbackOrPostcommitErrors(t *testing.T) {
	for _, post := range []bool{false, true} {
		t.Run(map[bool]string{false: "precommit-callback", true: "postcommit-callback"}[post], func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			f.lease(t)
			if _, err := f.syncService(t).SyncPeer(ctx, deviceA); err != nil {
				t.Fatal(err)
			}
			f.publish(t)
			page, err := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
			if err != nil {
				t.Fatal(err)
			}
			var item replication.Item
			for _, entry := range page.Entries {
				if entry.Kind == replication.Record {
					item, _, err = f.a.store.Fetch(ctx, entry)
				}
			}
			if err != nil || item.Entry.Kind != replication.Record {
				t.Fatal("missing record", err)
			}
			calls := 0
			err = f.b.store.Admit(ctx, item, func(context.Context) error {
				calls++
				if !post || calls == 2 {
					return serviceorigin.ErrExpired
				}
				return nil
			})
			if err == nil || errors.Is(err, replication.ErrRecordWindowDeferred) || errors.Is(err, ErrFaulted) != post {
				t.Fatal("arbitrary check failure became deferrable", err, calls)
			}
		})
	}
}
