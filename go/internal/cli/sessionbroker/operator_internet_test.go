//go:build !windows

package sessionbroker

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestOperatorInternetStreamOwnsOneLease(t *testing.T) {
	r, cc, activity, _ := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	peers := make(chan net.Conn, 2)
	r.internetOpen = func(ctx context.Context, peer meshid.Device) (net.Conn, error) {
		if peer != target {
			t.Error("wrong gateway", peer)
		}
		local, remote := net.Pipe()
		peers <- remote
		return local, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := openOperatorStream(ctx, cc, MeshSelector(target), operatorInternetMethod, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	remote := <-peers
	defer remote.Close()
	other, err := openOperatorStream(ctx, cc, MeshSelector(target), operatorInternetMethod, nil, nil)
	if err == nil {
		other.Close()
		t.Fatal("second lease admitted")
	}
	done := make(chan error, 1)
	go func() { _, err := remote.Write([]byte("packet frame")); done <- err }()
	data := make([]byte, len("packet frame"))
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "packet frame" {
		t.Fatal("stream lost bytes", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	conn.Close()
	remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream retained remote lease")
	}
	until := time.Now().Add(time.Second)
	for activity.activeRPCs.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if activity.activeRPCs.Load() != 0 {
		t.Fatal("stream retained broker activity")
	}
}
