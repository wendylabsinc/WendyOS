//go:build linux

package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

type udpKey struct {
	source, destination netip.AddrPort
	ifindex             int
	token               uint64
}
type udpProxyFlow struct {
	endpoint *udpmessage.Endpoint
	activity chan struct{}
	cancel   context.CancelFunc
}

// UDPProxy accepts only source addresses owned by running mesh-mode apps and
// original destinations inside their service CIDR. One QUIC connection per
// source/destination tuple preserves independent reply mappings.
type UDPProxy struct {
	vips        *VIPTable
	dialer      UDPDialer
	sources     UDPSourcePolicy
	mu          sync.Mutex
	flows       map[udpKey]*udpProxyFlow
	conn        *net.UDPConn
	closed      bool
	replies     *udpReplySender
	manager     *udpmessage.Manager
	ownsManager bool
	wg          sync.WaitGroup
}

func NewUDPProxy(dialer UDPDialer, sources UDPSourcePolicy, vips *VIPTable, managers ...*udpmessage.Manager) (*UDPProxy, error) {
	if dialer == nil || sources == nil || vips == nil {
		return nil, errors.New("missing UDP net dialer or app source registry")
	}
	manager := udpmessage.NewManager()
	owns := true
	if len(managers) > 0 && managers[0] != nil {
		manager = managers[0]
		owns = false
	}
	return &UDPProxy{vips: vips, dialer: dialer, sources: sources, flows: make(map[udpKey]*udpProxyFlow), manager: manager, ownsManager: owns}, nil
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
	replies, err := newUDPReplySender()
	if err != nil {
		_ = pc.Close()
		return err
	}
	p.mu.Lock()
	if p.closed || p.conn != nil {
		closed := p.closed
		p.mu.Unlock()
		_ = pc.Close()
		_ = replies.Close()
		if closed {
			return net.ErrClosed
		}
		return errors.New("net UDP proxy is already started")
	}
	p.replies = replies
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
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	for _, flow := range p.flows {
		flow.cancel()
	}
	var errs []error
	if p.conn != nil {
		errs = append(errs, p.conn.Close())
	}
	if p.replies != nil {
		errs = append(errs, p.replies.Close())
	}
	p.mu.Unlock()
	p.wg.Wait()
	if p.ownsManager {
		p.manager.Close()
	}
	return errors.Join(errs...)
}

func (p *UDPProxy) readLoop() {
	data := make([]byte, meshsession.MaxUDPMessageSize+1)
	oob := make([]byte, 256)
	for {
		n, oobn, flags, source, err := p.conn.ReadMsgUDP(data, oob)
		if err != nil {
			return
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n > meshsession.MaxUDPMessageSize || source == nil {
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
		p.manager.RecordDenied()
		return
	}
	peer, err := p.vips.Device(key.destination.Addr())
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
		guard := udpmessage.Guard(func(send func() error) error {
			return p.sources.WithSourceToken(key.source.Addr(), key.destination.Addr(), key.ifindex, key.token, send)
		})
		endpoint, err := p.manager.Open(peer, guard)
		if err != nil {
			cancel()
			p.mu.Unlock()
			return
		}
		flow = &udpProxyFlow{endpoint: endpoint, activity: make(chan struct{}, 1), cancel: cancel}
		p.flows[key] = flow
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.runFlow(ctx, key, peer, flow) }()
	}
	_ = flow.endpoint.Send(payload)
	select {
	case flow.activity <- struct{}{}:
	default:
	}
	p.mu.Unlock()
}

func (p *UDPProxy) runFlow(ctx context.Context, key udpKey, peer meshid.Device, flow *udpProxyFlow) {
	defer func() {
		flow.cancel()
		flow.endpoint.Close()
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
	sourceDone := p.sources.SourceDone(key.source.Addr(), key.destination.Addr(), key.ifindex, key.token)
	go func() {
		select {
		case <-sourceDone:
			flow.cancel()
		case <-flow.endpoint.Done():
			flow.cancel()
		case <-ctx.Done():
		}
	}()
	remote, err := p.dialer.DialUDP(dialCtx, UDPDialRequest{Peer: peer, Port: key.destination.Port(), Endpoint: flow.endpoint, Done: sourceDone, Source: meshsession.UDPSourceClaim{IP: key.source.Addr(), VIP: key.destination.Addr(), IfIndex: key.ifindex, Token: key.token}})
	cancel()
	if err != nil {
		return
	}
	failed := make(chan struct{})
	defer func() { flow.cancel(); _ = remote.Close(); <-failed }()
	go func() {
		defer close(failed)
		for {
			if err := p.deliverReply(ctx, remote, flow, key); err != nil {
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
		case <-flow.activity:
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

// Keep the full message charge through the final source-token-protected write.
func (p *UDPProxy) deliverReply(ctx context.Context, remote UDPFlow, flow *udpProxyFlow, key udpKey) error {
	message, err := remote.Receive(ctx)
	if err != nil {
		return err
	}
	defer message.Release()
	return flow.endpoint.Deliver(func() error {
		return p.replies.Send(message.Payload, key.destination, key.source, key.ifindex)
	})
}
