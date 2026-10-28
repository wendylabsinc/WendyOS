// Package netmesh connects the CLI to Wendy Net using a private IPv6 stack.
package netmesh

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type Network struct {
	self       netip.Addr
	stack      *stack.Stack
	endpoint   *channel.Endpoint
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.RWMutex
	ports      map[babel.LinkID]*packetPort
	routes     map[netip.Addr]babel.LinkID
	once       sync.Once
	operations sync.WaitGroup
}

func NewNetwork(self meshid.Device) (*Network, error) {
	if !self.IsValid() || !self.MeshAddress().IsValid() {
		return nil, errors.New("missing mesh identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Network{self: self.MeshAddress(), ctx: ctx, cancel: cancel, done: make(chan struct{}), ports: map[babel.LinkID]*packetPort{}, routes: map[netip.Addr]babel.LinkID{}}
	n.stack = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6}})
	n.endpoint = channel.New(128, 1280, "")
	if err := n.stack.CreateNIC(1, n.endpoint); err != nil {
		cancel()
		n.stack.Close()
		return nil, errors.New(err.String())
	}
	addr := tcpip.AddrFrom16(n.self.As16())
	if err := n.stack.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 128}}, stack.AddressProperties{}); err != nil {
		cancel()
		n.stack.Close()
		return nil, errors.New(err.String())
	}
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFrom16(meshid.MeshPrefix.Addr().As16()), tcpip.MaskFromBytes(net.CIDRMask(48, 128)))
	operatorSubnet, _ := tcpip.NewSubnet(tcpip.AddrFrom16(meshoperator.Prefix.Addr().As16()), tcpip.MaskFromBytes(net.CIDRMask(48, 128)))
	n.stack.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: 1}, {Destination: operatorSubnet, NIC: 1}})
	go func() {
		defer close(n.done)
		for {
			packet := n.endpoint.ReadContext(ctx)
			if packet == nil {
				return
			}
			view := packet.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			packet.DecRef()
			n.forward(data, false)
		}
	}()
	return n, nil
}
func (n *Network) ReconcileBase() error {
	if n.ctx.Err() != nil {
		return net.ErrClosed
	}
	return nil
}

// beginOperation excludes new stack users once shutdown starts. Register under
// the same lock as cancellation so Close can join without racing WaitGroup.Add.
func (n *Network) beginOperation() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.ctx.Err() != nil {
		return false
	}
	n.operations.Add(1)
	return true
}
func (n *Network) AddPeer(id babel.LinkID, peer meshid.Device) (io.ReadWriteCloser, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if peer.Tenant().IsValid() == false {
		return nil, errors.New("invalid peer")
	}
	if _, ok := n.ports[id]; ok {
		return nil, errors.New("duplicate mesh link")
	}
	p := &packetPort{network: n, queue: make(chan []byte, 32), done: make(chan struct{})}
	n.ports[id] = p
	return p, nil
}
func (n *Network) RemovePeer(id babel.LinkID) error {
	n.mu.Lock()
	p := n.ports[id]
	delete(n.ports, id)
	n.mu.Unlock()
	if p != nil {
		return p.Close()
	}
	return nil
}
func (n *Network) Apply(routes []babel.Route) error {
	next := map[netip.Addr]babel.LinkID{}
	for _, r := range routes {
		if !r.Local && !r.Unreachable && r.Prefix.Bits() == 128 && meshAddress(r.Prefix.Addr()) {
			next[r.Prefix.Addr()] = r.Link
		}
	}
	n.mu.Lock()
	if n.ctx.Err() != nil {
		n.mu.Unlock()
		return net.ErrClosed
	}
	n.routes = next
	n.mu.Unlock()
	return nil
}
func (n *Network) Stop() error { return n.Apply(nil) }
func (n *Network) Close() error {
	n.once.Do(func() {
		n.mu.Lock()
		n.cancel()
		for _, p := range n.ports {
			_ = p.Close()
		}
		n.ports = nil
		n.routes = nil
		n.mu.Unlock()
		// gVisor does not close endpoints created during Stack.Close. Cancel and
		// join every dial and inbound injection before destroying the stack. The
		// outgoing pump also owns stack packets and must finish before teardown.
		n.operations.Wait()
		<-n.done
		n.stack.Close()
		n.endpoint.Close()
		n.stack.Wait()
	})
	return nil
}
func (n *Network) forward(packet []byte, received bool) {
	if len(packet) < 40 || packet[0]>>4 != 6 || int(binary.BigEndian.Uint16(packet[4:6]))+40 != len(packet) || packet[7] == 0 {
		return
	}
	source := netip.AddrFrom16([16]byte(packet[8:24]))
	dest := netip.AddrFrom16([16]byte(packet[24:40]))
	if !meshAddress(source) || !meshAddress(dest) {
		return
	}
	if received && dest == n.self {
		p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		n.endpoint.InjectInbound(header.IPv6ProtocolNumber, p)
		p.DecRef()
		return
	}
	if received {
		if packet[7] <= 1 {
			return
		}
		packet = append([]byte(nil), packet...)
		packet[7]--
	}
	n.mu.RLock()
	id, ok := n.routes[dest]
	p := n.ports[id]
	n.mu.RUnlock()
	if !ok || p == nil {
		return
	}
	select {
	case <-p.done:
	case p.queue <- packet:
	default:
	}
}
func (n *Network) DialContext(ctx context.Context, address string) (net.Conn, error) {
	return n.Dial(ctx, "tcp", address)
}

func (n *Network) Address() netip.Addr { return n.self }

func (n *Network) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if n.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	target, err := netip.ParseAddrPort(address)
	if err != nil || target.Port() == 0 || !meshAddress(target.Addr()) {
		return nil, errors.New("mesh dial requires an IPv6 mesh address and port")
	}
	n.mu.RLock()
	id, ok := n.routes[target.Addr()]
	p := n.ports[id]
	n.mu.RUnlock()
	if !ok || p == nil {
		return nil, errors.New("mesh destination has no authenticated route")
	}
	if !n.beginOperation() {
		return nil, net.ErrClosed
	}
	defer n.operations.Done()
	dialCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	defer cancel()
	remote := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(target.Addr().As16()), Port: target.Port()}
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(n.self.As16())}
	var connection net.Conn
	switch network {
	case "tcp", "tcp6":
		conn, dialErr := gonet.DialTCPWithBind(dialCtx, n.stack, local, remote, ipv6.ProtocolNumber)
		err = dialErr
		if conn != nil {
			connection = conn
		}
	case "udp", "udp6":
		if err = dialCtx.Err(); err == nil {
			conn, dialErr := gonet.DialUDP(n.stack, &local, &remote, ipv6.ProtocolNumber)
			err = dialErr
			if conn != nil {
				connection = conn
			}
		}
	default:
		return nil, errors.New("mesh transport requires TCP or UDP")
	}

	if n.ctx.Err() != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, net.ErrClosed
	}
	if err != nil {
		return nil, err
	}
	return connection, nil
}

type packetPort struct {
	network *Network
	queue   chan []byte
	done    chan struct{}
	once    sync.Once
}

func (p *packetPort) Read(b []byte) (int, error) {
	select {
	case <-p.done:
		return 0, io.EOF
	case data := <-p.queue:
		if len(data) > len(b) {
			return 0, io.ErrShortBuffer
		}
		return copy(b, data), nil
	}
}
func (p *packetPort) Write(b []byte) (int, error) {
	select {
	case <-p.done:
		return 0, net.ErrClosed
	default:
	}
	if !p.network.beginOperation() {
		return 0, net.ErrClosed
	}
	defer p.network.operations.Done()
	p.network.forward(append([]byte(nil), b...), true)
	return len(b), nil
}
func (p *packetPort) Close() error { p.once.Do(func() { close(p.done) }); return nil }

func meshAddress(address netip.Addr) bool {
	return meshid.InMesh(address) || meshoperator.InOperatorPool(address)
}

// ListenTCP binds a packet-network listener while participating in shutdown.
func (n *Network) ListenTCP(port uint16) (net.Listener, error) {
	if port == 0 {
		return nil, errors.New("mesh listener requires a port")
	}
	if !n.beginOperation() {
		return nil, net.ErrClosed
	}
	defer n.operations.Done()
	listener, err := gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(n.self.As16()), Port: port}, ipv6.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	if n.ctx.Err() != nil {
		_ = listener.Close()
		return nil, net.ErrClosed
	}
	return listener, nil
}
