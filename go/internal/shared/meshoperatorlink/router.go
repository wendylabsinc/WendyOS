package meshoperatorlink

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// RouterALPN carries a full Babel participant, independently of the retired
// delegated endpoint handshake. Adjacent management shares this actual TLS.
const RouterALPN = "wendy-net-operator-router/1"
const MaxRouterControl = (64 << 10) + 4

// RouterLink reuses the bounded management credit ring and its lifetime owner.
// Only the routing queues are new: their contents remain opaque here and are
// validated by the shared localmesh Node. No delegated route is installed.
type RouterLink struct {
	*Link
	peer     meshid.Device
	packets  chan []byte
	controls chan []byte
	transmit chan []byte
}

func (l *RouterLink) Peer() meshid.Device                  { return l.peer }
func (l *RouterLink) ConnectionState() tls.ConnectionState { return l.conn.ConnectionState() }

func routerBegin(ctx context.Context, c *tls.Conn) (func(), error) {
	if c == nil {
		return nil, errors.New("missing operator router TLS")
	}
	s := c.ConnectionState()
	if !s.HandshakeComplete || s.Version != tls.VersionTLS13 || s.NegotiatedProtocol != RouterALPN {
		return nil, errors.New("full operator router requires its negotiated TLS role")
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.NetConn().Close() })
	return func() { stop() }, nil
}

// RouterClient authenticates the actual selected WendyOS peer and binds this
// installation. Its own router ID/sequence comes only from its local Babel Node.
func RouterClient(ctx context.Context, c *tls.Conn, cfg ClientConfig) (_ *RouterLink, err error) {
	stop, err := routerBegin(ctx, c)
	if err != nil {
		return nil, err
	}
	defer stop()
	ok := false
	defer func() {
		if !ok {
			_ = c.NetConn().Close()
		}
	}()
	if cfg.Instance == (meshoperator.InstanceID{}) || !cfg.Identity.Operator().IsValid() {
		return nil, errors.New("missing operator router installation")
	}
	expires, err := verifyRouterState(c.ConnectionState(), cfg, time.Now())
	if err != nil {
		return nil, err
	}
	if len(c.ConnectionState().PeerCertificates) == 0 {
		return nil, errors.New("missing router identity")
	}
	if err = writeControl(c, control{Kind: "operator-router-hello", Instance: cfg.Instance}); err != nil {
		return nil, err
	}
	reply, err := readControl(c)
	if err != nil {
		return nil, err
	}
	if reply.Kind != "operator-router-ready" {
		return nil, errors.New("missing operator router readiness")
	}
	peer, err := meshid.PeerFromPrincipal(cfg.Identity.Operator().Principal())
	if err != nil {
		return nil, err
	}
	self, err := peer.WithInstance(uuid.UUID(cfg.Instance))
	if err != nil {
		return nil, err
	}
	l, err := routerReady(ctx, c, cfg.Router, self, false, minTime(expires, cfg.Identity.NotAfter()), nil)
	if err != nil {
		return nil, err
	}
	ok = true
	return l, nil
}

// AcceptRouter admits a real primary operator as a Babel neighbour. No caller
// supplies a fake enrolled device or a first-hop route offer.
func AcceptRouter(ctx context.Context, c *tls.Conn, tenant meshid.Tenant, roots []*x509.Certificate, agent func(context.Context, net.Conn) error) (_ *RouterLink, err error) {
	stop, err := routerBegin(ctx, c)
	if err != nil {
		return nil, err
	}
	defer stop()
	ok := false
	defer func() {
		if !ok {
			_ = c.NetConn().Close()
		}
	}()
	s := c.ConnectionState()
	chain := make([][]byte, len(s.PeerCertificates))
	for i, p := range s.PeerCertificates {
		chain[i] = p.Raw
	}
	identity, err := meshoperator.VerifyPrimary(chain, roots, tenant, time.Now())
	if err != nil {
		return nil, err
	}
	hello, err := readControl(c)
	if err != nil {
		return nil, err
	}
	if hello.Kind != "operator-router-hello" || hello.Instance == (meshoperator.InstanceID{}) {
		return nil, errors.New("missing operator router installation hello")
	}
	peer, err := meshid.PeerFromPrincipal(identity.Operator().Principal())
	if err != nil {
		return nil, err
	}
	peer, err = peer.WithInstance(uuid.UUID(hello.Instance))
	if err != nil {
		return nil, err
	}
	if err = writeControl(c, control{Kind: "operator-router-ready"}); err != nil {
		return nil, err
	}
	l, err := routerReady(ctx, c, peer, peer, true, identity.NotAfter(), agent)
	if err != nil {
		return nil, err
	}
	ok = true
	return l, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func routerReady(ctx context.Context, c *tls.Conn, peer, self meshid.Device, server bool, expires time.Time, agent func(context.Context, net.Conn) error) (*RouterLink, error) {
	base, err := ready(ctx, c, self.MeshAddress(), server, expires)
	if err != nil {
		return nil, err
	}
	base.agentHandler, base.agentContext = agent, ctx
	l := &RouterLink{Link: base, peer: peer, packets: make(chan []byte, 32), controls: make(chan []byte, 8), transmit: make(chan []byte, 32)}
	base.workers.Add(2)
	go func() { defer base.workers.Done(); defer base.Close(); _ = l.readRouter() }()
	go func() { defer base.workers.Done(); defer base.Close(); l.writeDatagrams() }()
	return l, nil
}

func (l *RouterLink) readRouter() error {
	for {
		kind, data, err := readFrame(l.conn)
		if err != nil {
			return err
		}
		switch kind {
		case 1:
			var m control
			if err = netwire.UnmarshalExtensibleArtifact(data, &m, maxControlBytes); err != nil {
				return err
			}
			if _, err = l.agentControl(m); err != nil {
				return err
			}
		case 3:
			if err = l.agentData(data); err != nil {
				return err
			}
		case 4:
			if len(data) > 1100 {
				return errors.New("oversized router datagram")
			}
			select {
			case l.packets <- data:
			case <-l.done:
				return io.EOF
			}
		case 5:
			select {
			case l.controls <- data:
			case <-l.done:
				return io.EOF
			}
		default: // Future bounded frame types do not tear down a carrier.
		}
	}
}

func (l *RouterLink) SendDatagram(data []byte) error {
	if len(data) == 0 || len(data) > 1100 {
		return errors.New("invalid router datagram size")
	}
	// Datagram congestion must not block the Babel actor. Like an ordinary
	// stream carrier, drop a congested datagram and let its protocol recover.
	select {
	case <-l.done:
		return net.ErrClosed
	default:
	}
	copyData := bytes.Clone(data)
	select {
	case <-l.done:
		return net.ErrClosed
	case l.transmit <- copyData:
		return nil
	default:
		return nil
	}
}
func (l *RouterLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case data := <-l.packets:
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, io.EOF
	}
}
func (l *RouterLink) WriteControlBytes(data []byte) error {
	if len(data) == 0 || len(data) > MaxRouterControl {
		return errors.New("invalid router control size")
	}
	return l.routerWrite(5, data)
}
func (l *RouterLink) ReadControlBytes() ([]byte, error) {
	select {
	case data := <-l.controls:
		return data, nil
	case <-l.done:
		return nil, io.EOF
	}
}
func (l *RouterLink) routerWrite(kind byte, data []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if !l.Live() {
		return net.ErrClosed
	}
	if err := writeFrame(l.conn, kind, bytes.Clone(data)); err != nil {
		_ = l.Close()
		return err
	}
	return nil
}

func (l *RouterLink) writeDatagrams() {
	for {
		select {
		case <-l.done:
			return
		case data := <-l.transmit:
			if err := l.routerWrite(4, data); err != nil {
				return
			}
		}
	}
}
