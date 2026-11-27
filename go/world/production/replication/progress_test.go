package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func sortedItems(t *testing.T, n int) []Item {
	t.Helper()
	items := make([]Item, n)
	for i := range items {
		items[i] = recordItem(t, fmt.Sprintf("progress-%03d", i), 1)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Entry.OrderKey() < items[j].Entry.OrderKey() })
	return items
}
func TestContinuationReachesSuffixAndWrapsForEarlierKeys(t *testing.T) {
	items := sortedItems(t, 6)
	f := setup(t, items[1:]...)
	f.b.cfg.Limits.MaxEntries = 2
	for round, want := range []int{2, 2, 1} {
		r, e := f.b.SyncPeer(context.Background(), deviceA)
		if r.Admitted != want || r.Entries != want || (round < 2 && !errors.Is(e, ErrCapacity)) || (round == 2 && (e != nil || !r.Complete)) {
			t.Fatal(round, r, e)
		}
	}
	if len(f.local.items) != 5 || len(f.b.progress) != 0 {
		t.Fatal("suffix was not repaired or cursor did not wrap")
	}
	f.remote.items[items[0].Entry.OrderKey()] = items[0]
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if !errors.Is(e, ErrCapacity) || r.Admitted != 1 || f.local.admitted[5].Entry != items[0].Entry {
		t.Fatal("new earlier key skipped", r, e)
	}
}
func TestPageQuotaContinuesAndFreshServiceStartsAtPrefix(t *testing.T) {
	items := sortedItems(t, 65)
	f := setup(t, items...)
	f.b.cfg.Limits.MaxPages = 1
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if !errors.Is(e, ErrCapacity) || r.Admitted != 64 {
		t.Fatal(r, e)
	}
	r, e = f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || !r.Complete || r.Admitted != 1 || r.Entries != 1 {
		t.Fatal(r, e)
	}
	replacement, e := New(f.b.cfg)
	if e != nil {
		t.Fatal(e)
	}
	r, e = replacement.SyncPeer(context.Background(), deviceA)
	if !errors.Is(e, ErrCapacity) || r.Entries != 64 || r.Admitted != 0 {
		t.Fatal("new process retained cursor", r, e)
	}
}
func TestContinuationPinsPolicyBindingAndCursorLifetime(t *testing.T) {
	for _, mode := range []string{"local-policy", "remote-policy", "credential", "server-restart", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, sortedItems(t, 3)...)
			f.b.cfg.Limits.MaxEntries = 1
			if _, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) {
				t.Fatal(e)
			}
			saved := f.b.progress[deviceA]
			if len(saved.Cursor) == 0 {
				t.Fatal("no continuation")
			}
			rejects := false
			switch mode {
			case "local-policy":
				f.generation.Store(2)
			case "remote-policy":
				f.a.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
					return Access{2, []proofref.Source{source(deviceA)}}, nil
				}
				f.a.cfg.Current = func(_ context.Context, _ Identity, _ Direction, g uint64) error {
					if g != 2 {
						return ErrDenied
					}
					return nil
				}
				rejects = true
			case "credential":
				f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
					p := identity(deviceA)
					p.BindingSHA256 = strings.Repeat("b", 64)
					return Connection{p, f.client, f.closed}, nil
				}
			case "server-restart":
				s, e := New(f.a.cfg)
				if e != nil {
					t.Fatal(e)
				}
				f.client.server = s
				rejects = true
			case "expiry":
				f.b.elapsed = func() time.Duration { return saved.Expires }
			}
			r, e := f.b.SyncPeer(context.Background(), deviceA)
			if rejects {
				if status.Code(e) != codes.PermissionDenied && status.Code(e) != codes.InvalidArgument {
					t.Fatal(r, e)
				}
				if len(f.b.progress) != 0 {
					t.Fatal("refused cursor retained")
				}
				r, e = f.b.SyncPeer(context.Background(), deviceA)
			}
			if !errors.Is(e, ErrCapacity) || r.Entries != 1 || r.Admitted != 0 {
				t.Fatal("did not restart at already repaired prefix", r, e)
			}
		})
	}
}
func TestContinuationUncertainOrProtocolFailureDropsSavedPage(t *testing.T) {
	for _, mode := range []string{"uncertain", "protocol"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, sortedItems(t, 3)...)
			f.b.cfg.Limits.MaxEntries = 1
			if _, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) {
				t.Fatal(e)
			}
			if mode == "uncertain" {
				f.local.admit = func(ctx context.Context, i Item, check func(context.Context) error) error {
					if e := check(ctx); e != nil {
						return e
					}
					f.local.items[i.Entry.OrderKey()] = i
					return errors.New("commit acknowledgement lost")
				}
			} else {
				f.client.inventory = func(p *replicav1.InventoryReply) { p.Generation++ }
			}
			r, e := f.b.SyncPeer(context.Background(), deviceA)
			if e == nil || len(f.b.progress) != 0 || r.UnknownOutcome != (mode == "uncertain") {
				t.Fatal(r, e)
			}
		})
	}
}
func TestContinuationCancellationNeverSavesPartialPage(t *testing.T) {
	f := setup(t, sortedItems(t, 4)...)
	f.b.cfg.Limits.MaxEntries = 1
	if _, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	saved := f.b.progress[deviceA]
	ctx, cancel := context.WithCancel(context.Background())
	f.client.fetch = func(*replicav1.FetchReply) { cancel() }
	r, e := f.b.SyncPeer(ctx, deviceA)
	if !errors.Is(e, context.Canceled) || r.Admitted != 0 || !bytes.Equal(saved.Cursor, f.b.progress[deviceA].Cursor) {
		t.Fatal(r, e)
	}
	f.client.fetch = nil
	r, e = f.b.SyncPeer(context.Background(), deviceA)
	if !errors.Is(e, ErrCapacity) || r.Admitted != 1 {
		t.Fatal("failed to resume completed boundary", r, e)
	}
}

func TestCanceledPolicyCheckKeepsOnlyCompletedContinuation(t *testing.T) {
	for _, mode := range []string{"authorize", "current", "remote-authority"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, sortedItems(t, 3)...)
			f.b.cfg.Limits.MaxEntries = 1
			if _, err := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(err, ErrCapacity) {
				t.Fatal(err)
			}
			saved := f.b.progress[deviceA]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "authorize":
				f.b.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) { cancel(); return Access{}, ctx.Err() }
			case "current":
				f.b.cfg.Current = func(context.Context, Identity, Direction, uint64) error { cancel(); return ctx.Err() }
			case "remote-authority":
				f.b.cfg.RemoteAuthority = func(context.Context, Item) error { cancel(); return ctx.Err() }
			}
			r, err := f.b.SyncPeer(ctx, deviceA)
			if !errors.Is(err, context.Canceled) || r.Admitted != 0 || r.UnknownOutcome || !bytes.Equal(saved.Cursor, f.b.progress[deviceA].Cursor) {
				t.Fatal(r, err)
			}
		})
	}
}
func TestUnavailableAcrossContinuationPreventsFalseCompletion(t *testing.T) {
	f := setup(t, sortedItems(t, 2)...)
	f.b.cfg.Limits.MaxEntries = 1
	f.client.fetch = func(p *replicav1.FetchReply) { p.Unavailable = []*replicav1.Entry{p.Items[0].Entry}; p.Items = nil }
	if r, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) || !r.Deferred {
		t.Fatal(r, e)
	}
	f.client.fetch = nil
	if r, e := f.b.SyncPeer(context.Background(), deviceA); e != nil || r.Complete || !r.Deferred || r.Unavailable != 0 || r.Admitted != 1 {
		t.Fatal(r, e)
	}
	if len(f.b.progress) != 0 {
		t.Fatal("did not wrap after deferred pass")
	}
}
func TestContinuationStorageIsBoundedAndExpires(t *testing.T) {
	f := setup(t)
	f.b.cfg.Limits.MaxPeers = 1
	f.b.elapsed = func() time.Duration { return time.Second }
	last := recordItem(t, "gas", 1).Entry.OrderKey()
	if e := f.b.saveProgress(identity(deviceA), 1, 1, []byte("cursor"), last, 0, false); e != nil {
		t.Fatal(e)
	}
	if e := f.b.saveProgress(identity(deviceC), 1, 1, []byte("cursor"), last, 0, false); !errors.Is(e, ErrCapacity) || len(f.b.progress) != 1 {
		t.Fatal(e)
	}
	f.b.elapsed = func() time.Duration { return 2 * time.Minute }
	if e := f.b.saveProgress(identity(deviceC), 1, 1, []byte("cursor"), last, 2*time.Minute, false); e != nil || len(f.b.progress) != 1 {
		t.Fatal(e)
	}
	if len(f.b.loadProgress(identity(deviceC), 2).Cursor) != 0 || len(f.b.progress) != 0 {
		t.Fatal("old local generation retained")
	}
}
func TestTombstoneHintBindsExactOriginal(t *testing.T) {
	item := recordItem(t, "gas", 2)
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	_ = json.Unmarshal(item.Envelope, &signed)
	_ = json.Unmarshal(signed.Record, &record)
	record.Publication.Tombstone = true
	record.Publication.Payload = nil
	signed.Record, _ = json.Marshal(record)
	item.Envelope, _ = json.Marshal(signed)
	item.Entry.Reference.RecordSHA256 = hash(signed.Record)
	item.Entry.Reference.EnvelopeSHA256 = hash(item.Envelope)
	item.Entry.Tombstone = true
	f := setup(t, item)
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || !r.Complete || r.Admitted != 1 || !f.local.admitted[0].Entry.Tombstone || !bytes.Equal(item.Envelope, f.local.admitted[0].Envelope) {
		t.Fatal(r, e)
	}
	item.Entry.Tombstone = false
	if ValidateItem(item, endpoint(deviceA), deviceA) == nil {
		t.Fatal("changed tombstone hint accepted")
	}
}
