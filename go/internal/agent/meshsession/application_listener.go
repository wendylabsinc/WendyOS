package meshsession

import (
	"context"
	"net"
	"sync"
)

// ApplicationLANPort is advertised in the enrolled agent's app-tls TXT record.
// It carries only the app TLS/WAS protocol, never administrative agent RPCs.
const ApplicationLANPort = 43023

// ApplicationListener delivers admitted Cloud transports to ServeTLS without
// opening a network socket. Every delivered byte still passes through the
// application protocol's mutually authenticated TLS and live port admission.
type ApplicationListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func NewApplicationListener() *ApplicationListener {
	return &ApplicationListener{connections: make(chan net.Conn), done: make(chan struct{})}
}

func (l *ApplicationListener) Dial(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	local, remote := net.Pipe()
	select {
	case <-l.done:
		_ = local.Close()
		_ = remote.Close()
		return nil, net.ErrClosed
	default:
	}
	select {
	case l.connections <- remote:
		return local, nil
	case <-ctx.Done():
		_ = local.Close()
		_ = remote.Close()
		return nil, ctx.Err()
	case <-l.done:
		_ = local.Close()
		_ = remote.Close()
		return nil, net.ErrClosed
	}
}

func (l *ApplicationListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *ApplicationListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *ApplicationListener) Addr() net.Addr { return applicationAddr{} }

type applicationAddr struct{}

func (applicationAddr) Network() string { return "wendy-app" }
func (applicationAddr) String() string  { return "authenticated-application-stream" }
