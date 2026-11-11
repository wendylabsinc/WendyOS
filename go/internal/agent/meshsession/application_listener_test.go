package meshsession

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

type applicationRelayAuthorization struct {
	testUDPAuthorization
	tcpPort uint16
	tcpAddr string
}

func (a applicationRelayAuthorization) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != a.tcpPort {
		return nil, ErrDenied
	}
	return dial(a.tcpAddr)
}

func TestApplicationRelayPinsIdentityAndAdmitsTCPAndUDP(t *testing.T) {
	a, b := fixtureCredentials(t)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	tcpPort := uint16(tcp.Addr().(*net.TCPAddr).Port)
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				payload, err := io.ReadAll(conn)
				if err == nil {
					_, _ = conn.Write(payload)
				}
			}()
		}
	}()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	udpPort := uint16(udp.LocalAddr().(*net.UDPAddr).Port)
	go func() {
		buffer := make([]byte, MaxUDPMessageSize)
		for {
			n, peer, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP(buffer[:n], peer)
		}
	}()
	registry := meshingress.NewRegistry()
	if err := registry.ClaimUDPForApp("relay-udp", "com.wendy.relay", udpPort, "10.3.0.2", udpPort); err != nil {
		t.Fatal(err)
	}
	authorizer := applicationRelayAuthorization{testUDPAuthorization: testUDPAuthorization{registry}, tcpPort: tcpPort, tcpAddr: tcp.Addr().String()}
	server, err := NewServer(b, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	listener := NewApplicationListener()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, listener) }()
	defer func() { cancel(); _ = server.Close(); <-done }()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.Dial(ctx) }
	address := netip.AddrPortFrom(b.Device.MeshAddress(), TLSPort)
	setup, stop := context.WithTimeout(ctx, 3*time.Second)
	flow, err := DialTLSWithDialer(setup, a, b.Device, address, tcpPort, dial)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	_ = flow.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(flow, "app-only relay"); err != nil {
		t.Fatal(err)
	}
	if err := flow.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(flow); err != nil || string(got) != "app-only relay" {
		t.Fatalf("relay TCP echo=%q err=%v", got, err)
	}
	if _, err := DialTLSWithDialer(ctx, a, a.Device, address, tcpPort, dial); err == nil {
		t.Fatal("relay accepted another target principal")
	}
	if denied, err := DialTLSWithDialer(ctx, a, b.Device, address, 22, dial); !errors.Is(err, ErrDenied) || denied != nil {
		t.Fatalf("relay admitted administrative port: %v", err)
	}
	setup, stop = context.WithTimeout(ctx, 3*time.Second)
	datagrams, err := DialUDPTLSWithDialer(setup, a, b.Device, address, udpPort, dial)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	defer datagrams.Close()
	for _, payload := range [][]byte{nil, []byte("one"), bytes.Repeat([]byte{7}, localEchoMaximum())} {
		if err := datagrams.Send(payload); err != nil {
			t.Fatal(err)
		}
		message, err := datagrams.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		match := bytes.Equal(message.Payload, payload)
		message.Release()
		if !match {
			t.Fatal("relay changed UDP message boundaries")
		}
	}
	registry.Release("relay-udp")
	receive, stopReceive := context.WithTimeout(ctx, time.Second)
	defer stopReceive()
	if message, err := datagrams.Receive(receive); err == nil {
		message.Release()
		t.Fatal("relay delivered a revoked UDP application message")
	}
	select {
	case <-datagrams.Done():
	case <-time.After(time.Second):
		t.Fatal("relay kept a revoked UDP application flow")
	}
}

func TestApplicationListenerCancellationAndClose(t *testing.T) {
	listener := NewApplicationListener()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := listener.Dial(ctx); !errors.Is(err, context.Canceled) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("canceled admission=%v", err)
	}
	listener.Close()
	if _, err := listener.Dial(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed admission=%v", err)
	}
}
