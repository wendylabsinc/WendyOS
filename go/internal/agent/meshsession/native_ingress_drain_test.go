package meshsession

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

// Hold bytes after the local pipe has accepted them but before the outer
// transport receives them. This makes the endpoint-close/drain ordering exact.
type nativeDrainBarrier struct {
	net.Conn
	entered, release, closed, writeFailed chan struct{}
	readOnce, closeOnce, writeOnce        sync.Once
}

func (c *nativeDrainBarrier) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.readOnce.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.Done():
			return 0, net.ErrClosed
		}
		select {
		case <-c.Done():
			return 0, net.ErrClosed
		default:
		}
	}
	return n, err
}

func (c *nativeDrainBarrier) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *nativeDrainBarrier) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.writeOnce.Do(func() { close(c.writeFailed) })
	}
	return n, err
}

func (c *nativeDrainBarrier) Done() <-chan struct{} {
	return c.Conn.(interface{ Done() <-chan struct{} }).Done()
}

func (c *nativeDrainBarrier) Closed() <-chan struct{} {
	return c.Conn.(interface{ Closed() <-chan struct{} }).Closed()
}

func TestMeshNativeIngressDrainsNormalCloseAndRevokesOwnerShutdown(t *testing.T) {
	for _, transport := range []string{"tls", "quic"} {
		for _, name := range []string{"ordinary-close", "ordinary-close-with-input", "owner-shutdown", "credential-expiry"} {
			t.Run(transport+"/"+name, func(t *testing.T) {
				now := time.Now().Truncate(time.Second)
				var floor atomic.Int64
				floor.Store(now.UnixNano())
				policy := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return time.Unix(0, floor.Load()) }, Uncertain: func() bool { return true }}
				strictClient, controlledServer, _ := nativeIngressIdentities(t, policy, now)
				a, b := strictClient.credentials, controlledServer.credentials
				listener := meshoperatorlink.NewMeshTLSListener()
				t.Cleanup(func() { listener.Close() })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
				defer releaseRead()
				wrapped := make(chan *nativeDrainBarrier, 1)
				auth := WithAgentServiceDial(nil, 50052, func() (net.Conn, error) {
					conn, err := listener.DialContext(ctx)
					if err != nil {
						return nil, err
					}
					gate := &nativeDrainBarrier{Conn: conn, entered: make(chan struct{}), release: release, closed: make(chan struct{}), writeFailed: make(chan struct{})}
					wrapped <- gate
					return gate, nil
				})
				var address netip.AddrPort
				if transport == "tls" {
					_, address = startTLSAppTest(t, b, auth)
				} else {
					server, err := NewServer(b, auth)
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
					t.Cleanup(func() {
						cancel()
						server.Close()
						if err := <-done; err != nil {
							t.Error(err)
						}
					})
					for server.Addr() == nil {
						select {
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(time.Millisecond):
						}
					}
					address = netip.MustParseAddrPort(server.Addr().String())
				}
				client, err := NewClient(a)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				accepted := make(chan net.Conn, 1)
				go func() { conn, _ := listener.Accept(); accepted <- conn }()
				var flow net.Conn
				if transport == "tls" {
					flow, err = client.DialTLS(ctx, b.Device, address, 50052)
				} else {
					flow, err = client.Dial(ctx, b.Device, address, 50052)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer flow.Close()
				inner, gate := <-accepted, <-wrapped
				payload := bytes.Repeat([]byte("accepted-before-close"), 20)
				written := make(chan error, 1)
				go func() { _, err := inner.Write(payload); written <- err }()
				select {
				case <-gate.entered:
				case <-ctx.Done():
					t.Fatal("local bytes were not accepted", ctx.Err())
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				flow.SetDeadline(time.Now().Add(time.Second))
				if name == "owner-shutdown" || name == "credential-expiry" {
					if name == "owner-shutdown" {
						listener.Close()
					} else {
						// Only the server's certificate policy changes; the
						// client remains valid at ordinary wall time. Its guard
						// must interrupt the blocked drain without client help.
						floor.Store(now.Add(2 * time.Hour).UnixNano())
						select {
						case <-gate.closed:
						case <-ctx.Done():
							t.Fatal("credential expiry waited for outbound drain")
						}
						flow.SetDeadline(time.Now().Add(time.Second))
					}
					var one [1]byte
					_, err := flow.Read(one[:])
					var timed net.Error
					if err == nil || errors.As(err, &timed) && timed.Timeout() {
						t.Fatal("revocation waited for outbound drain", err)
					}
				} else {
					inner.Close()
					if name == "ordinary-close-with-input" {
						// Force the reverse copy to encounter the already closed
						// pipe before allowing the accepted response to drain.
						go func() { _, _ = flow.Write([]byte("client write side remains open")) }()
						select {
						case <-gate.writeFailed:
						case <-ctx.Done():
							t.Fatal("reverse copy did not observe local close")
						}
						if transport == "tls" {
							// Keep the response blocked while the incoming
							// worker reports its write error to the coordinator.
							// Premature closure is checked by the payload assertion.
							select {
							case <-gate.closed:
							case <-time.After(100 * time.Millisecond):
							}
						}
					}
					releaseRead()
					got, err := io.ReadAll(flow)
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("close lost accepted bytes: got %d of %d, error %v", len(got), len(payload), err)
					}
				}
				// Do not close the client's write direction. A full local close
				// must still retire the server's reverse copy worker promptly.
				select {
				case <-gate.closed:
				case <-ctx.Done():
					t.Fatal("closed local pipe left its proxy waiting for the client's write side")
				}
			})
		}
	}
}
