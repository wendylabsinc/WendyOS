package bleprovider

import (
	"crypto/tls"
	"net"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// handshakeMeter counts encrypted socket I/O without changing stream framing,
// deadlines or buffering. The original conn remains the caller's ACL/close owner.
// Read-ahead and session tickets can fall inside this window; these are not
// Certificate-only bytes, peer-delivery counters or HCI/CoC overhead.
type handshakeMeter struct {
	net.Conn
	read, written atomic.Uint64
	started       time.Time
}

type handshakeMeasurement struct {
	started, completed time.Time
	read, written      uint64
}

func meterHandshake(conn net.Conn, logger *zap.Logger) (net.Conn, *handshakeMeter) {
	if logger == nil || !logger.Core().Enabled(zap.DebugLevel) {
		return conn, nil
	}
	meter := &handshakeMeter{Conn: conn}
	return meter, meter
}
func (m *handshakeMeter) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	if n > 0 {
		m.read.Add(uint64(n))
	}
	return n, err
}
func (m *handshakeMeter) Write(p []byte) (int, error) {
	n, err := m.Conn.Write(p)
	if n > 0 {
		m.written.Add(uint64(n))
	}
	return n, err
}
func (m *handshakeMeter) start() {
	if m != nil {
		m.started = time.Now()
	}
}
func (m *handshakeMeter) finish() *handshakeMeasurement {
	if m == nil {
		return nil
	}
	completed := time.Now()
	return &handshakeMeasurement{started: m.started, completed: completed, read: m.read.Load(), written: m.written.Load()}
}
func (m *handshakeMeasurement) log(logger *zap.Logger, direction string, peer int32, state tls.ConnectionState, identityAccepted bool) {
	if m == nil {
		return
	}
	logger.Debug("BLE TLS handshake measured", zap.String("direction", direction), zap.Int32("peer", peer),
		zap.Int64("started_unix_ns", m.started.UnixNano()), zap.Int64("completed_unix_ns", m.completed.UnixNano()),
		zap.Duration("elapsed", m.completed.Sub(m.started)), zap.Bool("tls_complete", state.HandshakeComplete),
		zap.Bool("identity_accepted", identityAccepted), zap.Bool("session_resumed", state.DidResume),
		zap.Uint64("socket_read_bytes", m.read), zap.Uint64("socket_write_bytes", m.written),
		zap.String("byte_scope", "encrypted socket I/O during HandshakeContext; may include read-ahead/tickets; excludes CoC/HCI framing"))
}
