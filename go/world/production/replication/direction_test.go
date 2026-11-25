package replication

import (
	"context"
	"testing"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
)

func TestEmptyDirectionDoesNotScanOrFetch(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	f.a.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
		return Access{Generation: 1}, nil
	}
	f.remote.inventory = func() { t.Fatal("empty export scanned store") }
	page, err := f.a.Inventory(context.Background(), &replicav1.InventoryRequest{Version: Version, Limit: 1})
	if err != nil || len(page.Entries) != 0 || len(page.NextCursor) != 0 || page.Generation != 1 {
		t.Fatal(page, err)
	}
	if out, err := f.a.Fetch(context.Background(), &replicav1.FetchRequest{Version: Version, ExpectedGeneration: 1, Entries: []*replicav1.Entry{entryWire(recordItem(t, "gas", 1).Entry)}}); err == nil || out != nil {
		t.Fatal("empty export granted a requested source")
	}
	f.b.cfg.Authorize = f.a.cfg.Authorize
	result, err := f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || !result.Complete || result.Pages != 0 || f.client.calls.Load() != 0 {
		t.Fatal(result, err)
	}
	f.generation.Store(2)
	if _, err := f.b.SyncPeer(context.Background(), deviceA); err == nil {
		t.Fatal("empty direction bypassed current policy")
	}
}
