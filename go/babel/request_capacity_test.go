package babel

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestNewerSeqRequestCanReplacePendingRequestAtCapacity(t *testing.T) {
	e, err := New(Config{RouterID: 1, MaxRequests: 1, HelloInterval: time.Second, UpdateInterval: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for id := LinkID(1); id <= 2; id++ {
		step(t, e, 0, AddLink{testLink(id)})
		reachable(t, e, id)
	}
	prefix := netip.MustParsePrefix("fd00::9/128")
	other := netip.MustParsePrefix("fd00::10/128")
	for _, p := range []netip.Prefix{prefix, other} {
		receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 9, Seqno: 7, Interval: 400})
	}
	step(t, e, 100*time.Millisecond, Tick{})

	forwarded := func(p netip.Prefix, seq uint16) []wire.Message {
		t.Helper()
		packet := wire.Pack([]wire.Message{{Type: wire.SeqRequest, Prefix: p, RouterID: 9, Seqno: seq, HopCount: 5}}, 512)[0]
		fx := step(t, e, 100*time.Millisecond, Receive{2, packet})
		var requests []wire.Message
		for _, d := range fx.Datagrams {
			messages, err := wire.Decode(d.Payload, testLink(d.Link).Local)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range messages {
				if m.Type == wire.SeqRequest {
					if d.Link != 1 || m.HopCount != 4 {
						t.Fatalf("request forwarded to wrong neighbour or with wrong hop count: %+v", d)
					}
					requests = append(requests, m)
				}
			}
		}
		return requests
	}
	for _, seq := range []uint16{8, 9} {
		requests := forwarded(prefix, seq)
		if len(requests) != 1 || requests[0].Seqno != seq {
			t.Fatalf("sequence %d did not progress through a full request table: %+v", seq, requests)
		}
		if e.Snapshot().PendingRequests != 1 {
			t.Fatal("replacement changed the request admission bound")
		}
	}
	if requests := forwarded(other, 8); len(requests) != 0 {
		t.Fatalf("new source exceeded request admission bound: %+v", requests)
	}
	if requests := forwarded(prefix, 9); len(requests) != 0 {
		t.Fatalf("duplicate request was forwarded: %+v", requests)
	}
}
