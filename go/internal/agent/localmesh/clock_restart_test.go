package localmesh

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"github.com/wendylabsinc/wendy/go/internal/testfixture/clockproof"
)

func TestClockRecoveryPreviouslyJoinedRestart(t *testing.T) {
	for _, test := range []struct {
		skew      time.Duration
		transient bool
	}{
		{6 * time.Second, false}, {598 * time.Second, false}, {20 * time.Minute, false}, {598 * time.Second, true},
	} {
		t.Run(fmt.Sprintf("%s/transient=%t", test.skew, test.transient), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			staleCredentials, healthyCredentials := testCredentials(t)
			dir := t.TempDir()
			var offset atomic.Int64
			wall := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
			newNode := func(ctx context.Context, dir string, credentials *Credentials, name string, wall func() time.Time) *Node {
				t.Helper()
				n, err := newNodeWithClock(ctx, dir, credentials, name, 50052, func(meshid.Device) (PacketNetwork, error) { return &clockPacketNetwork{}, nil }, wall)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			healthy := newNode(ctx, t.TempDir(), healthyCredentials, "healthy", nil)
			authorities := make([]clockproof.Authority, len(roughtime.Servers))
			servers := make([]roughtime.Server, len(authorities))
			for i := range authorities {
				authorities[i] = clockproof.New(t, fmt.Sprintf("restart%d", i), time.Now())
				servers[i] = authorities[i].Server
			}
			var authorityRequests atomic.Int32
			healthy.clockQuery = func(ctx context.Context, index uint8, nonce []byte) ([]byte, error) {
				if authorityRequests.Add(1) <= int32(len(authorities)) && test.transient {
					return nil, errors.New("temporary authority outage")
				}
				return authorities[index].ReplyAt(nonce, time.Now()), nil
			}
			healthy.clockSync = func(context.Context, roughtime.RelayQuery) error {
				return errors.New("healthy clock must not need recovery")
			}
			healthyDone := make(chan error, 1)
			go func() { healthyDone <- healthy.Run() }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-healthyDone:
				case <-time.After(time.Second):
					t.Error("healthy Node did not stop")
				}
			})
			oldCtx, oldCancel := context.WithCancel(ctx)
			old := newNode(oldCtx, dir, staleCredentials, "restart", wall)
			oldDone := make(chan error, 1)
			go func() { oldDone <- old.Run() }()
			clockWait(t, "initial own leases", func() bool { return len(old.Snapshot().Devices) == 1 && len(healthy.Snapshot().Devices) == 1 })
			clockAttachPair(t, ctx, old, healthy)
			clockWait(t, "initial mutual admission", func() bool { return len(old.Snapshot().Devices) == 2 && len(healthy.Snapshot().Devices) == 2 })
			oldCancel()
			select {
			case <-oldDone:
			case <-ctx.Done():
				t.Fatal("old Node did not stop")
			}
			clockWait(t, "old carrier removal", func() bool { return healthy.Snapshot().Peers == 0 })
			offset.Store(int64(-test.skew))
			restarted := newNode(ctx, dir, staleCredentials, "restart", wall)
			var originalReceipt DirectoryReceipt
			for _, receipt := range restarted.state.Receipts {
				if receipt.Device == healthyCredentials.Device.Key() {
					originalReceipt = receipt
				}
			}
			if originalReceipt.Revision == 0 {
				t.Fatal("fixture did not retain healthy replay floor")
			}
			ready, release := make(chan struct{}), make(chan struct{})
			failed := make(chan struct{}, 1)
			var attempts atomic.Int32
			restarted.clockSync = func(ctx context.Context, relay roughtime.RelayQuery) error {
				attempts.Add(1)
				consensus, err := timesync.QueryConsensusVia(ctx, servers, relay)
				if err != nil {
					failed <- struct{}{}
					return err
				}
				if consensus.Quorum < 2 {
					failed <- struct{}{}
					return errors.New("missing authenticated quorum")
				}
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				boot, err := timesync.BootTime()
				if err != nil {
					return err
				}
				target := time.Unix(0, boot+consensus.LowerOffsetNanos+(consensus.UpperOffsetNanos-consensus.LowerOffsetNanos)/2)
				offset.Store(int64(target.Sub(time.Now())))
				return nil
			}
			restartedDone := make(chan error, 1)
			restartedStopped := false
			go func() { restartedDone <- restarted.Run() }()
			t.Cleanup(func() {
				cancel()
				if restartedStopped {
					return
				}
				select {
				case <-restartedDone:
				case <-time.After(time.Second):
					t.Error("restarted Node did not stop")
				}
			})
			clockWait(t, "restarted own lease", func() bool { return len(restarted.Snapshot().Devices) == 1 })
			ended := clockAttachPair(t, ctx, restarted, healthy)
			if test.transient {
				select {
				case <-failed:
				case <-ctx.Done():
					t.Fatal("first failed quorum did not finish")
				}
				if len(restarted.Snapshot().Devices) != 1 || offset.Load() == 0 {
					t.Fatal("failed quorum admitted peer or changed UTC")
				}
				select {
				case err := <-ended:
					t.Fatalf("failed first query closed carrier: %v", err)
				default:
				}
			}
			select {
			case <-ready:
			case err := <-ended:
				t.Fatalf("restarted carrier ended: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("previously joined coldboot did not start authority recovery")
			}
			if len(restarted.Snapshot().Devices) != 1 {
				t.Fatal("future replay restored route before clock recovery")
			}
			wantAttempts := int32(1)
			if test.transient {
				wantAttempts = 2
			}
			if attempts.Load() != wantAttempts {
				t.Fatalf("authority rounds=%d want=%d", attempts.Load(), wantAttempts)
			}
			close(release)
			clockWait(t, "restarted normal admission", func() bool { return len(restarted.Snapshot().Devices) == 2 && len(healthy.Snapshot().Devices) == 2 })
			// Stop before reading actor-owned replay state. A restored identical
			// lease must keep its original receipt and expiry, not renew either.
			cancel()
			select {
			case <-restartedDone:
				restartedStopped = true
			case <-time.After(time.Second):
				t.Fatal("restarted Node did not stop")
			}
			if got := restarted.directory.history[originalReceipt.Device]; got != originalReceipt {
				t.Fatalf("clock recovery replaced persisted replay receipt: got=%+v want=%+v", got, originalReceipt)
			}
		})
	}
}
