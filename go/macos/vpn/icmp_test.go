package vpn

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func echoPacket(t *testing.T, destination string, payload []byte) []byte {
	t.Helper()
	body, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 0x1234, Seq: 7, Data: payload}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	// x/net/ipv4.Header.Marshal uses host byte order for Darwin raw sockets;
	// utun carries network-order IP packets on every platform.
	ip := make([]byte, 20)
	ip[0], ip[8], ip[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(body)))
	copy(ip[12:16], net.ParseIP(TunnelAddress).To4())
	copy(ip[16:20], net.ParseIP(destination).To4())
	binary.BigEndian.PutUint16(ip[10:12], ^checksum.Checksum(ip, 0))
	return append(ip, body...)
}

func newEchoEngine(t *testing.T, probe ProbeFunc) (*Engine, *Relay, Config) {
	t.Helper()
	relay, cfg, err := NewRelay(testRoutes(t), func(context.Context, string, uint16) (net.Conn, error) {
		t.Error("ICMP reached TCP dialer")
		return nil, io.EOF
	}, probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	return engine, relay, cfg
}

func readEcho(t *testing.T, engine *Engine) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	packet := engine.Read(ctx)
	if packet == nil {
		t.Fatal("missing echo reply")
	}
	return packet
}

func expectNoEcho(t *testing.T, engine *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if reply := engine.Read(ctx); reply != nil {
		t.Fatalf("unexpected reply: %x", reply)
	}
}

func TestICMPEchoWaitsForRemoteResponse(t *testing.T) {
	called := make(chan string, 1)
	respond := make(chan struct{})
	engine, _, cfg := newEchoEngine(t, func(ctx context.Context, selector string) error {
		called <- selector
		select {
		case <-respond:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	payload := []byte("ping timestamp and odd-length payload!")
	request := echoPacket(t, cfg.Routes[0].Address, payload)
	engine.Write(request)
	select {
	case selector := <-called:
		if selector != cfg.Routes[0].Selector {
			t.Fatalf("probe targeted %s", selector)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not reach relay")
	}
	// utun may reuse its input buffer as soon as Write returns.
	clear(request)
	expectNoEcho(t, engine)
	close(respond)
	reply := readEcho(t, engine)
	if len(reply) < 28 || reply[0] != 0x45 || net.IP(reply[12:16]).String() != cfg.Routes[0].Address ||
		net.IP(reply[16:20]).String() != TunnelAddress || reply[9] != 1 || int(binary.BigEndian.Uint16(reply[2:4])) != len(reply) ||
		checksum.Checksum(reply[:20], 0) != 0xffff {
		t.Fatalf("invalid reply header: %x", reply)
	}
	body := reply[20:]
	message, err := icmp.ParseMessage(1, body)
	if err != nil {
		t.Fatal(err)
	}
	echo, ok := message.Body.(*icmp.Echo)
	if !ok || message.Type != ipv4.ICMPTypeEchoReply || message.Code != 0 || checksum.Checksum(body, 0) != 0xffff ||
		echo.ID != 0x1234 || echo.Seq != 7 || !bytes.Equal(echo.Data, payload) {
		t.Fatalf("invalid echo: %+v, body %+v", message, echo)
	}
}

func TestICMPDoesNotReplyForOfflineDevice(t *testing.T) {
	called := make(chan struct{}, 1)
	engine, _, cfg := newEchoEngine(t, func(context.Context, string) error {
		called <- struct{}{}
		return errors.New("device offline or permission denied")
	})
	engine.Write(echoPacket(t, cfg.Routes[1].Address, nil))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("device was never probed")
	}
	expectNoEcho(t, engine)
}

func TestICMPValidation(t *testing.T) {
	routes := testRoutes(t)
	valid := echoPacket(t, routes[0].Address, []byte("ping"))
	fixIP := func(p []byte) []byte {
		p[10], p[11] = 0, 0
		binary.BigEndian.PutUint16(p[10:12], ^checksum.Checksum(p[:20], 0))
		return p
	}
	fixICMP := func(p []byte) []byte {
		p[22], p[23] = 0, 0
		binary.BigEndian.PutUint16(p[22:24], ^checksum.Checksum(p[20:], 0))
		return p
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"short IP":       func(p []byte) []byte { return p[:10] },
		"invalid IHL":    func(p []byte) []byte { p[0] = 0x4f; return p },
		"truncated":      func(p []byte) []byte { return p[:len(p)-1] },
		"short ICMP":     func(p []byte) []byte { p = p[:24]; binary.BigEndian.PutUint16(p[2:4], 24); return fixIP(p) },
		"IP checksum":    func(p []byte) []byte { p[10] ^= 1; return p },
		"ICMP checksum":  func(p []byte) []byte { p[22] ^= 1; return p },
		"wrong source":   func(p []byte) []byte { p[15] ^= 1; return fixIP(p) },
		"unselected":     func(p []byte) []byte { p[16] = 127; return fixIP(p) },
		"DNS resolver":   func(p []byte) []byte { copy(p[16:20], net.ParseIP(DNSAddress).To4()); return fixIP(p) },
		"expired TTL":    func(p []byte) []byte { p[8] = 0; return fixIP(p) },
		"first fragment": func(p []byte) []byte { p[6] = 0x20; return fixIP(p) },
		"later fragment": func(p []byte) []byte { p[7] = 1; return fixIP(p) },
		"reply":          func(p []byte) []byte { p[20] = 0; return fixICMP(p) },
		"error":          func(p []byte) []byte { p[20] = 3; return fixICMP(p) },
		"invalid code":   func(p []byte) []byte { p[21] = 1; return fixICMP(p) },
		"oversized":      func([]byte) []byte { return echoPacket(t, routes[0].Address, make([]byte, 1400)) },
	} {
		t.Run(name, func(t *testing.T) {
			e := &Engine{ctx: context.Background(), cfg: Config{Routes: routes}, echoes: make(chan echoRequest, 1)}
			e.queueEcho(mutate(bytes.Clone(valid)))
			if len(e.echoes) != 0 {
				t.Fatal("invalid ICMP queued a device probe")
			}
		})
	}
	e := &Engine{ctx: context.Background(), cfg: Config{Routes: routes}, echoes: make(chan echoRequest, 1)}
	e.queueEcho(valid)
	e.queueEcho(valid) // A full queue drops packets without blocking utun.
	if len(e.echoes) != 1 {
		t.Fatal("valid ICMP was not queued")
	}
	// Ensure changing checksums in the tests did not accidentally mutate valid.
	if !header.IPv4(valid).IsChecksumValid() {
		t.Fatal("fixture checksum changed")
	}
}

func TestEchoRelayAuthorization(t *testing.T) {
	var probes atomic.Int32
	_, _, cfg := newEchoEngine(t, func(context.Context, string) error { probes.Add(1); return nil })
	for _, req := range []flowRequest{
		{Token: strings.Repeat("0", 64), Address: cfg.Routes[0].Address, Kind: "echo"},
		{Token: cfg.Token, Address: "127.0.0.1", Kind: "echo"},
		{Token: cfg.Token, Address: "ping", Kind: "echo"},
		{Token: cfg.Token, Address: cfg.Routes[0].Address, Port: 80, Kind: "echo"},
		{Token: cfg.Token, Address: cfg.Routes[0].Address, Kind: "unknown"},
	} {
		conn, err := net.DialTimeout("tcp", cfg.Endpoint, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		json.NewEncoder(conn).Encode(req)
		io.ReadAll(conn)
		conn.Close()
	}
	if probes.Load() != 0 {
		t.Fatal("unauthorized request reached probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ProbeRelay(ctx, cfg, cfg.Routes[0].Address); err != nil || probes.Load() != 1 {
		t.Fatalf("authorized probe failed: %v, probes=%d", err, probes.Load())
	}
}

func TestEchoWorkersBoundedAndCancelled(t *testing.T) {
	started, stopped := make(chan struct{}, 32), make(chan struct{}, 32)
	engine, relay, cfg := newEchoEngine(t, func(ctx context.Context, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return ctx.Err()
	})
	packet := echoPacket(t, cfg.Routes[0].Address, nil)
	for range 1000 {
		engine.Write(packet)
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("more than 8 simultaneous device probes")
	default:
	}
	done := make(chan struct{})
	go func() { engine.Close(); relay.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel probes")
	}
	if len(stopped) != 8 {
		t.Fatalf("only %d probes cancelled", len(stopped))
	}
	engine.Write(packet) // Writes after shutdown must not start more probes.
}
