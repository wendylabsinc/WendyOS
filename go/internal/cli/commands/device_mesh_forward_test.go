package commands

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestNormalMeshForwardDispatchesRoutedLiteService(t *testing.T) {
	old := dialLiteMeshApp
	defer func() { dialLiteMeshApp = old }()
	selector := "mesh://11111111-1111-4111-8111-111111111111/33333333-3333-4333-8333-333333333333"
	target := &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"selector": selector}}}
	var called bool
	dialLiteMeshApp = func(_ context.Context, target string, port uint16) (net.Conn, error) {
		if target != selector || port != 28000 {
			t.Fatal("wrong Lite service", target, port)
		}
		called = true
		local, remote := net.Pipe()
		remote.Close()
		return local, nil
	}
	dial, label, err := meshAppForwardDialer(target, 28000)
	if err != nil || label != selector {
		t.Fatal(label, err)
	}
	flow, err := dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	flow.Close()
	if !called {
		t.Fatal("Lite forwarding did not use its declared native app path")
	}
}

func TestNormalMeshForwardTCPHalfCloseAndListenerCancellation(t *testing.T) {
	service, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	payload := bytes.Repeat([]byte("normal mesh upload\n"), 4096)
	response := append([]byte("reply\n"), payload...)
	serviceDone := make(chan error, 1)
	go func() {
		conn, err := service.Accept()
		if err != nil {
			serviceDone <- err
			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err == nil && !bytes.Equal(got, payload) {
			t.Error("forward upload differs")
		}
		if err == nil {
			_, err = conn.Write(response)
		}
		serviceDone <- err
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- serveMeshForward(owner, listener, func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", service.Addr().String())
		})
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, response) {
		t.Fatal("forward reply", len(got), err)
	}
	if err := <-serviceDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("forward listener did not stop")
	}
}

func TestNormalMeshForwardBoundsPendingFlowsAndJoinsOnCancel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opened, closed atomic.Int32
	finished := make(chan error, 1)
	go func() {
		finished <- serveMeshForward(owner, listener, func(ctx context.Context) (net.Conn, error) {
			opened.Add(1)
			<-ctx.Done()
			closed.Add(1)
			return nil, ctx.Err()
		})
	}()
	var clients []net.Conn
	defer func() {
		for _, conn := range clients {
			conn.Close()
		}
	}()
	for range meshForwardFlows + 1 {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
	}
	until := time.Now().Add(time.Second)
	for opened.Load() != meshForwardFlows && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if opened.Load() != meshForwardFlows {
		t.Fatal("flow admission", opened.Load())
	}
	_ = clients[len(clients)-1].SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clients[len(clients)-1].Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("full listener did not close excess connection", err)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending forwarding flows were not joined")
	}
	if closed.Load() != meshForwardFlows {
		t.Fatal("pending flow ownership", closed.Load())
	}
}
