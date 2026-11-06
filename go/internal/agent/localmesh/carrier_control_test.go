package localmesh

import (
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestRouterControlNegotiatesDirectionalReceiveLimits(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		limit int
		err   error
	}
	serverResult := make(chan result, 1)
	serverClose := make(chan struct{})
	defer close(serverClose)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			serverResult <- result{err: err}
			return
		}
		defer conn.CloseWithError(0, "done")
		_, limit, err := openRouterControl(ctx, conn, b.Device, a.Device, DatagramLimit)
		serverResult <- result{limit, err}
		select {
		case <-serverClose:
		case <-ctx.Done():
		}
	}()
	clientTLS, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	_, limit, err := openRouterControl(ctx, conn, a.Device, b.Device, WholeIPDatagramLimit)
	if err != nil {
		t.Fatal(err)
	}
	if limit != DatagramLimit {
		t.Fatalf("server receive constraint was not authenticated: %d", limit)
	}
	select {
	case result := <-serverResult:
		if result.err != nil || result.limit != WholeIPDatagramLimit {
			t.Fatalf("client receive capability was not independently negotiated: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRouterRejectsInvalidLocalCapabilityBeforeConnectionAccess(t *testing.T) {
	a, b := testDevices(t)
	for _, limit := range []int{0, -1, 1099, 1101, 1328, 1330} {
		if _, _, err := openRouterControl(context.Background(), nil, a, b, limit); err == nil {
			t.Fatalf("invalid local capability accepted: %d", limit)
		}
	}
}
