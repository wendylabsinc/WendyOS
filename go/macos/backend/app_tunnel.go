package backend

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const appTunnelSetupTimeout = 10 * time.Second

type appTunnelRenewal struct {
	done chan struct{}
	err  error
}

// App forwarders can outlive the certificate in their original connection.
// Re-enter the existing authenticated connect path before expiry.
func renewingAppTunnel(ctx context.Context, selector string, initial *grpcclient.AgentConnection, connect func(context.Context, string) (*grpcclient.AgentConnection, error)) func(context.Context, int) (net.Conn, error) {
	var mu sync.Mutex
	current := initial
	var owned []*grpcclient.AgentConnection
	var pending *appTunnelRenewal
	context.AfterFunc(ctx, func() {
		mu.Lock()
		closing := owned
		owned = nil
		mu.Unlock()
		for _, c := range closing {
			c.Close()
		}
	})

	renew := func(attempt *appTunnelRenewal) {
		// Setup belongs to the forward and retains its original account scope.
		setup, cancel := context.WithTimeout(ctx, appTunnelSetupTimeout)
		defer cancel()
		next, err := connect(setup, selector)
		if err == nil && (next == nil || next.RegistryDialer == nil) {
			err = errors.New("device has no TCP tunnel transport")
		}
		mu.Lock()
		if err == nil {
			err = setup.Err()
		}
		if err == nil {
			current = next
			owned = append(owned, next)
		}
		pending = nil
		attempt.err = err
		close(attempt.done)
		mu.Unlock()
		if err != nil && next != nil {
			next.Close()
		}
	}

	return func(request context.Context, port int) (net.Conn, error) {
		mu.Lock()
		if err := ctx.Err(); err != nil {
			mu.Unlock()
			return nil, err
		}
		if err := request.Err(); err != nil {
			mu.Unlock()
			return nil, err
		}
		if appTunnelCertificateDue(current, time.Now()) {
			if pending == nil {
				pending = &appTunnelRenewal{done: make(chan struct{})}
				go renew(pending)
			}
			attempt := pending
			mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-request.Done():
				return nil, request.Err()
			case <-attempt.done:
				if attempt.err != nil {
					return nil, attempt.err
				}
			}
			mu.Lock()
		}
		dial := current.RegistryDialer
		mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := request.Err(); err != nil {
			return nil, err
		}
		return dial(request, port)
	}
}

func appTunnelCertificateDue(conn *grpcclient.AgentConnection, now time.Time) bool {
	if conn.CertInfo == nil {
		return false
	}
	cert, err := certs.ParseStoredLeaf(conn.CertInfo.PemCertificate)
	if err != nil {
		return false
	}
	return !now.Before(cert.NotAfter.Add(-time.Minute))
}
