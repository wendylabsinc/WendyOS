package mesh

import (
	"context"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

// ProxyPort is the host TCP port the WENDY-MESH nat REDIRECT rule points at.
// 50051 is the agent's plaintext port and 50052 its mTLS port; 50058 is
// otherwise unused by the agent.
const ProxyPort = 50058

// PeerDialer opens a byte stream to a port on another mesh device. The
// context bounds dialing only; the returned conn outlives it. It also
// returns the mode it dialed with ("lan-direct" or "relay") so Proxy can
// label its own logs without re-deriving the choice the dialer already made;
// mode is "" on error.
type PeerDialer interface {
	DialDevice(ctx context.Context, device meshid.Device, port uint16) (conn net.Conn, mode string, err error)
}

// ConnMetrics records mesh connection byte totals. Dial-outcome metrics
// (mesh.connections, mesh.dial.duration_ms) are recorded by the PeerDialer
// implementation itself, since it is the authority on mode/result/timing;
// Proxy only ever sees the bytes moved once a peer conn is established.
// Defined here (rather than depending on services.MeshMetrics's concrete
// type) so this package doesn't import services, which already imports mesh.
type ConnMetrics interface {
	RecordBytes(device meshid.Device, dir string, n int64)
}

// Proxy terminates REDIRECTed mesh VIP connections, recovers where the
// container was actually connecting, and splices the connection onto a
// PeerDialer stream toward that device.
type Proxy struct {
	logger      *zap.Logger
	dialer      PeerDialer
	metrics     ConnMetrics
	vips        *VIPTable                                  // resolves original destinations back to devices
	origDst     func(*net.TCPConn) (netip.AddrPort, error) // swapped in tests
	dialTimeout time.Duration
	ln          net.Listener
}

// NewProxy builds a Proxy. metrics may be nil (e.g. in tests); RecordBytes
// calls are skipped when it is. vips is the table every REDIRECTed
// destination is resolved through; without one the proxy refuses every
// connection, since a VIP it cannot map to a principal has no peer to dial.
func NewProxy(logger *zap.Logger, dialer PeerDialer, metrics ConnMetrics, vips *VIPTable) *Proxy {
	return &Proxy{
		logger:  logger,
		dialer:  dialer,
		metrics: metrics,
		vips:    vips,
		origDst: originalDst,
		// Cover the full end-to-end TLS and app-port setup budget over BLE.
		dialTimeout: meshsession.TLSAppSessionSetupTimeout,
	}
}

func (p *Proxy) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	p.ln = ln
	go p.acceptLoop()
	return nil
}

func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

func (p *Proxy) Close() error {
	if p.ln != nil {
		return p.ln.Close()
	}
	return nil
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleConn(conn)
	}
}

func (p *Proxy) handleConn(conn net.Conn) {
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	dst, err := p.origDst(tcp)
	if err != nil {
		p.logger.Warn("wendy net proxy: original destination unavailable", zap.Error(err))
		return
	}
	if p.vips == nil {
		p.logger.Warn("wendy net proxy: no VIP table configured", zap.String("dst", dst.String()))
		return
	}
	device, err := p.vips.Device(dst.Addr())
	if err != nil {
		p.logger.Warn("wendy net proxy: destination is not a wendy net VIP", zap.String("dst", dst.String()), zap.Error(err))
		return
	}
	peerID := device.UUID().String()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout)
	peer, mode, err := p.dialer.DialDevice(ctx, device, dst.Port())
	cancel()
	if err != nil {
		// The dialer already recorded the mesh.connections/dial.duration_ms
		// error metric for this attempt; this log is the per-connection
		// signal (dashboard filters on the wendy.mesh scope).
		p.logger.Warn("wendy net connection failed",
			zap.String("mesh.peer", peerID),
			zap.String("mesh.mode", mode),
			zap.Uint32("mesh.port", uint32(dst.Port())),
			zap.String("mesh.result", "error"),
			zap.Error(err))
		return
	}
	defer peer.Close()
	tx, rx := relayBytes(conn, peer)
	if p.metrics != nil {
		p.metrics.RecordBytes(device, "tx", tx)
		p.metrics.RecordBytes(device, "rx", rx)
	}
	p.logger.Info("wendy net connection",
		zap.String("mesh.peer", peerID),
		zap.String("mesh.mode", mode),
		zap.Uint32("mesh.port", uint32(dst.Port())),
		zap.String("mesh.result", "ok"),
		zap.Int64("mesh.bytes_tx", tx),
		zap.Int64("mesh.bytes_rx", rx),
		zap.Int64("mesh.duration_ms", time.Since(start).Milliseconds()))
}

// relayBytes splices two connections until both directions finish,
// propagating half-closes where the transport supports them, and returns the
// byte counts copied in each direction: tx is a (client) -> b (peer), rx is
// b (peer) -> a (client).
//
// When one direction's copy finishes, dst must always receive some signal
// that no more data is coming - otherwise a peer waiting for our EOF before
// finishing its own side can leave the opposite io.Copy blocked forever,
// leaking the goroutine and its file descriptor. PeerDialer connections are
// expected to implement CloseWrite for a clean half-close; conns without it
// (e.g. a net.Pipe-backed tunnel conn) are fully closed instead once the
// opposite direction finishes. A full close can truncate any in-flight data
// still travelling the other way, but that tradeoff is accepted here since
// it guarantees the relay never leaks.
func relayBytes(a, b net.Conn) (txBytes, rxBytes int64) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn, n *int64) {
		copied, _ := io.Copy(dst, src)
		*n = copied
		type closeWriter interface{ CloseWrite() error }
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(b, a, &txBytes) // a (client) -> b (peer): tx
	go cp(a, b, &rxBytes) // b (peer) -> a (client): rx
	<-done
	<-done
	return txBytes, rxBytes
}
