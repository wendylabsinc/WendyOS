//go:build linux

package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"golang.org/x/sys/unix"
)

const UDPProxyPort = 50059
const udpFlowLimit = 256
const udpQueueLimit = 32
const udpFlowIdle = 30 * time.Second
const udpDialTimeout = 40 * time.Second

type UDPFlow interface {
	Send([]byte) error
	Receive(context.Context) ([]byte, error)
	Close() error
}

type UDPDialer interface {
	DialUDP(context.Context, int32, uint16) (UDPFlow, error)
}

type UDPDialFunc func(context.Context, int32, uint16) (UDPFlow, error)

func (f UDPDialFunc) DialUDP(ctx context.Context, peer int32, port uint16) (UDPFlow, error) {
	return f(ctx, peer, port)
}

type UDPSourcePolicy interface {
	SourceToken(netip.Addr, netip.Addr, int) uint64
	WithSourceToken(netip.Addr, netip.Addr, int, uint64, func() error) error
}

type udpKey struct {
	source, destination netip.AddrPort
	ifindex             int
	token               uint64
}
type udpProxyFlow struct {
	queue  chan []byte
	cancel context.CancelFunc
}

// UDPProxy accepts only source addresses owned by running mesh-mode apps and
// original destinations inside their service CIDR. One QUIC connection per
// source/destination tuple preserves independent reply mappings.
type UDPProxy struct {
	dialer    UDPDialer
	sources   UDPSourcePolicy
	mu        sync.Mutex
	flows     map[udpKey]*udpProxyFlow
	conn      *net.UDPConn
	closed    bool
	replyDial func(netip.AddrPort, netip.AddrPort) (*net.UDPConn, error)
}

func NewUDPProxy(dialer UDPDialer, sources UDPSourcePolicy) (*UDPProxy, error) {
	if dialer == nil || sources == nil {
		return nil, errors.New("missing UDP mesh dialer or app source registry")
	}
	return &UDPProxy{dialer: dialer, sources: sources, flows: make(map[udpKey]*udpProxyFlow), replyDial: dialUDPReply}, nil
}

func (p *UDPProxy) Start(addr string) error {
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		err := raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVORIGDSTADDR, 1) })
		if err != nil {
			return err
		}
		if sockErr == nil {
			err = raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1) })
			if err != nil {
				return err
			}
		}
		if sockErr == nil {
			err = raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1) })
			if err != nil {
				return err
			}
		}
		return sockErr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = pc.Close()
		return net.ErrClosed
	}
	p.conn = pc.(*net.UDPConn)
	p.mu.Unlock()
	go p.readLoop()
	return nil
}

func (p *UDPProxy) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil
	}
	return p.conn.LocalAddr()
}

func (p *UDPProxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	for _, flow := range p.flows {
		flow.cancel()
	}
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

func (p *UDPProxy) readLoop() {
	data := make([]byte, meshsession.MaxUDPPayload+1)
	oob := make([]byte, 256)
	for {
		n, oobn, _, source, err := p.conn.ReadMsgUDP(data, oob)
		if err != nil {
			return
		}
		if n == 0 || n > meshsession.MaxUDPPayload || source == nil {
			continue
		}
		destination, ifindex, err := originalUDPMetadata(oob[:oobn])
		if err != nil {
			continue
		}
		src, ok := netip.AddrFromSlice(source.IP)
		if !ok || !src.Is4() {
			continue
		}
		p.handlePacket(udpKey{source: netip.AddrPortFrom(src.Unmap(), uint16(source.Port)), destination: destination, ifindex: ifindex}, data[:n])
	}
}

func originalUDPMetadata(oob []byte) (netip.AddrPort, int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	var destination netip.AddrPort
	var ifindex int
	for _, msg := range msgs {
		if msg.Header.Level != unix.IPPROTO_IP {
			continue
		}
		if msg.Header.Type == unix.IP_PKTINFO && len(msg.Data) >= 4 {
			ifindex = int(binary.NativeEndian.Uint32(msg.Data[:4]))
		}
		if msg.Header.Type == unix.IP_ORIGDSTADDR && len(msg.Data) >= 8 {
			var ip [4]byte
			copy(ip[:], msg.Data[4:8])
			port := binary.BigEndian.Uint16(msg.Data[2:4])
			if port != 0 {
				destination = netip.AddrPortFrom(netip.AddrFrom4(ip), port)
			}
		}
	}
	if !destination.IsValid() || ifindex <= 0 {
		return netip.AddrPort{}, 0, errors.New("missing original UDP destination or ingress interface")
	}
	return destination, ifindex, nil
}

func (p *UDPProxy) handlePacket(key udpKey, payload []byte) {
	key.token = p.sources.SourceToken(key.source.Addr(), key.destination.Addr(), key.ifindex)
	if key.token == 0 {
		return
	}
	peer, err := DeviceForVIP(key.destination.Addr())
	if err != nil || key.destination.Port() == 0 {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	flow := p.flows[key]
	if flow == nil {
		if len(p.flows) >= udpFlowLimit {
			p.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		flow = &udpProxyFlow{queue: make(chan []byte, udpQueueLimit), cancel: cancel}
		p.flows[key] = flow
		go p.runFlow(ctx, key, peer, flow)
	}
	select {
	case flow.queue <- append([]byte(nil), payload...):
	default:
	}
	p.mu.Unlock()
}

func (p *UDPProxy) runFlow(ctx context.Context, key udpKey, peer int32, flow *udpProxyFlow) {
	defer func() {
		flow.cancel()
		p.mu.Lock()
		if p.flows[key] == flow {
			delete(p.flows, key)
		}
		p.mu.Unlock()
	}()
	// BLE carries the end-to-end QUIC handshake through a high-cost, ordered
	// hop. Concurrent app TCP handshakes can delay the UDP port ACK beyond
	// 20 seconds while the hop is healthy. Keep setup bounded independently
	// of the per-flow idle timeout.
	dialCtx, cancel := context.WithTimeout(ctx, udpDialTimeout)
	remote, err := p.dialer.DialUDP(dialCtx, peer, key.destination.Port())
	cancel()
	if err != nil {
		return
	}
	defer remote.Close()
	reply, err := p.replyDial(key.destination, key.source)
	if err != nil {
		return
	}
	defer reply.Close()
	failed := make(chan struct{})
	go func() {
		defer close(failed)
		for {
			data, err := remote.Receive(ctx)
			if err != nil {
				return
			}
			_ = reply.SetWriteDeadline(time.Now().Add(time.Second))
			if err := p.sources.WithSourceToken(key.source.Addr(), key.destination.Addr(), key.ifindex, key.token, func() error {
				_, err := reply.Write(data)
				return err
			}); err != nil {
				return
			}
		}
	}()
	idle := time.NewTimer(udpFlowIdle)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-failed:
			return
		case <-idle.C:
			return
		case data := <-flow.queue:
			if err := p.sources.WithSourceToken(key.source.Addr(), key.destination.Addr(), key.ifindex, key.token, func() error { return remote.Send(data) }); err != nil {
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(udpFlowIdle)
		}
	}
}

func dialUDPReply(source, destination netip.AddrPort) (*net.UDPConn, error) {
	lc := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(source), Control: func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		err := raw.Control(func(fd uintptr) {
			if sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1); sockErr != nil {
				return
			}
			if sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1); sockErr != nil {
				return
			}
			if sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); sockErr != nil {
				return
			}
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return sockErr
	}}
	conn, err := lc.DialContext(context.Background(), "udp4", destination.String())
	if err != nil {
		return nil, fmt.Errorf("UDP reply bind %s: %w", source, err)
	}
	return conn.(*net.UDPConn), nil
}
