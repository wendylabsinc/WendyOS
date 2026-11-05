package vpn

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Engine terminates TCP from utun and relays the byte stream through the app's
// authenticated mesh/cloud transport. Only the DNS service accepts UDP.
// ICMP echo replies require a successful remote agent probe.
type Engine struct {
	cfg            Config
	stack          *stack.Stack
	link           *channel.Endpoint
	ctx            context.Context
	cancel         context.CancelFunc
	once           sync.Once
	echoes         chan echoRequest
	echoWG         sync.WaitGroup
	pumpWG         sync.WaitGroup
	packets        chan []byte
	internet       net.Conn
	internetWrites chan []byte
}

func NewEngine(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{cfg: cfg, ctx: ctx, cancel: cancel, packets: make(chan []byte, 256), internetWrites: make(chan []byte, 128)}
	e.stack = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	e.link = channel.New(256, 1400, "")
	fail := func(err tcpip.Error) (*Engine, error) { e.Close(); return nil, errors.New(err.String()) }
	if err := e.stack.CreateNIC(1, e.link); err != nil {
		return fail(err)
	}
	if err := e.stack.SetPromiscuousMode(1, true); err != nil {
		return fail(err)
	}
	if err := e.stack.SetSpoofing(1, true); err != nil {
		return fail(err)
	}
	e.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	tcpSlots := make(chan struct{}, 128)
	udpSlots := make(chan struct{}, 64)
	forward := tcp.NewForwarder(e.stack, 0, 128, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		address := id.LocalAddress.String()
		isDNS := address == DNSAddress && id.LocalPort == 53
		if !isDNS && !e.allowed(address) {
			r.Complete(true)
			return
		}
		select {
		case tcpSlots <- struct{}{}:
		default:
			r.Complete(true)
			return
		}
		var queue waiter.Queue
		endpoint, err := r.CreateEndpoint(&queue)
		if err != nil {
			<-tcpSlots
			r.Complete(true)
			return
		}
		r.Complete(false)
		conn := gonet.NewTCPConn(&queue, endpoint)
		go func() {
			defer func() { <-tcpSlots }()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			if isDNS {
				e.serveTCPDNS(conn)
				return
			}
			remote, err := DialRelay(ctx, cfg, address, id.LocalPort)
			if err != nil {
				return
			}
			defer remote.Close()
			finish := context.AfterFunc(ctx, func() { remote.Close() })
			defer finish()
			Pipe(conn, remote)
		}()
	})
	e.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, forward.HandlePacket)
	udpForward := udp.NewForwarder(e.stack, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		if id.LocalAddress.String() != DNSAddress || id.LocalPort != 53 {
			return false
		}
		select {
		case udpSlots <- struct{}{}:
		default:
			return false
		}
		var queue waiter.Queue
		endpoint, err := r.CreateEndpoint(&queue)
		if err != nil {
			<-udpSlots
			return false
		}
		conn := gonet.NewUDPConn(&queue, endpoint)
		go func() {
			defer func() { <-udpSlots }()
			defer conn.Close()
			timeout := 3 * time.Second
			if cfg.Internet != nil {
				timeout = 12 * time.Second
			}
			conn.SetDeadline(time.Now().Add(timeout))
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			buf := make([]byte, 4096)
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			answer, err := e.answerDNS(buf[:n])
			if err == nil {
				conn.Write(answer)
			}
		}()
		return true
	})
	e.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpForward.HandlePacket)
	e.startEchoWorkers()
	e.pumpWG.Add(1)
	go func() {
		defer e.pumpWG.Done()
		for {
			p := e.link.ReadContext(ctx)
			if p == nil {
				return
			}
			view := p.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			p.DecRef()
			select {
			case e.packets <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	if cfg.Internet != nil {
		if err := e.startInternet(); err != nil {
			e.Close()
			return nil, err
		}
	}
	context.AfterFunc(ctx, func() {
		if e.internet != nil {
			e.internet.Close()
		}
	})

	return e, nil
}
func (e *Engine) allowed(ip string) bool {
	for _, r := range e.cfg.Routes {
		if r.Address == ip {
			return true
		}
	}
	return false
}
func (e *Engine) serveTCPDNS(conn net.Conn) {
	for {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint16(size[:])
		if n > 4096 || n < 12 {
			return
		}
		wire := make([]byte, n)
		if _, err := io.ReadFull(conn, wire); err != nil {
			return
		}
		reply, err := e.answerDNS(wire)
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
		if _, err = conn.Write(append(size[:], reply...)); err != nil {
			return
		}
	}
}
func (e *Engine) Write(packet []byte) {
	if e.writeInternet(packet) {
		return
	}
	if len(packet) < 20 || packet[0]>>4 != 4 || len(packet) > 65535 {
		return
	}
	if packet[9] == uint8(header.ICMPv4ProtocolNumber) {
		e.queueEcho(packet)
		return
	}
	p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
	defer p.DecRef()
	e.link.InjectInbound(ipv4.ProtocolNumber, p)
}
func (e *Engine) Read(ctx context.Context) []byte {
	select {
	case <-ctx.Done():
		return nil
	case <-e.ctx.Done():
		return nil
	case p := <-e.packets:
		return p
	}
}
func (e *Engine) Close() {
	e.once.Do(func() {
		e.cancel()
		if e.internet != nil {
			e.internet.Close()
		}
		e.pumpWG.Wait()
		e.echoWG.Wait()
		if e.stack != nil {
			e.stack.Close()
		}
		if e.link != nil {
			e.link.Close()
		}
		if e.stack != nil {
			e.stack.Wait()
		}
	})
}

func (e *Engine) Healthy() bool {
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	defer cancel()
	return e.ctx.Err() == nil && Healthy(ctx, e.cfg)
}
