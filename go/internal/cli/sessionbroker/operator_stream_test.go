//go:build !windows

package sessionbroker

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// A normal slow destination fills the bounded HTTP/2 window after the final
// pipe chunk is accepted. Half-close must still obey the socket's deadline.
func TestOperatorStreamNormalHalfCloseHonorsStalledWriteDeadline(t *testing.T) {
	for _, updateDuringClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline_during_close_%t", updateDuringClose), func(t *testing.T) {
			r, cc, activity, _ := liteManagementFixture(t)
			_, _, target := remoteTopologyFixture(t)
			var peers []net.Conn
			r.packetDial = func(context.Context, string, string) (net.Conn, error) {
				local, peer := net.Pipe()
				peers = append(peers, peer)
				return local, nil
			}
			t.Cleanup(func() {
				for _, peer := range peers {
					_ = peer.Close()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			flow, err := openOperatorTCP(ctx, cc, MeshSelector(target), 43022, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			_ = flow.SetWriteDeadline(time.Now().Add(150 * time.Millisecond))
			payload := make([]byte, operatorLiteChunk)
			var accepted int
			for {
				n, err := flow.Write(payload)
				accepted += n
				if err != nil {
					if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
						t.Fatal("stalled upload lost its deadline", err)
					}
					break
				}
			}
			if accepted == 0 {
				t.Fatal("fixture accepted no upload before backpressure")
			}
			if updateDuringClose {
				if err := flow.SetWriteDeadline(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			conn := flow.(*operatorLiteConn)
			closed := make(chan error, 1)
			go func() { closed <- conn.CloseWrite() }()
			if updateDuringClose {
				until := time.Now().Add(time.Second)
				for !conn.writeClosed.Load() && time.Now().Before(until) {
					time.Sleep(time.Millisecond)
				}
				if !conn.writeClosed.Load() {
					t.Fatal("half-close did not start draining")
				}
				if err := flow.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal("update pending half-close deadline", err)
				}
			}
			select {
			case err := <-closed:
				if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
					t.Fatal("half-close lost its deadline", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("half-close outlived its deadline")
			}
			select {
			case <-conn.writeDone:
			case <-time.After(time.Second):
				t.Fatal("expired flow retained its sender")
			}
			for until := time.Now().Add(time.Second); activity.activeRPCs.Load() != 0 && time.Now().Before(until); {
				time.Sleep(time.Millisecond)
			}
			if activity.activeRPCs.Load() != 0 {
				t.Fatal("expired flow retained its broker lease")
			}
			other, err := openOperatorTCP(ctx, cc, MeshSelector(target), 43022, nil)
			if err != nil {
				t.Fatal("flow expiry closed the shared broker connection", err)
			}
			_ = other.Close()
		})
	}
}
