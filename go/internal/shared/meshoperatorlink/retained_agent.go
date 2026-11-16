package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
)

// RetainedAgentIdentity must come from the authenticated retained-session owner,
// never a socket address, request metadata, or peer-supplied TLS fields. The
// owner pins the original identity and revokes Done on logical session death.
// This handoff supplies authentication only; normal gRPC RPC policy still runs.
type RetainedAgentIdentity interface {
	TLSState() (tls.ConnectionState, error)
	Done() <-chan struct{}
}

func retainedIdentityState(identity RetainedAgentIdentity) (tls.ConnectionState, error) {
	if identity == nil || identity.Done() == nil {
		return tls.ConnectionState{}, errors.New("missing retained management identity")
	}
	select {
	case <-identity.Done():
		return tls.ConnectionState{}, net.ErrClosed
	default:
	}
	state, err := identity.TLSState()
	if err != nil {
		return tls.ConnectionState{}, err
	}
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || len(state.PeerCertificates) == 0 || state.PeerCertificates[0] == nil {
		return tls.ConnectionState{}, errors.New("invalid retained management authentication")
	}
	return state, nil
}

// RetainedAgentListener opens no network endpoint. Only its authenticated Dial
// is available to native-service dispatch; ordinary pipes cannot select the
// retained credential path. Serve it with the existing authenticated gRPC server.
type RetainedAgentListener struct{ pipes *MeshTLSListener }

func NewRetainedAgentListener() *RetainedAgentListener {
	return &RetainedAgentListener{pipes: NewMeshTLSListener()}
}
func (l *RetainedAgentListener) Accept() (net.Conn, error) { return l.pipes.Accept() }
func (l *RetainedAgentListener) Close() error              { return l.pipes.Close() }
func (l *RetainedAgentListener) Addr() net.Addr            { return l.pipes.Addr() }
func (l *RetainedAgentListener) Dial(ctx context.Context, identity RetainedAgentIdentity) (net.Conn, error) {
	if _, err := retainedIdentityState(identity); err != nil {
		return nil, err
	}
	return l.pipes.dialContext(ctx, identity)
}

type retainedAgentClientConn struct {
	net.Conn
	identity RetainedAgentIdentity
	done     chan struct{}
	once     sync.Once
}

func (c *retainedAgentClientConn) Close() error {
	var err error
	c.once.Do(func() { close(c.done); err = c.Conn.Close() })
	return err
}

// RetainedAgentClient takes ownership only on success. Cancellation of the
// original dial context after handoff must not kill a live management stream;
// the retained identity and explicit Close govern it instead.
func RetainedAgentClient(conn net.Conn, identity RetainedAgentIdentity) (net.Conn, error) {
	if conn == nil {
		return nil, errors.New("missing retained management stream")
	}
	if _, err := retainedIdentityState(identity); err != nil {
		return nil, err
	}
	c := &retainedAgentClientConn{Conn: conn, identity: identity, done: make(chan struct{})}
	go func() {
		select {
		case <-identity.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c, nil
}

// Recognized but revoked/wrong-direction streams must fail directly, never
// fall through into a new TLS handshake or another identity namespace.
func retainedAgentTLSState(raw net.Conn, server bool) (tls.ConnectionState, bool, error) {
	var identity RetainedAgentIdentity
	var closed <-chan struct{}
	switch c := raw.(type) {
	case *retainedAgentClientConn:
		if server {
			return tls.ConnectionState{}, true, errors.New("client retained stream on server")
		}
		identity, closed = c.identity, c.done
	case *meshTLSConn:
		if c.identity == nil {
			return tls.ConnectionState{}, false, nil
		}
		if !server {
			return tls.ConnectionState{}, true, errors.New("server retained stream on client")
		}
		identity, closed = c.identity, c.pipe.done
	default:
		return tls.ConnectionState{}, false, nil
	}
	select {
	case <-closed:
		return tls.ConnectionState{}, true, net.ErrClosed
	default:
	}
	state, err := retainedIdentityState(identity)
	return state, true, err
}
