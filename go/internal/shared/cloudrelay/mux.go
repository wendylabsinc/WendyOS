package cloudrelay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

// TCPMuxFeature is advertised by agents that carry many TCP connections to
// one authorized port in a single relay session (OpenTCPMux).
const TCPMuxFeature = "cloud-relay-tcp-mux"

// muxPreamble is a multiplexed session's first caller frame. The agent then
// runs the endpoint mTLS handshake and serves each HTTP/2 stream as one
// connection to the session's authorized port. Any other first frame is the
// start of one raw connection, as before.
var muxPreamble = []byte("wendy-tcp-mux/1\n")

// muxWait is how long a mux-capable agent waits for the caller's first frame
// before it dials the port for one raw connection.
// ponytail: a caller without mux whose protocol has the server speak first
// waits this long once per connection; it shortens once such callers are gone.
const muxWait = 2 * time.Second

// OpenTCPMux joins one relay session to a TCP service for a multiplexed
// forward. It returns the session's byte stream, on which the caller runs the
// endpoint mTLS handshake and then NewMuxClient, and when the relay ends.
func OpenTCPMux(ctx, authCtx context.Context, cloudConn *grpc.ClientConn, verifier *Verifier, assetID, service string, signRequest func([]byte) ([]byte, error)) (net.Conn, time.Time, error) {
	s, err := openPortSession(ctx, authCtx, cloudConn, verifier, assetID, service, pb.DialTransport_DIAL_TRANSPORT_TCP, signRequest)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err := s.Send(muxPreamble, false); err != nil {
		_ = s.Close()
		return nil, time.Time{}, err
	}
	expires, _ := s.ctx.Deadline()
	return s.Conn(), expires, nil
}

// MuxClient opens connections inside one multiplexed relay session.
type MuxClient struct {
	cc      *http2.ClientConn
	expires time.Time
}

// NewMuxClient starts HTTP/2 over conn, the secured stream of OpenTCPMux.
func NewMuxClient(conn net.Conn, expires time.Time) (*MuxClient, error) {
	cc, err := new(http2.Transport).NewClientConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &MuxClient{cc: cc, expires: expires}, nil
}

// Usable reports whether a new connection may start in this session: it is
// open and its relay has more than margin left.
func (m *MuxClient) Usable(margin time.Duration) bool {
	return m.cc.CanTakeNewRequest() && time.Until(m.expires) > margin
}

func (m *MuxClient) Close() error { return m.cc.Close() }

// Dial opens one connection to the session's port. It ends with ctx. A device
// that cannot reach its port ends the connection without a byte, as a raw
// session does.
func (m *MuxClient) Dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	body, toDevice := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://wendy-forward/", body)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := m.cc.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &muxConn{ReadCloser: resp.Body, toDevice: toDevice, cancel: cancel}, nil
}

// muxConn is one stream. CloseWrite ends the request body: the device sees
// EOF and the response keeps flowing.
type muxConn struct {
	io.ReadCloser
	toDevice *io.PipeWriter
	cancel   context.CancelFunc
}

func (c *muxConn) Write(p []byte) (int, error) { return c.toDevice.Write(p) }
func (c *muxConn) CloseWrite() error           { return c.toDevice.Close() }
func (c *muxConn) Close() error {
	c.cancel()
	_ = c.toDevice.Close()
	return c.ReadCloser.Close()
}
func (c *muxConn) LocalAddr() net.Addr              { return muxAddr{} }
func (c *muxConn) RemoteAddr() net.Addr             { return muxAddr{} }
func (c *muxConn) SetDeadline(time.Time) error      { return errors.ErrUnsupported }
func (c *muxConn) SetReadDeadline(time.Time) error  { return errors.ErrUnsupported }
func (c *muxConn) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }

type muxAddr struct{}

func (muxAddr) Network() string { return "wendy-relay" }
func (muxAddr) String() string  { return "wendy-relay" }

// serveMux runs the endpoint mTLS handshake on a multiplexed session and
// serves each stream as one connection to the authorized port. The session's
// grant authorized that port; the handshake adds the operator check of the
// agent's management TLS.
func (a *Agent) serveMux(session *Session, c claims, d *pb.DialInstruction) error {
	cfg, err := a.ForwardTLS()
	if err != nil {
		return err
	}
	conn := tls.Server(session.Conn(), cfg)
	defer conn.Close()
	handshakeCtx, cancel := context.WithTimeout(session.ctx, 30*time.Second)
	err = conn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("forward mTLS handshake: %w", err)
	}
	host, port := d.Host, strconv.Itoa(int(d.Port))
	log := a.Logger.With(zap.String("session_id", c.Session), zap.String("service", string(d.ServiceDescriptor)))
	// One session carries at most as many connections as the agent has
	// relay sessions, the bound a caller had with a tunnel per connection.
	(&http2.Server{MaxConcurrentStreams: uint32(cap(a.slots))}).ServeConn(conn, &http2.ServeConnOpts{Context: session.ctx, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardStream(w, r, host, port, log)
	})})
	return nil
}

// forwardStream relays one stream to a new local connection and records its
// outcome in the agent log, the per-connection record of a multiplexed forward.
func forwardStream(w http.ResponseWriter, r *http.Request, host, port string, log *zap.Logger) {
	local, err := dialLocalService(r.Context(), host, port, false)
	if err != nil {
		log.Info("cloud relay forwarded connection", zap.Error(err))
		return
	}
	defer local.Close()
	stop := context.AfterFunc(r.Context(), func() { _ = local.Close() })
	defer stop()
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		log.Info("cloud relay forwarded connection", zap.Error(err))
		return
	}
	go func() {
		_, _ = io.Copy(local, r.Body)
		if tcp, ok := local.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	n, err := io.Copy(flushWriter{w, rc}, local)
	log.Info("cloud relay forwarded connection", zap.Int64("bytes_to_caller", n), zap.Error(err))
}

type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}
