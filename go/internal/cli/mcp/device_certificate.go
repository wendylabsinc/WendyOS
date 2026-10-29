package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc/connectivity"
)

// deviceCertificateCheckInterval is how often the server looks for a device
// connection that can no longer reconnect because its certificate expired.
const deviceCertificateCheckInterval = 30 * time.Second

// runDeviceCertificateWatch replaces a device connection whose operator
// certificate has expired once it has lost its transport.
//
// A connection keeps the certificate it was dialled with. gRPC redials with
// that same TLS configuration after any transport loss, such as a device
// reboot, backing off to one attempt every one to two minutes, and the
// container-MCP scan's RPC every few seconds keeps the channel from idling out
// of that loop. After the certificate expires, the device refuses every redial,
// and a login renewed on disk is never picked up: on 2026-10-28 a long-running
// server presented an expired certificate to mesh-1 every 1-2 minutes for more
// than half an hour after the user had renewed it.
func (s *mcpServer) runDeviceCertificateWatch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.replaceExpiredDeviceConnection(ctx, time.Now())
		}
	}
}

// replaceExpiredDeviceConnection reconnects to the same target when the active
// connection's certificate has expired and its channel is not Ready. A Ready
// connection is left alone: the device accepted that certificate while it was
// valid, and the established session keeps working. A connection that is not
// Ready can only redial with the expired certificate. The new connection reads
// the stored login again. If that fails, the old connection is closed so it
// stops presenting the expired certificate, and tools report that no device is
// connected.
func (s *mcpServer) replaceExpiredDeviceConnection(ctx context.Context, now time.Time) {
	s.mu.RLock()
	conn, connType, target, revision := s.conn, s.connType, s.commandTarget, s.connRevision
	s.mu.RUnlock()
	notAfter, expired := expiredDisconnectedCertificate(conn, now)
	if !expired {
		return
	}
	var next *grpcclient.AgentConnection
	var err error
	switch {
	case conn.Reconnect != nil:
		next, err = conn.Reconnect(ctx)
	case s.connectFn != nil && target.Device != "":
		next, err = s.connectFn(ctx, target.Device)
	default:
		err = errors.New("the connection has no target to reconnect to")
	}
	if err == nil && next == nil {
		err = errors.New("reconnect returned no connection")
	}
	if err != nil && next != nil {
		// A failed reconnect that still handed back a connection must not
		// leak it.
		_ = next.Close()
		next = nil
	}

	s.mu.Lock()
	if s.conn != conn || s.connRevision != revision {
		// A tool changed the connection meanwhile; its choice stands.
		s.mu.Unlock()
		if next != nil {
			_ = next.Close()
		}
		return
	}
	if err != nil {
		s.setConnectionLocked(nil, "", commandTarget{})
	} else {
		s.setConnectionLocked(next, connType, target)
	}
	s.mu.Unlock()
	if err != nil {
		s.recordProxyDiag("", "device-certificate", fmt.Errorf("disconnected: the operator certificate for this connection expired at %s and reconnecting failed: %w", notAfter.UTC().Format(time.RFC3339), err))
	}
}

// expiredDisconnectedCertificate reports the NotAfter of a direct mTLS
// connection's certificate when it has expired and the channel is not Ready.
func expiredDisconnectedCertificate(conn *grpcclient.AgentConnection, now time.Time) (time.Time, bool) {
	if conn == nil || conn.Conn == nil || !conn.IsMTLS || conn.IsSessionProxy || conn.CertInfo == nil {
		return time.Time{}, false
	}
	leaves, _ := certs.ParseCertsFromPEM([]byte(conn.CertInfo.PemCertificate))
	if len(leaves) == 0 || !now.After(leaves[0].NotAfter) {
		return time.Time{}, false
	}
	if conn.Conn.GetState() == connectivity.Ready {
		return time.Time{}, false
	}
	return leaves[0].NotAfter, true
}
