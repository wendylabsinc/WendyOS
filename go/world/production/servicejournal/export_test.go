package servicejournal

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func TestExportLocalHistoricalPaginationAndExactFetch(t *testing.T) {
	f := remoteSetup(t)
	expected := map[string][]byte{}
	for _, entity := range []string{"first", "second", "tombstone"} {
		f.request.Entity = entity
		if entity == "tombstone" {
			f.request.Tombstone = true
			f.request.Payload = nil
		}
		signed := f.sign(t)
		f.admit(t, signed, world.Applied)
		raw, _ := encode(signed)
		expected[entity] = raw
	}
	f.sourceNow = f.sourceNow.Add(3 * time.Second)
	if live, err := f.j.Snapshot(context.Background()); err != nil || len(live) != 0 {
		t.Fatal("setup not expired", err)
	}
	access := exportAccess(f)
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		page, err := f.j.ExportInventory(context.Background(), []proofref.Source{f.source()}, cursor, 1, access)
		if err != nil || len(page.Entries) != 1 || page.More != (i < 2) {
			t.Fatal("bad bounded page", i, page, err)
		}
		d := page.Entries[0]
		if d.OrderKey() <= cursor || seen[d.Reference.Key.Entity] {
			t.Fatal("unstable key pagination")
		}
		cursor = d.OrderKey()
		seen[d.Reference.Key.Entity] = true
		raw, found, err := f.j.ExportFetch(context.Background(), d, access)
		if err != nil || !found || !bytes.Equal(raw, expected[d.Reference.Key.Entity]) || d.Tombstone != (d.Reference.Key.Entity == "tombstone") {
			t.Fatal("historical bytes/tombstone changed", err)
		}
		d.ExpiresAtUnixNS--
		if raw, found, err = f.j.ExportFetch(context.Background(), d, access); err != nil || found || raw != nil {
			t.Fatal("fetch ignored exact cap", err)
		}
	}
	page, err := f.j.ExportInventory(context.Background(), []proofref.Source{f.source()}, cursor, 64, access)
	if err != nil || len(page.Entries) != 0 || page.More {
		t.Fatal("nonterminal cursor", page, err)
	}
	f.allowed = false
	if _, err = f.j.ExportInventory(context.Background(), []proofref.Source{f.source()}, cursor, 64, access); !errors.Is(err, serviceorigin.ErrDenied) {
		t.Fatal("empty page bypassed peer permission", err)
	}
}

func TestExportPermissionAfterBlockingWorkAndBounds(t *testing.T) {
	f := remoteSetup(t)
	f.admit(t, f.sign(t), world.Applied)
	for _, mode := range []string{"final-expired", "final-denied", "missing-window", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			access := exportAccess(f)
			original := access.Authorize
			access.Authorize = func(ctx context.Context, s []proofref.Source, w trustedtime.Interval) (AuthorityWindow, error) {
				calls++
				window, err := original(ctx, s, w)
				if mode == "missing-window" {
					return AuthorityWindow{}, nil
				}
				if calls == 2 {
					switch mode {
					case "final-denied":
						return AuthorityWindow{}, serviceorigin.ErrDenied
					case "final-expired":
						window.NotAfter = w.Latest.Add(time.Second)
						f.clock.advance(2 * time.Second)
					case "canceled":
						cancel()
					}
				}
				return window, err
			}
			page, err := f.j.ExportInventory(ctx, []proofref.Source{f.source()}, "", 1, access)
			if err == nil || len(page.Entries) != 0 {
				t.Fatal("post-read permission failure released data", mode, err)
			}
		})
	}
	access := exportAccess(f)
	for _, limit := range []int{0, 65} {
		if _, err := f.j.ExportInventory(context.Background(), []proofref.Source{f.source()}, "", limit, access); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	for _, cursor := range []string{"not-a-cursor", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := f.j.ExportInventory(context.Background(), []proofref.Source{f.source()}, cursor, 1, access); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	if _, err := f.j.ExportInventory(context.Background(), []proofref.Source{f.source(), f.source()}, "", 1, access); err == nil {
		t.Fatal("duplicate source accepted")
	}
}
