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
