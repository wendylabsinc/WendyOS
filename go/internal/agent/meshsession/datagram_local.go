package meshsession

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// LocalDatagramFlow reaches a sibling app through the same protocol and
// incarnation authorization as peer UDP ingress. It needs no peer route.
type LocalDatagramFlow struct {
	conn   *net.UDPConn
	auth   UDPAuthorizer
	port   uint16
	token  uint64
	readMu sync.Mutex
}

func DialLocalUDP(ctx context.Context, auth UDPAuthorizer, port uint16) (*LocalDatagramFlow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil || port == 0 {
		return nil, ErrDenied
	}
	token := auth.UDPToken(port)
	var conn *net.UDPConn
	err := auth.WithAuthorizedUDPToken(port, token, func() error {
		var err error
		conn, err = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &LocalDatagramFlow{conn: conn, auth: auth, port: port, token: token}, nil
}

func (f *LocalDatagramFlow) Send(payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxUDPPayload {
		return errors.New("invalid mesh UDP payload size")
	}
	return f.auth.WithAuthorizedUDPToken(f.port, f.token, func() error {
		if err := f.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		_, err := f.conn.Write(payload)
		return err
	})
}

func (f *LocalDatagramFlow) Receive(ctx context.Context) ([]byte, error) {
	f.readMu.Lock()
	defer f.readMu.Unlock()
	buf := make([]byte, MaxUDPPayload+1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.auth.WithAuthorizedUDPToken(f.port, f.token, func() error { return nil }); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := f.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, err := f.conn.Read(buf)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return nil, err
		}
		if n == 0 || n > MaxUDPPayload {
			return nil, errors.New("invalid mesh UDP reply size")
		}
		if err := f.auth.WithAuthorizedUDPToken(f.port, f.token, func() error { return nil }); err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}
func (f *LocalDatagramFlow) Close() error { return f.conn.Close() }
