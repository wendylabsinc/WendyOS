package replicastore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

func TestFloorOnlyRepairNeedsFreshAttestationForNewReceiver(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	floor, e := f.a.cfg.Authority.IssueFloor(ctx, sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.SourceOf(f.scope), 2)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := sourceauthority.EncodeFloor(floor)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := f.syncService(t).SyncPeer(ctx, deviceA); e != nil || !r.Complete || r.Admitted != 1 {
		t.Fatal("independent floor-only repair failed", r, e)
	}
	rows, e := f.b.cfg.Authority.Snapshot(ctx)
	if e != nil || len(rows) != 1 || !bytes.Equal(rows[0].Floor, raw) {
		t.Fatal(rows, e)
	}
	f.clock.advance(31 * time.Second)
	// Explicitly commission another new empty receiver, retaining the old
	// instance's files. Opening missing history is never used as initialization.
	_ = f.b.store.Close()
	_ = f.b.cfg.Authority.Close()
	f.b.authorityCfg.Path += ".fresh"
	f.b.cfg.Authority, e = sourceauthority.Create(f.b.authorityCfg)
	if e != nil {
		t.Fatal(e)
	}
	f.b.cfg.RemotePath += ".fresh"
	f.b.store, e = Create(f.b.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := f.syncService(t).SyncPeer(ctx, deviceA); e == nil || r.Admitted != 0 {
		t.Fatal("new receiver accepted expired floor signature", r, e)
	}
	refreshed, e := f.a.cfg.Authority.RefreshFloor(ctx, sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.SourceOf(f.scope))
	if e != nil {
		t.Fatal(e)
	}
	fresh, e := sourceauthority.EncodeFloor(refreshed)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := f.syncService(t).SyncPeer(ctx, deviceA); e != nil || !r.Complete || r.Admitted != 1 {
		t.Fatal(r, e)
	}
	rows, e = f.b.cfg.Authority.Snapshot(ctx)
	if e != nil || len(rows) != 1 || rows[0].MinimumEpoch != 2 || rows[0].ThroughRevision == 0 || rows[0].FloorRevision <= rows[0].ThroughRevision || !bytes.Equal(rows[0].Floor, fresh) {
		t.Fatal(rows, e)
	}
}

func TestDefiniteForgedLeaseRefusesWithoutFaultingHealthyStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
	if e != nil {
		t.Fatal(e)
	}
	item, _, e := f.a.store.Fetch(ctx, page.Entries[0])
	if e != nil {
		t.Fatal(e)
	}
	signed, _, e := sourceauthority.DecodeLease(item.Envelope)
	if e != nil {
		t.Fatal(e)
	}
	signed.Signature[0] ^= 1
	item.Envelope, e = sourceauthority.EncodeLease(signed)
	if e != nil {
		t.Fatal(e)
	}
	item.Entry.Authority.EnvelopeSHA256 = hash(item.Envelope)
	if e = f.b.store.Admit(ctx, item, func(context.Context) error { return nil }); e == nil || errors.Is(e, ErrFaulted) {
		t.Fatal(e)
	}
	if f.b.store.faulted.Load() {
		t.Fatal("known precommit rejection faulted adapter")
	}
	states, e := f.b.cfg.Authority.Snapshot(ctx)
	if e != nil || len(states) != 0 {
		t.Fatal(states, e)
	}
}

func TestAuthorityExpiresWhilePostCommitCheckBlocks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
	if e != nil {
		t.Fatal(e)
	}
	item, _, e := f.a.store.Fetch(ctx, page.Entries[0])
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	e = f.b.store.Admit(ctx, item, func(context.Context) error {
		calls++
		if calls == 2 {
			f.clock.advance(31 * time.Second)
		}
		return nil
	})
	if !errors.Is(e, ErrFaulted) || calls != 2 {
		t.Fatal("expired postcommit authority acknowledged", e, calls)
	}
	states, e := f.b.cfg.Authority.Snapshot(ctx)
	if e != nil || len(states) != 1 || !bytes.Equal(states[0].Lease, item.Envelope) {
		t.Fatal("test did not commit before expiry", states, e)
	}
}

func TestExactAuthorityHintAndExpiredOperationContext(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
	if e != nil {
		t.Fatal(e)
	}
	item, _, e := f.a.store.Fetch(ctx, page.Entries[0])
	if e != nil {
		t.Fatal(e)
	}
	var retained context.Context
	if e = f.b.store.Admit(ctx, item, func(ctx context.Context) error { retained = ctx; return nil }); e != nil {
		t.Fatal(e)
	}
	changed := item.Entry
	changed.Authority.Epoch++
	if need, e := f.b.store.Need(ctx, changed); e != nil || !need {
		t.Fatal("same hash hid mismatched descriptor", need, e)
	}
	before := 0
	old := f.b.cfg.Guard
	f.b.store.cfg.Guard = func(ctx context.Context, fn func() error) error { before++; return old(ctx, fn) }
	if e = f.b.store.RemoteAuthority(retained, item); e == nil || before != 0 {
		t.Fatal("retained context reacquired or bypassed guard", e, before)
	}
	if e = f.a.store.RemoteAuthority(retained, item); e == nil {
		t.Fatal("another adapter accepted context")
	}
	if e = f.b.store.journalGuard(retained, func() error { t.Fatal("late callback executed"); return nil }); e == nil {
		t.Fatal("late journal guard accepted")
	}
}

func TestMandatoryHostSeamsAndBoundedSelectors(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []string{"guard", "authorize", "schema", "generation", "binding", "selectors", "domain"} {
		t.Run(mode, func(t *testing.T) {
			c := f.b.cfg
			c.RemotePath += "." + mode
			switch mode {
			case "guard":
				c.Guard = nil
			case "authorize":
				c.Authorize = nil
			case "schema":
				c.AdmitSchema = nil
			case "generation":
				c.Generation = 0
			case "binding":
				c.Peer.BindingSHA256 = ""
			case "selectors":
				c.Import = append(c.Import, c.Import[0])
			case "domain":
				c.Endpoint.ExecutionDomain = "physical"
				c.Replica.AllowSynthetic = false
			}
			if s, e := Create(c); e == nil {
				_ = s.Close()
				t.Fatal("missing authority seam accepted")
			}
		})
	}
	if _, e := f.a.store.Inventory(context.Background(), f.a.cfg.Export, "", replication.MaxInventoryEntries+1); e == nil {
		t.Fatal("inventory limit ignored")
	}
}
