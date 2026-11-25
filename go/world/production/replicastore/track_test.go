package replicastore

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testtrack"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

func TestRealTrackRepairOriginalBoundsAndRevocation(t *testing.T) {
	for _, mode := range []string{"sample-expiry", "physical-expiry", "late-arrival", "tombstone", "floor"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixtureComponent(t, resultrecord.ComponentType, "tracks")
			physical, _ := f.clock.Now()
			f.request = testtrack.Request(t, f.scope, f.a.cfg.Replica.Frames[0], "repair", time.Unix(0, f.sample.Load()), physical.Earliest)
			f.lease(t)
			original := f.publish(t)
			if mode == "late-arrival" {
				f.clock.advance(5 * time.Second)
			}
			syncer := f.syncService(t)
			ctx := context.Background()
			result, err := syncer.SyncPeer(ctx, deviceA)
			if err != nil || !result.Complete || result.Admitted != 2 {
				t.Fatal(result, err)
			}
			live, err := f.b.store.Snapshot(ctx)
			if mode == "late-arrival" {
				if err != nil || len(live) != 0 {
					t.Fatal("late original became live", live, err)
				}
			} else if err != nil || len(live) != 1 || !bytes.Equal(live[0].Envelope, original) {
				t.Fatal("original changed or missing", live, err)
			}
			switch mode {
			case "sample-expiry":
				f.sample.Add(int64(time.Second))
			case "physical-expiry":
				f.clock.advance(4 * time.Second)
			case "tombstone":
				f.request.Tombstone, f.request.Payload = true, nil
				f.publish(t)
			case "floor":
				_, err = f.a.cfg.Authority.IssueFloor(ctx, sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.SourceOf(f.scope), 2)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = syncer.SyncPeer(ctx, deviceA); err != nil {
				t.Fatal(err)
			}
			if live, err = f.b.store.Snapshot(ctx); err != nil || len(live) != 0 {
				t.Fatal("expired/revoked track visible", live, err)
			}
			if err = f.b.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.b.store, err = Open(f.b.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if live, err = f.b.store.Snapshot(ctx); err != nil || len(live) != 0 {
				t.Fatal("reopen resurrected track", live, err)
			}
		})
	}
}
