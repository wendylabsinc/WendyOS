package netmesh

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// InternetTunnel carries IP-in-IPv6 over the existing router. Netstack delivers
// replies after IPv6 extension processing and fragment reassembly. Raw socket
// reception alone is insufficient: it runs before reassembly in netstack.
type InternetTunnel struct {
	Config  meshsharing.InternetConfig
	network *Network
	gateway netip.Addr
	ctx     context.Context
	cancel  context.CancelFunc
	packets chan []byte
	once    sync.Once
}

func (n *Network) OpenInternetTunnel(gateway netip.Addr, config meshsharing.InternetConfig) (*InternetTunnel, error) {
	if !config.Valid() || !meshid.InMesh(gateway) {
		return nil, errors.New("invalid internet tunnel")
	}
	if !n.beginOperation() {
		return nil, net.ErrClosed
	}
	n.mu.Lock()
	if n.internet != nil || n.ctx.Err() != nil {
		n.mu.Unlock()
		n.operations.Done()
		return nil, errors.New("internet tunnel is closed or already in use")
	}
	ctx, cancel := context.WithCancel(n.ctx)
	t := &InternetTunnel{Config: config, network: n, gateway: gateway, ctx: ctx, cancel: cancel, packets: make(chan []byte, 128)}
	n.internet = t
	n.mu.Unlock()
	go func() { <-ctx.Done(); t.Close() }()
	return t, nil
}

func (n *Network) receiveInternet(id stack.TransportEndpointID, packet *stack.PacketBuffer) bool {
	n.mu.RLock()
	t := n.internet
	n.mu.RUnlock()
	if t == nil || id.RemoteAddress.String() != t.gateway.String() || id.LocalAddress.String() != n.self.String() {
		return true
	}
	payload := packet.Data().ToBuffer()
	defer payload.Release()
	data := append(append([]byte(nil), packet.TransportHeader().Slice()...), payload.Flatten()...)
	if !t.validPacket(data, false) {
		return true
	}
	select {
	case <-t.ctx.Done():
	case t.packets <- data:
	default:
	}
	return true
}
func (t *InternetTunnel) validPacket(p []byte, outgoing bool) bool {
	source, destination, ok := meshsharing.PacketAddresses(p)
	if !ok {
		return false
	}
	address := destination
	if outgoing {
		address = source
	}
	return address.String() == t.Config.Address4 || (address.Is6() && address.String() == t.Config.Address6)
}
func (t *InternetTunnel) WritePacket(packet []byte) error {
	if t.ctx.Err() != nil {
		return net.ErrClosed
	}
	if len(packet) > t.Config.MTU || !t.validPacket(packet, true) {
		return errors.New("invalid borrowed internet packet")
	}
	outer := make([]byte, 40+len(packet))
	outer[0] = 0x60
	outer[6] = 4
	outer[7] = 64
	if packet[0]>>4 == 6 {
		if t.Config.Address6 == "" {
			return errors.New("gateway has no IPv6 uplink")
		}
		outer[6] = 41
	}
	binary.BigEndian.PutUint16(outer[4:6], uint16(len(packet)))
	copy(outer[8:24], t.network.self.AsSlice())
	copy(outer[24:40], t.gateway.AsSlice())
	copy(outer[40:], packet)
	t.network.forward(outer, false)
	return nil
}
func (t *InternetTunnel) ReadPacket(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, net.ErrClosed
	case p := <-t.packets:
		return p, nil
	}
}
func (t *InternetTunnel) Close() error {
	t.once.Do(func() {
		t.cancel()
		t.network.mu.Lock()
		if t.network.internet == t {
			t.network.internet = nil
		}
		t.network.mu.Unlock()
		t.network.operations.Done()
	})
	return nil
}
