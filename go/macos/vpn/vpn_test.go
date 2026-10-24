package vpn

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

func testRoutes(t *testing.T) []Route {
	t.Helper()
	r, err := BuildRoutes([]Route{{Selector: "mesh:12345678-abcd-4321-abcd-123456789012", Name: "Lab"}, {Selector: "cloud://api.wendy.dev:443/org/7/asset/42", Name: "Remote"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestRouteValidation(t *testing.T) {
	for _, selector := range []string{"localhost:22", "cloud://42", "mesh:../bad", "mesh:", "cloud://api:443/org/1/asset/42?override=yes"} {
		if _, err := BuildRoutes([]Route{{Selector: selector}}); err == nil {
			t.Errorf("accepted %s", selector)
		}
	}
	routes := testRoutes(t)
	again, err := BuildRoutes(routes)
	if err != nil || again[0] != routes[0] {
		t.Fatal("routes are not stable", err)
	}
	cfg := Config{Endpoint: "127.0.0.1:8080", Token: strings.Repeat("a", 64), Routes: routes}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Routes[0].Address = "127.0.0.1"
	if cfg.Validate() == nil {
		t.Fatal("accepted non-mesh destination")
	}
	cfg.Routes = routes
	cfg.Endpoint = "192.0.2.1:8080"
	if cfg.Validate() == nil {
		t.Fatal("accepted remote relay")
	}
}
func TestDNS(t *testing.T) {
	routes := testRoutes(t)
	for _, tt := range []struct {
		name          string
		kind          uint16
		code, answers int
	}{
		{strings.ToUpper(routes[0].Host), dns.TypeA, 0, 1},
		{routes[1].Host, dns.TypeAAAA, 0, 0},
		{"device-missing.mesh.wendy.dev", dns.TypeA, dns.RcodeNameError, 0},
		{"example.org", dns.TypeA, dns.RcodeRefused, 0},
		{"cloud.wendy.dev", dns.TypeA, dns.RcodeRefused, 0},
		{"dashboard.cloud.wendy.dev", dns.TypeA, dns.RcodeRefused, 0},
		{"evilcloud.wendy.dev", dns.TypeA, dns.RcodeRefused, 0},
	} {
		t.Run(tt.name+dns.TypeToString[tt.kind], func(t *testing.T) {
			q := new(dns.Msg)
			q.SetQuestion(dns.Fqdn(tt.name), tt.kind)
			wire, _ := q.Pack()
			reply, err := AnswerDNS(wire, routes)
			if err != nil {
				t.Fatal(err)
			}
			var r dns.Msg
			if err = r.Unpack(reply); err != nil {
				t.Fatal(err)
			}
			if r.Id != q.Id || r.Rcode != tt.code || len(r.Answer) != tt.answers {
				t.Fatal(r.String())
			}
		})
	}
	if _, err := AnswerDNS([]byte{1, 2}, routes); err == nil {
		t.Fatal("accepted truncated DNS")
	}
}
func TestRelayAuthenticationAndRevocation(t *testing.T) {
	called := make(chan struct{}, 1)
	relay, cfg, err := NewRelay(testRoutes(t), func(context.Context, string, uint16) (net.Conn, error) { called <- struct{}{}; return nil, io.EOF })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !Healthy(ctx, cfg) {
		t.Fatal("authenticated heartbeat failed")
	}
	bad := cfg
	bad.Token = strings.Repeat("0", 64)
	if Healthy(ctx, bad) {
		t.Fatal("wrong credential accepted")
	}
	for _, request := range []flowRequest{{bad.Token, cfg.Routes[0].Address, 80}, {cfg.Token, "127.0.0.1", 22}, {cfg.Token, cfg.Routes[0].Address, 0}} {
		conn, err := net.Dial("tcp", cfg.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		json.NewEncoder(conn).Encode(request)
		io.ReadAll(conn)
		conn.Close()
	}
	select {
	case <-called:
		t.Fatal("unauthorized flow reached dialer")
	default:
	}
	relay.Close()
	if Healthy(ctx, cfg) {
		t.Fatal("closed relay accepted heartbeat")
	}
}
func TestPacketTunnelTCPAndDNS(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		body, _ := io.ReadAll(c)
		c.Write(append([]byte("reply:"), body...))
	}()
	routes := testRoutes(t)
	relay, cfg, err := NewRelay(routes, func(ctx context.Context, selector string, port uint16) (net.Conn, error) {
		if selector != routes[0].Selector || port != 8080 {
			t.Errorf("wrong target %s:%d", selector, port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	peer := newPeer(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := func(ip string, port uint16) tcpip.FullAddress {
		return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(net.ParseIP(ip).To4()), Port: port}
	}
	conn, err := gonet.DialContextTCP(ctx, peer, address(routes[0].Address, 8080), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	conn.Write([]byte("hello"))
	conn.CloseWrite()
	reply, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(reply) != "reply:hello" {
		t.Fatalf("TCP half-close reply %q: %v", reply, err)
	}
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(routes[1].Host), dns.TypeA)
	wire, _ := query.Pack()
	dest := address(DNSAddress, 53)
	udpConn, err := gonet.DialUDP(peer, nil, &dest, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	udpConn.SetDeadline(time.Now().Add(time.Second))
	udpConn.Write(wire)
	buf := make([]byte, 4096)
	n, err := udpConn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var udpReply dns.Msg
	if err = udpReply.Unpack(buf[:n]); err != nil || len(udpReply.Answer) != 1 {
		t.Fatal("UDP DNS", err, udpReply.String())
	}
	tcpConn, err := gonet.DialContextTCP(ctx, peer, dest, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpConn.Close()
	tcpConn.SetDeadline(time.Now().Add(time.Second))
	framed := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(framed, uint16(len(wire)))
	copy(framed[2:], wire)
	// Deliberately fragment the DNS-over-TCP prefix.
	tcpConn.Write(framed[:1])
	tcpConn.Write(framed[1:])
	var size [2]byte
	if _, err = io.ReadFull(tcpConn, size[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err = io.ReadFull(tcpConn, body); err != nil {
		t.Fatal(err)
	}
	var tcpReply dns.Msg
	if err = tcpReply.Unpack(body); err != nil || len(tcpReply.Answer) != 1 {
		t.Fatal("TCP DNS", err, tcpReply.String())
	}
}
func newPeer(t *testing.T, engine *Engine) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	link := channel.New(256, 1400, "")
	if err := s.CreateNIC(1, link); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(net.ParseIP(TunnelAddress).To4()), PrefixLen: 32}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done1)
		for {
			p := link.ReadContext(ctx)
			if p == nil {
				return
			}
			view := p.ToView()
			engine.Write(append([]byte(nil), view.AsSlice()...))
			view.Release()
			p.DecRef()
		}
	}()
	go func() {
		defer close(done2)
		for {
			data := engine.Read(ctx)
			if data == nil {
				return
			}
			p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
			link.InjectInbound(ipv4.ProtocolNumber, p)
			p.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		s.Close()
		link.Close()
		<-done1
		<-done2
		s.Wait()
	})
	return s
}
