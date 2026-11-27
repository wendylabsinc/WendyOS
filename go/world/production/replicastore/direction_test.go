package replicastore

import (
	"bytes"
	"context"
	"testing"
)

func TestRealStoreOneWayRepair(t *testing.T) {
	f := newFixture(t)
	f.a.cfg.Import = nil
	f.b.cfg.Export = nil
	for _, n := range []*node{f.a, f.b} {
		if err := n.store.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		n.store, err = Open(n.cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.lease(t)
	raw := f.publish(t)
	result, err := f.syncService(t).SyncPeer(context.Background(), deviceA)
	if err != nil || !result.Complete || result.Admitted != 2 {
		t.Fatal(result, err)
	}
	values, err := f.b.store.Snapshot(context.Background())
	if err != nil || len(values) != 1 || !bytes.Equal(values[0].Envelope, raw) {
		t.Fatal(values, err)
	}
	page, err := f.b.store.Inventory(context.Background(), nil, "", 1)
	if err != nil || len(page.Entries) != 0 || page.More {
		t.Fatal(page, err)
	}
	if _, err := f.a.store.Inventory(context.Background(), nil, "", 1); err == nil {
		t.Fatal("empty request bypassed configured export scope")
	}
	cfg := f.b.cfg
	cfg.Import = nil
	cfg.RemotePath += ".empty"
	if s, err := Create(cfg); err == nil {
		_ = s.Close()
		t.Fatal("meaningless peer accepted")
	}
}
