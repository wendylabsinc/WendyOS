package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"

	"google.golang.org/grpc/credentials"
)

// AgentTransportCredentials preserves the actual attachment's authenticated
// TLS state for gRPC. Only this package's admitted management stream type can
// use the handoff. Ordinary network connections use fallback's normal handshake;
// nil fallback refuses them. Neither source addresses nor RPC metadata are
// identity inputs.
func AgentTransportCredentials(fallback credentials.TransportCredentials) credentials.TransportCredentials {
	return &agentCredentials{fallback: fallback}
}

type agentCredentials struct {
	fallback credentials.TransportCredentials
}

func agentTLSState(raw net.Conn, server bool) (tls.ConnectionState, bool) {
	c, ok := raw.(*agentStream)
	if !ok || c.link.server != server || !c.link.Live() {
		return tls.ConnectionState{}, false
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return tls.ConnectionState{}, false
	}
	return c.link.conn.ConnectionState(), true
}
func agentTLSInfo(state tls.ConnectionState) credentials.AuthInfo {
	return credentials.TLSInfo{State: state, CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}
}
func (c *agentCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if state, ok := agentTLSState(raw, false); ok {
		return raw, agentTLSInfo(state), nil
	}
	if c.fallback != nil {
		return c.fallback.ClientHandshake(ctx, authority, raw)
	}
	return nil, nil, errors.New("gRPC needs an admitted operator management stream")
}
func (c *agentCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if state, ok := agentTLSState(raw, true); ok {
		return raw, agentTLSInfo(state), nil
	}
	if c.fallback != nil {
		return c.fallback.ServerHandshake(raw)
	}
	return nil, nil, errors.New("gRPC needs an admitted operator management stream")
}
func (c *agentCredentials) Info() credentials.ProtocolInfo {
	if c.fallback != nil {
		return c.fallback.Info()
	}
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.3"}
}
func (c *agentCredentials) Clone() credentials.TransportCredentials {
	if c.fallback != nil {
		return AgentTransportCredentials(c.fallback.Clone())
	}
	return AgentTransportCredentials(nil)
}
func (c *agentCredentials) OverrideServerName(name string) error {
	if c.fallback != nil {
		return c.fallback.OverrideServerName(name)
	}
	return nil
}

// AgentListener is an in-memory ingress for the already registered authenticated
// gRPC server. It opens no socket or extra plaintext endpoint. Deliver retains
// the stream's handler lease until the stream closes or its owner is canceled.
type AgentListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func NewAgentListener() *AgentListener {
	return &AgentListener{connections: make(chan net.Conn), done: make(chan struct{})}
}
func (l *AgentListener) Deliver(ctx context.Context, c net.Conn) error {
	stream, ok := c.(*agentStream)
	if !ok || !stream.link.server {
		return errors.New("agent ingress requires a server operator stream")
	}
	if _, ok := agentTLSState(c, true); !ok {
		return net.ErrClosed
	}
	defer c.Close()
	select {
	case l.connections <- c:
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.done:
		return net.ErrClosed
	case <-l.done:
		return net.ErrClosed
	}
	select {
	case <-stream.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return net.ErrClosed
	}
}
func (l *AgentListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *AgentListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *AgentListener) Addr() net.Addr { return agentListenerAddr{} }

type agentListenerAddr struct{}

func (agentListenerAddr) Network() string { return "wendy-net-operator" }
func (agentListenerAddr) String() string  { return "authenticated-agent-stream" }

// MeshTLSListener is an in-process ingress for ordinary end-to-end agent TLS.
// It must be served only by the mesh-scoped TLS server and dialed only from the
// already authenticated app-session service. No network address, ALPN or peer
// metadata can select this listener. Unlike AgentListener, it never inherits an
// outer stream's identity: the bytes it delivers still require a TLS handshake.
type MeshTLSListener struct {
	connections chan *meshTLSConn
	done        chan struct{}
	mu          sync.Mutex
	closed      bool
	pipes       map[*meshTLSPipe]struct{}
}

func NewMeshTLSListener() *MeshTLSListener {
	return &MeshTLSListener{connections: make(chan *meshTLSConn), done: make(chan struct{}), pipes: make(map[*meshTLSPipe]struct{})}
}

type meshTLSPipe struct {
	listener *MeshTLSListener
	ends     [2]net.Conn
	done     chan struct{}
	once     sync.Once
}

func (p *meshTLSPipe) close() {
	p.once.Do(func() {
		// Publish full closure before either endpoint can return its I/O
		// error, so forwarders can distinguish it from a transport failure.
		close(p.done)
		_ = p.ends[0].Close()
		_ = p.ends[1].Close()
		p.listener.mu.Lock()
		delete(p.listener.pipes, p)
		p.listener.mu.Unlock()
	})
}

type meshTLSConn struct {
	net.Conn
	pipe *meshTLSPipe
}

func (c *meshTLSConn) Close() error { c.pipe.close(); return nil }

// Done reports owner revocation. An ordinary endpoint close must not revoke
// bytes already accepted by an outer proxy but still waiting to be forwarded.
func (c *meshTLSConn) Done() <-chan struct{} { return c.pipe.listener.done }

// Closed reports a full pipe close, not a TCP half-close. Forwarders drain the
// accepted outbound bytes before retiring their otherwise idle reverse read.
func (c *meshTLSConn) Closed() <-chan struct{} { return c.pipe.done }

// DialContext waits for the local TLS server to accept this pipe. Cancellation
// before handoff closes both ends; after handoff the ordinary connection and
// listener lifecycle own the pipe, as with net.Dialer.DialContext.
func (l *MeshTLSListener) DialContext(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, b := net.Pipe()
	p := &meshTLSPipe{listener: l, ends: [2]net.Conn{a, b}, done: make(chan struct{})}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = a.Close()
		_ = b.Close()
		return nil, net.ErrClosed
	}
	l.pipes[p] = struct{}{}
	l.mu.Unlock()
	select {
	case l.connections <- &meshTLSConn{Conn: b, pipe: p}:
		if err := ctx.Err(); err != nil {
			p.close()
			return nil, err
		}
		return &meshTLSConn{Conn: a, pipe: p}, nil
	case <-ctx.Done():
		p.close()
		return nil, ctx.Err()
	case <-l.done:
		p.close()
		return nil, net.ErrClosed
	}
}

func (l *MeshTLSListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		select {
		case <-l.done:
			_ = c.Close()
			return nil, net.ErrClosed
		default:
			return c, nil
		}
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *MeshTLSListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.done)
	pipes := make([]*meshTLSPipe, 0, len(l.pipes))
	for p := range l.pipes {
		pipes = append(pipes, p)
	}
	l.mu.Unlock()
	for _, p := range pipes {
		p.close()
	}
	return nil
}

func (l *MeshTLSListener) Addr() net.Addr { return meshTLSListenerAddr{} }

type meshTLSListenerAddr struct{}

func (meshTLSListenerAddr) Network() string { return "wendy-net-inner-tls" }
func (meshTLSListenerAddr) String() string  { return "mesh-agent-tls-ingress" }
