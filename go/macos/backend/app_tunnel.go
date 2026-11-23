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

// App forwarders can outlive the certificate in their original connection.
// Re-enter the existing authenticated connect path before expiry, only for the
// bundled client's app forwards. The CLI's tunnel behavior stays unchanged.
func renewingAppTunnel(ctx context.Context, selector string, initial *grpcclient.AgentConnection, connect func(context.Context, string) (*grpcclient.AgentConnection, error)) func(context.Context, int) (net.Conn, error) {
	var mu sync.Mutex
	current := initial
	var owned []*grpcclient.AgentConnection
	context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range owned {
			c.Close()
		}
		owned = nil
	})
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
			// ctx retains the original account generation and forward lifetime.
			next, err := connect(ctx, selector)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			if next == nil || next.RegistryDialer == nil {
				if next != nil {
					next.Close()
				}
				mu.Unlock()
				return nil, errors.New("device has no TCP tunnel transport")
			}
			current = next
			owned = append(owned, next)
		}
		dial := current.RegistryDialer
		mu.Unlock()
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
