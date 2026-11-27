package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
)

func TestTotalWireByteBudgetStopsBeforeFurtherAdmissions(t *testing.T) {
	items := make([]Item, 12)
	for n := range items {
		i := recordItem(t, fmt.Sprintf("large-%02d", n), 1)
		var signed serviceorigin.SignedRecord
		var record serviceorigin.Record
		_ = json.Unmarshal(i.Envelope, &signed)
		_ = json.Unmarshal(signed.Record, &record)
		record.Publication.Payload = []byte(`{"test_padding":"` + strings.Repeat("x", 30<<10) + `"}`)
		signed.Record, _ = json.Marshal(record)
		i.Envelope, _ = json.Marshal(signed)
		i.Entry.Reference.RecordSHA256 = hash(signed.Record)
		i.Entry.Reference.EnvelopeSHA256 = hash(i.Envelope)
		items[n] = i
	}
	f := setup(t, items...)
	f.b.cfg.Limits.MaxBytes = MaxPageBytes
	r, err := f.b.SyncPeer(context.Background(), deviceA)
	if !errors.Is(err, ErrCapacity) || r.Complete || r.Bytes > MaxPageBytes || r.Admitted == 0 || r.Admitted >= len(items) || r.UnknownOutcome {
		t.Fatal(r, err)
	}
}

func TestNoStoreOrPolicyFenceCrossesDialOrRPC(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	var policy sync.Mutex
	f.b.cfg.Current = func(context.Context, Identity, Direction, uint64) error {
		policy.Lock()
		defer policy.Unlock()
		return nil
	}
	assertUnlocked := func() {
		if !policy.TryLock() {
			t.Fatal("policy lock crossed a network callback")
		}
		policy.Unlock()
		if !f.local.mu.TryLock() {
			t.Fatal("local store lock crossed a network callback")
		}
		f.local.mu.Unlock()
		if !f.b.mu.TryLock() {
			t.Fatal("repair bookkeeping lock crossed network callback")
		}
		f.b.mu.Unlock()
	}
	f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
		assertUnlocked()
		return Connection{identity(deviceA), f.client, f.closed}, nil
	}
	f.client.inventory = func(*replicav1.InventoryReply) { assertUnlocked() }
	f.client.fetch = func(*replicav1.FetchReply) { assertUnlocked() }
	if r, e := f.b.SyncPeer(context.Background(), deviceA); e != nil || !r.Complete {
		t.Fatal(r, e)
	}
}

func TestForeignPeerAndWrongBindingCannotConsumeAnotherCursor(t *testing.T) {
	f := setup(t)
	for _, mode := range []string{"wrong-device", "missing-binding", "nil-client", "nil-close"} {
		t.Run(mode, func(t *testing.T) {
			f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
				c := Connection{identity(deviceA), f.client, f.closed}
				switch mode {
				case "wrong-device":
					c.Identity.Device = deviceC
				case "missing-binding":
					c.Identity.BindingSHA256 = ""
				case "nil-client":
					c.Client = nil
				case "nil-close":
					c.Close = nil
				}
				return c, nil
			}
			if r, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrDenied) || r.Complete {
				t.Fatal(r, e)
			}
		})
	}
	if f.client.calls.Load() != 0 {
		t.Fatal("unbound transport reached peer methods")
	}
}

func TestServerRejectsUnknownRequestsBeforeAuthenticatingAndLimitsReply(t *testing.T) {
	item := recordItem(t, "gas", 1)
	f := setup(t, item)
	calls := 0
	f.a.cfg.IncomingPeer = func(context.Context) (Identity, error) { calls++; return identity(deviceB), nil }
	request := &replicav1.InventoryRequest{Version: Version}
	request.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, e := f.a.Inventory(context.Background(), request); e == nil {
		t.Fatal("unknown request accepted")
	}
	if calls != 0 {
		t.Fatal("malformed request entered authentication")
	}
	fetch := &replicav1.FetchRequest{Version: Version, ExpectedGeneration: 1}
	for n := 0; n < 9; n++ {
		fetch.Entries = append(fetch.Entries, entryWire(item.Entry))
	}
	if _, e := f.a.Fetch(context.Background(), fetch); e == nil {
		t.Fatal("over quota fetch accepted")
	}
	if calls != 0 {
		t.Fatal("over quota request entered authentication")
	}
	fetch.Entries = fetch.Entries[:1]
	page, e := f.a.Fetch(context.Background(), fetch)
	if e != nil || proto.Size(page) > MaxPageBytes {
		t.Fatal(page, e)
	}
}

func TestCursorCycleAndPolicyGenerationChangeRefuseLaterPage(t *testing.T) {
	items := make([]Item, 65)
	for n := range items {
		items[n] = recordItem(t, fmt.Sprintf("entity-%02d", n), 1)
	}
	for _, mode := range []string{"cycle", "generation"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, items...)
			var first []byte
			calls := 0
			f.client.inventory = func(p *replicav1.InventoryReply) {
				calls++
				if calls == 1 {
					first = append([]byte(nil), p.NextCursor...)
					return
				}
				if mode == "cycle" {
					p.NextCursor = first
				} else {
					p.Generation++
				}
			}
			r, e := f.b.SyncPeer(context.Background(), deviceA)
			if !errors.Is(e, ErrProtocol) || r.Complete || calls != 2 {
				t.Fatal(r, e, calls)
			}
		})
	}
}

func TestCallbacksCannotMutateOriginalAdmissionBytes(t *testing.T) {
	i := recordItem(t, "gas", 1)
	f := setup(t, i)
	checks := 0
	f.b.cfg.RemoteAuthority = func(_ context.Context, copy Item) error { checks++; copy.Envelope[0] ^= 1; return nil }
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || r.Admitted != 1 || checks != 2 {
		t.Fatal(r, e, checks)
	}
	if string(f.local.admitted[0].Envelope) != string(i.Envelope) {
		t.Fatal("authorizer mutated retained bytes")
	}
}
