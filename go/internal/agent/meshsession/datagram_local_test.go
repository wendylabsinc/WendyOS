package meshsession

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

func TestLocalUDPAppEchoAndIncarnationRevocation(t *testing.T) {
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		buf := make([]byte, MaxUDPMessageSize+1)
		for {
			n, a, e := app.ReadFromUDP(buf)
			if e != nil {
				return
			}
			_, _ = app.WriteToUDP(buf[:n], a)
		}
	}()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	r := meshingress.NewRegistry()
	if _, err := DialLocalUDP(context.Background(), testUDPAuthorization{r}, port); err == nil {
		t.Fatal("host UDP listener admitted")
	}
	if err := r.ClaimForApp("tcp", "com.wendy.tcp", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	if _, err := DialLocalUDP(context.Background(), testUDPAuthorization{r}, port); err == nil {
		t.Fatal("TCP claim admitted UDP")
	}
	if err := r.ClaimUDPForApp("echo", "com.wendy.echo", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	flow, err := DialLocalUDP(context.Background(), testUDPAuthorization{r}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, payload := range [][]byte{nil, []byte("self VIP"), bytes.Repeat([]byte{42}, localEchoMaximum())} {
		if err := flow.Send(payload); err != nil {
			t.Fatal(err)
		}
		got, err := readUDPMessage(flow, ctx)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("echo %v %v", got, err)
		}
	}
	for _, payload := range [][]byte{bytes.Repeat([]byte{42}, MaxUDPMessageSize+1)} {
		if flow.Send(payload) == nil {
			t.Fatal("invalid payload accepted")
		}
	}
	if err := flow.Send([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	r.Release("echo")
	if err := r.ClaimUDPForApp("replacement", "com.wendy.replacement", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	if err := flow.Send([]byte("old flow")); !errors.Is(err, meshingress.ErrPortDenied) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("revoked send: %v", err)
	}
	if _, err := readUDPMessage(flow, ctx); !errors.Is(err, meshingress.ErrPortDenied) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("revoked receive: %v", err)
	}
	fresh, err := DialLocalUDP(ctx, testUDPAuthorization{r}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Send([]byte("new flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := readUDPMessage(fresh, ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLocalUDPReceiveCancellationAndClose(t *testing.T) {
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	r := meshingress.NewRegistry()
	if err := r.ClaimUDPForApp("silent", "com.wendy.silent", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	flow, err := DialLocalUDP(context.Background(), testUDPAuthorization{r}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialLocalUDP(ctx, testUDPAuthorization{r}, port); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := readUDPMessage(flow, ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := readUDPMessage(flow, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readUDPMessage(flow, context.Background()); done <- err }()
	_ = flow.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close failed to unblock receive")
	}
}

type combinedLocalFixture struct {
	testUDPAuthorization
	combinedCalls *atomic.Int32
}

func (f combinedLocalFixture) WithAuthorizedLocalUDP(ip, dst netip.Addr, index int, source uint64, port uint16, token uint64, send func(netip.AddrPort) error) error {
	f.combinedCalls.Add(1)
	return f.Registry.WithAuthorizedLocalUDP(ip, dst, index, source, port, token, func(address netip.AddrPort) error {
		return send(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), address.Port()))
	})
}

func TestSelfUDPReplacesSourceGuardWithSingleCombinedGuard(t *testing.T) {
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	r := meshingress.NewRegistry()
	ip, vip := netip.MustParseAddr("10.79.42.2"), netip.MustParseAddr("10.99.0.1")
	if err := r.ClaimSource("source", "app", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	if err := r.ClaimUDPForApp("dest", "app", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	_, creds := fixtureCredentials(t)
	manager := udpmessage.NewManager()
	defer manager.Close()
	var sourceCalls, combinedCalls atomic.Int32
	endpoint, err := manager.Open(creds.Device, func(send func() error) error {
		sourceCalls.Add(1)
		return r.WithSourceToken(ip, vip, 7, r.SourceToken(ip, vip, 7), send)
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := UDPSourceClaim{IP: ip, VIP: vip, IfIndex: 7, Token: r.SourceToken(ip, vip, 7)}
	flow, err := DialLocalUDP(context.Background(), combinedLocalFixture{testUDPAuthorization{r}, &combinedCalls}, port, UDPOptions{Manager: manager, Endpoint: endpoint, Source: &claim, Done: r.SourceDone(ip, vip, 7, claim.Token)})
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	if err := flow.Send([]byte("one lock")); err != nil {
		t.Fatal(err)
	}
	_ = app.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := app.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "one lock" {
		t.Fatalf("delivery %q: %v", buf[:n], err)
	}
	if sourceCalls.Load() != 0 || combinedCalls.Load() < 2 {
		t.Fatalf("nested/source-only guard: source=%d combined=%d", sourceCalls.Load(), combinedCalls.Load())
	}
	r.Release("dest")
	select {
	case <-endpoint.Done():
	case <-time.After(time.Second):
		t.Fatal("idle destination lease did not retire flow")
	}
}

func TestUDPReceiveSerializationHonorsCancellationAndRetirement(t *testing.T) {
	for _, kind := range []string{"self", "QUIC"} {
		t.Run(kind, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			r := meshingress.NewRegistry()
			port := udpLifecycleApp(t, r, "receive")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var receive func(context.Context) (*udpmessage.Message, error)
			var closeFlow func() error
			var lock *udpReceiveLock
			var done <-chan struct{}
			if kind == "self" {
				f, err := DialLocalUDP(ctx, testUDPAuthorization{r}, port)
				if err != nil {
					t.Fatal(err)
				}
				receive, closeFlow, lock, done = f.Receive, f.Close, &f.readMu, f.endpoint.Done()
			} else {
				address := lifecycleServer(t, b, testUDPAuthorization{r}, true)
				f, err := DialUDP(ctx, a, b.Device, address, port)
				if err != nil {
					t.Fatal(err)
				}
				receive, closeFlow, lock, done = f.Receive, f.Close, &f.readMu, f.endpoint.Done()
			}
			defer closeFlow()
			if err := lock.lock(ctx, done); err != nil {
				t.Fatal(err)
			}
			defer lock.unlock()
			short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
			defer stop()
			canceled := make(chan error, 1)
			go func() {
				message, err := receive(short)
				if message != nil {
					message.Release()
				}
				canceled <- err
			}()
			select {
			case err := <-canceled:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("serialized receive ignored cancellation")
			}
			retired := make(chan error, 1)
			go func() {
				message, err := receive(context.Background())
				if message != nil {
					message.Release()
				}
				retired <- err
			}()
			closeFlow()
			select {
			case err := <-retired:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("serialized receive ignored close")
			}
		})
	}
}
