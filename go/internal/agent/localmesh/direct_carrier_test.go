package localmesh

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
	"time"
)

func TestDirectOwnerRunsOnlyOnAdmittedPhysicalBLE(t *testing.T) {
	for _, tc := range []struct {
		name               string
		physical, eligible bool
	}{
		{"diagnostic label only", false, true}, {"physical BLE", true, true}, {"upper owned peer keeps routing carrier", true, false},
	} {
		physical := tc.physical
		t.Run(tc.name, func(t *testing.T) {
			ca, cb := testCredentials(t)
			a := runStaleClockNode(t, t.TempDir(), ca)
			b := runStaleClockNode(t, t.TempDir(), cb)
			admitted := make(chan DirectCarrierAdmission, 2)
			ended := make(chan struct{}, 2)
			handler := func(ctx context.Context, c DirectCarrierAdmission) error {
				defer func() { ended <- struct{}{} }()
				if c.Cost != 256 || c.Link == 0 || c.Self == c.Peer || !c.TLS.HandshakeComplete || c.IdentityDigest == ([32]byte{}) {
					return errors.New("incomplete admission")
				}
				if _, err := c.TLS.ExportKeyingMaterial("test-direct-owner", nil, 32); err != nil {
					return err
				}
				if !c.Deferred {
					ok, err := c.Records.TrySendDirectRecord([]byte{42})
					if err != nil || !ok {
						return errors.New("owner queue unavailable")
					}
					p, err := c.Records.ReceiveDirectRecord(ctx)
					if err != nil {
						return err
					}
					if len(p) != 1 || p[0] != 42 {
						return errors.New("owner demux mismatch")
					}
				}
				admitted <- c
				<-ctx.Done()
				return ctx.Err()
			}
			tlsCap := directstream.CapabilityVersion | directstream.CapabilityTLS
			if err := a.node.SetDirectCarrierHandler(tlsCap|directstream.CapabilityQUIC, handler, func(peer meshid.Device, digest [32]byte) bool {
				if digest == ([32]byte{}) {
					t.Error("missing authenticated identity")
				}
				if peer != cb.Device {
					t.Errorf("eligibility peer mismatch: %v", peer)
				}
				return tc.eligible
			}); err != nil {
				t.Fatal(err)
			}
			if err := b.node.SetDirectCarrierHandler(tlsCap, handler); err != nil {
				t.Fatal(err)
			}
			if a.node.SetDirectCarrierHandler(tlsCap, handler) == nil {
				t.Fatal("replaced session owner")
			}
			marker := func(ctx context.Context) context.Context { return WithCarrier(ctx, "ble") }
			if physical {
				marker = WithDirectBLECarrier
			}
			closeLink := connectStaleClockNodes(t, a.node, b.node, marker)
			if physical {
				var previous *DirectCarrierAdmission
				for range 2 {
					select {
					case c := <-admitted:
						if previous != nil && (previous.IdentityDigest != c.IdentityDigest || previous.Role == c.Role) {
							t.Fatal("peer identity binding differs")
						}
						copy := c
						previous = &copy
						if c.Selected != tlsCap || c.Deferred != !tc.eligible {
							t.Fatal("wrong promotion path")
						}
					case <-time.After(5 * time.Second):
						t.Fatal("owner not admitted")
					}
				}
				closeLink()
				for range 2 {
					select {
					case <-ended:
					case <-time.After(5 * time.Second):
						t.Fatal("owner not joined")
					}
				}
			} else {
				deadline := time.Now().Add(5 * time.Second)
				for a.node.Snapshot().Peers == 0 || b.node.Snapshot().Peers == 0 {
					if time.Now().After(deadline) {
						t.Fatal("legacy carrier not admitted")
					}
					time.Sleep(5 * time.Millisecond)
				}
				closeLink()
				select {
				case <-admitted:
					t.Fatal("label enabled direct mode")
				case <-ended:
					t.Fatal("label started owner")
				default:
				}
			}
		})
	}
}

func TestDirectBLEProvenanceRejectsDiagnosticLabel(t *testing.T) {
	ctx := context.Background()
	if IsDirectBLECarrier(ctx) || IsDirectBLECarrier(WithCarrier(ctx, "ble")) {
		t.Fatal("diagnostic label enabled direct mode")
	}
	if !IsDirectBLECarrier(WithDirectBLECarrier(ctx)) {
		t.Fatal("physical BLE provenance lost")
	}
}
