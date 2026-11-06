package meshsession

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type localUDPAuthorizer interface {
	UDPAuthorizer
	WithAuthorizedLocalUDP(netip.Addr, netip.Addr, int, uint64, uint16, uint64, func(netip.AddrPort) error) error
}
type LocalDatagramFlow struct {
	conn        *net.UDPConn
	auth        UDPAuthorizer
	port        uint16
	token       uint64
	endpoint    *udpmessage.Endpoint
	readMu      udpReceiveLock
	once        sync.Once
	ownsManager bool
}

func DialLocalUDP(ctx context.Context, auth UDPAuthorizer, port uint16, options ...UDPOptions) (*LocalDatagramFlow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil || port == 0 {
		return nil, ErrDenied
	}
	opts := udpOptions(options)
	token := auth.UDPToken(port)
	guard := udpmessage.Guard(func(send func() error) error {
		return auth.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send() })
	})
	if source := opts.Source; source != nil {
		combined, ok := auth.(localUDPAuthorizer)
		if !ok {
			return nil, ErrDenied
		}
		guard = func(send func() error) error {
			return combined.WithAuthorizedLocalUDP(source.IP, source.VIP, source.IfIndex, source.Token, port, token, func(netip.AddrPort) error { return send() })
		}
	}
	// Reserve flow and a conservative configured-socket allowance before
	// creating the socket. Replace the allowance with actual kernel values.
	if err := guard(func() error { return nil }); err != nil {
		return nil, err
	}
	endpoint := opts.Endpoint
	manager := opts.Manager
	owns := false
	if endpoint != nil {
		if manager != nil && manager != endpoint.Manager() {
			return nil, udpmessage.ErrMalformed
		}
		manager = endpoint.Manager()
	}
	var err error
	if endpoint == nil {
		if manager == nil {
			manager = udpmessage.NewManager()
			owns = true
		}
		// Production supplies a preallocated endpoint with its local peer
		// identity. Direct standalone callers share an explicit local identity.
		tenant, _ := meshid.ParseTenant("00000000-0000-0000-0000-000000000000")
		peer, _ := meshid.ParseDevice(tenant, "00000000-0000-0000-0000-000000000000")
		endpoint, err = manager.Open(peer, guard)
		if err != nil {
			if owns {
				manager.Close()
			}
			return nil, err
		}
	}
	var conn *net.UDPConn
	fail := func(err error) (*LocalDatagramFlow, error) {
		if conn != nil {
			conn.Close()
		}
		endpoint.Close()
		if owns {
			manager.Close()
		}
		return nil, err
	}
	if err := endpoint.ReserveSocketBytes(512 << 10); err != nil {
		return fail(err)
	}
	err = auth.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		var err error
		conn, err = net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(address))
		return err
	})
	if err != nil {
		return fail(err)
	}
	if err := configureLocalUDP(conn); err != nil {
		return fail(err)
	}
	socketBytes, err := localUDPBuffers(conn)
	if err == nil {
		err = endpoint.ReserveSocketBytes(socketBytes)
	}
	if err != nil {
		return fail(err)
	}
	if err := endpoint.SetLocalTransport(MaxUDPTransportChunk, guard, func(payload []byte) error { return writeLocalUDP(conn, payload) }); err != nil {
		return fail(err)
	}
	// The endpoint guard belongs to the source; do not nest it around the
	// combined local guard. The local transport's combined guard is atomic.
	f := &LocalDatagramFlow{conn: conn, auth: auth, port: port, token: token, endpoint: endpoint, ownsManager: owns}
	go func() {
		select {
		case <-endpoint.Done():
		case <-auth.UDPDone(port, token):
		case <-opts.Done:
		}
		_ = f.Close()
	}()
	return f, nil
}
func (f *LocalDatagramFlow) Send(payload []byte) error {
	if f == nil || f.endpoint == nil {
		return net.ErrClosed
	}
	return localUDPError(f.endpoint.Send(payload))
}
func localUDPError(err error) error {
	if errors.Is(err, udpmessage.ErrClosed) {
		return net.ErrClosed
	}
	return err
}
func (f *LocalDatagramFlow) Receive(ctx context.Context) (*udpmessage.Message, error) {
	if err := f.readMu.lock(ctx, f.endpoint.Done()); err != nil {
		return nil, localUDPError(err)
	}
	defer f.readMu.unlock()
	buf := make([]byte, MaxUDPMessageSize+1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.endpoint.WithGuard(func() error { return nil }); err != nil {
			return nil, localUDPError(err)
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := f.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, _, flags, _, err := f.conn.ReadMsgUDP(buf, nil)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return nil, err
		}
		if udpReadTruncated(flags) || n > MaxUDPMessageSize {
			continue
		}
		if err := f.endpoint.WithGuard(func() error { return nil }); err != nil {
			return nil, localUDPError(err)
		}
		message, err := f.endpoint.RetainReceived(buf[:n])
		if errors.Is(err, udpmessage.ErrCapacity) {
			continue
		}
		return message, localUDPError(err)
	}
}

func (f *LocalDatagramFlow) Close() error {
	if f == nil {
		return nil
	}
	var err error
	f.once.Do(func() {
		err = f.conn.Close()
		f.endpoint.Close()
		if f.ownsManager {
			f.endpoint.Manager().Close()
		}
	})
	return err
}
