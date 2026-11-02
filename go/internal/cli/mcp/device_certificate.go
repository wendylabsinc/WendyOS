package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/connectivity"
)

// deviceCertificateCheckInterval is how often the server looks for a device
// connection that can no longer reconnect because its certificate expired.
const deviceCertificateCheckInterval = 30 * time.Second

// errReconnectNotMTLS is a reconnect that produced only a plaintext
// connection. An unpinned dial falls back to a lazy plaintext connection when
// nothing answers on the mTLS port, so for a device this server reached over
// mTLS it means the device has not come back yet.
var errReconnectNotMTLS = errors.New("the device did not answer over mTLS")

// SetTransientConnectError tells the device certificate watch which reconnect
// failures can clear without anyone acting, such as a device that is still
// rebooting. The watch keeps such a connection and its target and tries again
// at the next check. Every other failure, and every failure while fn is nil,
// ends the connection.
func (s *mcpServer) SetTransientConnectError(fn func(error) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transientConnectErrFn = fn
}

// SetLoginRenewer gives the device certificate watch a way to renew the stored
// login that issued an expired connection certificate before it reconnects to
// a direct (LAN, USB) target. renew must do nothing when the stored login is
// already current, and must renew only that login, never the current context.
func (s *mcpServer) SetLoginRenewer(renew func(context.Context, config.CertificateInfo) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewLoginFn = renew
}

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
// Ready can only redial with the expired certificate.
//
// The reconnect dials the target's address or cloud:// selector through
// connectFn, as device_connect does, so it reads the stored logins again; for
// a cloud selector, connectFn also renews an expired OAuth login. Before a
// direct target's reconnect, the login that issued the expired certificate is
// renewed once per connection (SetLoginRenewer). conn.Reconnect is the
// fallback for a target connectFn cannot dial: it reuses the login the
// connection captured, which is the expired one.
//
// A simulator is left alone: connecting to vm:NAME boots it, and the watch
// must not start a simulator that was stopped outside this server.
//
// When nothing answered the reconnect (see SetTransientConnectError), the
// connection and its target stay and the next check tries again: losing the
// transport usually means the device is rebooting. Any other failure means the
// device refused the stored login, or there is none to use. The old connection
// is then closed, so it stops presenting the expired certificate, and tools
// report that no device is connected.
func (s *mcpServer) replaceExpiredDeviceConnection(ctx context.Context, now time.Time) {
	s.mu.RLock()
	conn, connType, target, revision := s.conn, s.connType, s.commandTarget, s.connRevision
	transient := s.transientConnectErrFn
	s.mu.RUnlock()
	notAfter, expired := expiredDisconnectedCertificate(conn, now)
	if !expired || isSimulatorConnection(conn, target) {
		return
	}
	s.renewConnectionLogin(ctx, conn, target)
	next, err := s.reconnectExpiredDevice(ctx, conn, target)
	if err == nil && next == nil {
		err = errors.New("reconnect returned no connection")
	}
	if err == nil && !next.IsMTLS {
		err = errReconnectNotMTLS
	}
	if err != nil && next != nil {
		// A failed reconnect that still handed back a connection must not
		// leak it.
		_ = next.Close()
		next = nil
	}
	retry := err != nil && (errors.Is(err, errReconnectNotMTLS) || (transient != nil && transient(err)))

	s.mu.Lock()
	if s.conn != conn || s.connRevision != revision {
		// A tool changed the connection meanwhile; its choice stands.
		s.mu.Unlock()
		if next != nil {
			_ = next.Close()
		}
		return
	}
	noted := s.certWatchRetrying == conn
	switch {
	case err == nil:
		s.certWatchRetrying = nil
		s.setConnectionLocked(next, connType, target)
	case retry:
		s.certWatchRetrying = conn
	default:
		s.certWatchRetrying = nil
		s.setConnectionLocked(nil, "", commandTarget{})
	}
	s.mu.Unlock()
	expiredAt := notAfter.UTC().Format(time.RFC3339)
	switch {
	case err == nil:
	case retry:
		// Once per connection: the check repeats every interval.
		if !noted {
			s.recordProxyDiag("", "device-certificate", fmt.Errorf("the operator certificate for this connection expired at %s and the device did not answer a reconnect; trying again at each check: %w", expiredAt, err))
		}
	default:
		s.recordProxyDiag("", "device-certificate", fmt.Errorf("disconnected: the operator certificate for this connection expired at %s and reconnecting failed: %w", expiredAt, err))
	}
}

// renewConnectionLogin renews the login that issued conn's expired certificate
// before a reconnect to a direct target, once per connection: the watch checks
// every interval while the device is down, and each renewal spends the
// session's rotating refresh token. A cloud selector's dial renews its own
// login, and conn.Reconnect reuses the captured one, so neither gets it.
func (s *mcpServer) renewConnectionLogin(ctx context.Context, conn *grpcclient.AgentConnection, target commandTarget) {
	s.mu.Lock()
	renew := s.renewLoginFn
	first := s.certWatchRenewed != conn
	s.certWatchRenewed = conn
	s.mu.Unlock()
	if renew == nil || !first || s.connectFn == nil || target.Transport == "cloud" || watchReconnectAddress(target) == "" {
		return
	}
	if err := renew(ctx, *conn.CertInfo); err != nil && ctx.Err() == nil {
		s.recordProxyDiag("", "device-certificate", fmt.Errorf("could not renew the stored login for this connection's expired certificate: %w", err))
	}
}

// reconnectExpiredDevice dials target again for the certificate watch.
func (s *mcpServer) reconnectExpiredDevice(ctx context.Context, conn *grpcclient.AgentConnection, target commandTarget) (*grpcclient.AgentConnection, error) {
	if address := watchReconnectAddress(target); s.connectFn != nil && address != "" {
		return s.connectFn(ctx, address)
	}
	if conn.Reconnect != nil {
		return conn.Reconnect(ctx)
	}
	return nil, errors.New("the connection has no target to reconnect to")
}

// watchReconnectAddress is the address connectFn reconnects target with, or ""
// when connectFn cannot dial it: a cloud target recorded by device name
// rather than by its cloud:// selector.
func watchReconnectAddress(target commandTarget) string {
	if target.Transport == "cloud" && !strings.HasPrefix(strings.ToLower(target.Device), "cloud:") {
		return ""
	}
	return target.Device
}

// isSimulatorConnection reports a connection to a local simulator, which a
// connect to its vm:NAME target would boot.
func isSimulatorConnection(conn *grpcclient.AgentConnection, target commandTarget) bool {
	return conn.SimulatorName != "" || strings.HasPrefix(target.Device, "vm:")
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
