package meshsession

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// A flow is named by its reliable control stream's ID. QUIC never reuses a
// stream ID in a connection, so late datagrams cannot enter a replacement flow.
// The flow prefix is outside udpmessage's existing fragment envelope.
const udpMuxHeaderSize = 8
const maxMuxUDPChunk = MaxUDPTransportChunk - udpMuxHeaderSize

// udpMultiplexer is the sole DATAGRAM reader for an authenticated v2 session.
// Complete messages remain charged to udpmessage's flow, peer and node budgets.
// Its dispatch never waits for an application's receiver.
type udpMultiplexer struct {
	conn   *quic.Conn
	mu     sync.Mutex
	flows  map[uint64]*muxUDPFlow
	closed bool
}

type muxUDPFlow struct {
	mux          *udpMultiplexer
	id           uint64
	control      *quic.Stream
	endpoint     *udpmessage.Endpoint
	ownsManager  bool
	release      func()
	messages     chan *udpmessage.Message
	done         chan struct{}
	mu           sync.Mutex
	closed       bool
	readMu       udpReceiveLock
	lastReceived atomic.Int64
}

func newUDPMultiplexer(conn *quic.Conn) *udpMultiplexer {
	return &udpMultiplexer{conn: conn, flows: make(map[uint64]*muxUDPFlow)}
}

func (m *udpMultiplexer) register(stream *quic.Stream, endpoint *udpmessage.Endpoint, owns bool, release func()) (*muxUDPFlow, error) {
	f := &muxUDPFlow{mux: m, id: uint64(stream.StreamID()), control: stream, endpoint: endpoint, ownsManager: owns, release: release, messages: make(chan *udpmessage.Message, udpmessage.FlowRXMessages), done: make(chan struct{})}
	f.lastReceived.Store(time.Now().UnixNano())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, net.ErrClosed
	}
	if len(m.flows) >= udpmessage.PeerFlows {
		return nil, udpmessage.ErrCapacity
	}
	if _, exists := m.flows[f.id]; exists {
		return nil, udpmessage.ErrMalformed
	}
	m.flows[f.id] = f
	return f, nil
}

func (m *udpMultiplexer) receive() {
	defer func() {
		m.mu.Lock()
		m.closed = true
		flows := make([]*muxUDPFlow, 0, len(m.flows))
		for _, f := range m.flows {
			flows = append(flows, f)
		}
		m.mu.Unlock()
		for _, f := range flows {
			f.close()
		}
	}()
	for {
		wire, err := m.conn.ReceiveDatagram(m.conn.Context())
		if err != nil {
			return
		}
		if len(wire) < udpMuxHeaderSize+udpmessage.HeaderSize || len(wire) > MaxUDPTransportChunk {
			continue
		}
		id := binary.BigEndian.Uint64(wire[:udpMuxHeaderSize])
		m.mu.Lock()
		f := m.flows[id]
		m.mu.Unlock()
		if f == nil {
			continue
		}
		f.lastReceived.Store(time.Now().UnixNano())
		message, err := f.endpoint.ReceiveFrame(wire[udpMuxHeaderSize:])
		if err != nil {
			f.close()
			continue
		}
		if message != nil {
			f.deliver(message)
		}
	}
}

func (f *muxUDPFlow) deliver(message *udpmessage.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		select {
		case f.messages <- message:
			return
		default:
		}
	}
	// Datagram loss is preferable to blocking every other flow on the session.
	message.Release()
}

func (f *muxUDPFlow) receive(ctx context.Context) (*udpmessage.Message, error) {
	if err := f.readMu.lock(ctx, f.done); err != nil {
		return nil, localUDPError(err)
	}
	defer f.readMu.unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-f.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return nil, net.ErrClosed
	case message := <-f.messages:
		return message, nil
	}
}

func (f *muxUDPFlow) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.done)
	for {
		select {
		case message := <-f.messages:
			message.Release()
		default:
			goto drained
		}
	}
drained:
	f.mu.Unlock()
	f.mux.mu.Lock()
	delete(f.mux.flows, f.id)
	f.mux.mu.Unlock()
	f.control.CancelRead(0)
	_ = f.control.Close()
	f.endpoint.Close()
	if f.ownsManager {
		f.endpoint.Manager().Close()
	}
	if f.release != nil {
		f.release()
	}
}

// A UDP control stream stays open until the flow ends. FIN/reset, credentials,
// route retirement and app revocation all retire only this flow's resources.
func (f *muxUDPFlow) watchControl() {
	var b [1]byte
	_, _ = f.control.Read(b[:])
	f.close()
}

func (f *muxUDPFlow) watchLifetime(parent context.Context, sourceDone <-chan struct{}, idle bool) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-parent.Done():
			f.close()
			return
		case <-sourceDone:
			f.close()
			return
		case <-f.endpoint.Done():
			f.close()
			return
		case <-f.done:
			return
		case <-tick.C:
			if idle && time.Since(time.Unix(0, f.lastReceived.Load())) >= udpIdleTimeout {
				f.close()
				return
			}
		}
	}
}

func multiplexUDPAdmission(conn *quic.Conn, id uint64) func([]byte) error {
	return multiplexUDPAdmissionWithSender(conn.TrySendDatagram, id)
}

func multiplexUDPAdmissionWithSender(send func([]byte) error, id uint64) func([]byte) error {
	return func(fragment []byte) error {
		wire := make([]byte, udpMuxHeaderSize+len(fragment))
		binary.BigEndian.PutUint64(wire, id)
		copy(wire[udpMuxHeaderSize:], fragment)
		err := send(wire)
		if errors.Is(err, quic.ErrDatagramQueueFull) {
			return udpmessage.ErrQueueFull
		}
		var large *quic.DatagramTooLargeError
		if errors.As(err, &large) {
			// The manager sizes its fragment including its own 20-byte header. QUIC's
			// capacity covers our flow prefix too, so subtract it before retrying.
			return &udpmessage.ChunkTooLarge{Maximum: int(large.MaxDatagramPayloadSize) - udpMuxHeaderSize}
		}
		return err
	}
}

func muxUDPRequest(port uint16) []byte {
	b := udpOpenRequest(port)
	copy(b, "WAU4")
	binary.BigEndian.PutUint16(b[12:14], maxMuxUDPChunk)
	return b
}
func writeMuxUDPAck(stream *quic.Stream, status byte, maximum int) error {
	b := make([]byte, 16)
	copy(b, "WUA4")
	b[4], b[5] = status, 4
	binary.BigEndian.PutUint16(b[6:8], uint16(maximum))
	binary.BigEndian.PutUint32(b[8:12], MaxUDPMessageSize)
	_, err := stream.Write(b)
	return err
}

func requestMuxUDP(ctx context.Context, conn *quic.Conn, port uint16) (*quic.Stream, int, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, 0, err
	}
	success := false
	defer func() {
		if !success {
			stream.CancelRead(1)
			stream.CancelWrite(1)
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()); close(finished) })
	var joined sync.Once
	join := func() {
		joined.Do(func() {
			if !stop() {
				<-finished
			}
		})
	}
	defer join()
	if _, err = stream.Write(muxUDPRequest(port)); err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, err
	}
	var ack [16]byte
	if _, err = io.ReadFull(stream, ack[:]); err != nil {
		var refusal *quic.StreamError
		if ctx.Err() != nil && !errors.As(err, &refusal) {
			err = ctx.Err()
		}
		return nil, 0, &portACKError{err}
	}
	maximum := int(binary.BigEndian.Uint16(ack[6:8]))
	if string(ack[:4]) != "WUA4" || ack[5] != 4 || maximum < udpmessage.MinChunkSize || maximum > maxMuxUDPChunk || binary.BigEndian.Uint32(ack[8:12]) != MaxUDPMessageSize || binary.BigEndian.Uint32(ack[12:]) != 0 {
		return nil, 0, ErrUDPUnsupported
	}
	if ack[4] != 1 {
		if ack[4] == 0 {
			return nil, 0, ErrDenied
		}
		if ack[4] == 3 {
			return nil, 0, udpmessage.ErrCapacity
		}
		return nil, 0, ErrUDPUnsupported
	}
	join()
	if err = ctx.Err(); err != nil {
		return nil, 0, err
	}
	_ = stream.SetDeadline(time.Time{})
	success = true
	return stream, maximum, nil
}

func (c *Client) dialMultiplexedUDP(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, opts UDPOptions) (*DatagramFlow, error) {
	if port == 0 || !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("invalid multiplexed UDP dial")
	}
	entry, reused, err := c.acquire(ctx, peer, address)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			c.release(entry)
		}
	}()
	if entry.mux == nil || !udpDatagramsSupported(entry.conn) {
		return nil, ErrUDPUnsupported
	}
	requestCtx := ctx
	if reused {
		limit := c.portACKLimit
		if limit <= 0 {
			limit = pooledPortACKLimit
		}
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	stream, maximum, err := requestMuxUDP(requestCtx, entry.conn, port)
	if err != nil {
		// Only an unanswered request can make a live-looking connection
		// suspect. Stream-credit exhaustion and per-flow refusal preserve
		// its siblings. The next dial must repeat its caller's route check.
		if stalePortACK(err) && ctx.Err() == nil {
			c.retire(peer, entry)
		}
		return nil, err
	}
	endpoint, manager := opts.Endpoint, opts.Manager
	if endpoint != nil {
		if manager != nil && manager != endpoint.Manager() {
			stream.CancelRead(1)
			stream.CancelWrite(1)
			return nil, udpmessage.ErrMalformed
		}
		manager = endpoint.Manager()
	}
	owns := false
	if manager == nil {
		manager = udpmessage.NewManager()
		owns = true
	}
	fail := func(err error) (*DatagramFlow, error) {
		stream.CancelRead(1)
		stream.CancelWrite(1)
		if endpoint != nil {
			endpoint.Close()
		}
		if owns {
			manager.Close()
		}
		return nil, err
	}
	if endpoint == nil {
		guard := opts.Guard
		if guard == nil {
			guard = func(send func() error) error {
				if entry.conn.Context().Err() != nil {
					return net.ErrClosed
				}
				return send()
			}
		}
		endpoint, err = manager.Open(peer, guard)
		if err != nil {
			return fail(err)
		}
	}
	if err = endpoint.SetTransport(maximum, false, multiplexUDPAdmission(entry.conn, uint64(stream.StreamID()))); err != nil {
		return fail(err)
	}
	shared, err := entry.mux.register(stream, endpoint, owns, func() { c.release(entry) })
	if err != nil {
		return fail(err)
	}
	transferred = true
	flow := &DatagramFlow{conn: entry.conn, endpoint: endpoint, shared: shared}
	go shared.watchControl()
	go shared.watchLifetime(entry.conn.Context(), opts.Done, false)
	return flow, nil
}

func (s *Server) serveMultiplexedDatagrams(parent context.Context, conn *quic.Conn, port uint16, stream *quic.Stream, mux *udpMultiplexer) {
	var rest [10]byte
	if _, err := io.ReadFull(stream, rest[:]); err != nil {
		return
	}
	maximum := min(maxMuxUDPChunk, int(binary.BigEndian.Uint16(rest[6:8])))
	ack := func(status byte) { _ = writeMuxUDPAck(stream, status, maximum) }
	if binary.BigEndian.Uint16(rest[:2]) != 0 || binary.BigEndian.Uint32(rest[2:6]) != MaxUDPMessageSize || maximum < udpmessage.MinChunkSize || binary.BigEndian.Uint16(rest[8:]) != 0 || !udpDatagramsSupported(conn) {
		ack(2)
		return
	}
	auth, ok := s.authorizer.(UDPAuthorizer)
	if !ok || port == 0 {
		ack(0)
		return
	}
	token := auth.UDPToken(port)
	guard := udpmessage.Guard(func(send func() error) error {
		return auth.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send() })
	})
	var chain [][]byte
	for _, cert := range conn.ConnectionState().TLS.PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	identity, err := s.credentials.VerifyTLS(chain, time.Now())
	if err != nil {
		return
	}
	endpoint, err := s.udpManager.Open(identity.Device, guard)
	if err != nil {
		ack(3)
		return
	}
	defer endpoint.Close()
	if endpoint.ReserveSocketBytes(512<<10) != nil {
		ack(3)
		return
	}
	var local *net.UDPConn
	err = auth.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		var err error
		local, err = net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(address))
		return err
	})
	if err != nil {
		s.udpManager.RecordDenied()
		ack(0)
		return
	}
	defer local.Close()
	if configureLocalUDP(local) != nil {
		ack(3)
		return
	}
	socketBytes, err := localUDPBuffers(local)
	if err != nil || endpoint.ReserveSocketBytes(socketBytes) != nil {
		ack(3)
		return
	}
	if err = endpoint.SetTransport(maximum, false, multiplexUDPAdmission(conn, uint64(stream.StreamID()))); err != nil {
		ack(3)
		return
	}
	flow, err := mux.register(stream, endpoint, false, nil)
	if err != nil {
		ack(3)
		return
	}
	defer flow.close()
	if err = writeMuxUDPAck(stream, 1, maximum); err != nil {
		return
	}
	_ = stream.SetDeadline(time.Time{})
	go flow.watchControl()
	go flow.watchLifetime(parent, auth.UDPDone(port, token), true)
	go func() { <-flow.done; _ = local.Close() }()
	replyDone := make(chan struct{})
	go func() {
		defer close(replyDone)
		defer flow.close()
		buf := make([]byte, MaxUDPMessageSize+1)
		for {
			n, _, flags, _, err := local.ReadMsgUDP(buf, nil)
			if err != nil {
				return
			}
			if n > MaxUDPMessageSize || udpReadTruncated(flags) {
				continue
			}
			if sendErr := endpoint.Send(buf[:n]); sendErr != nil && !errors.Is(sendErr, udpmessage.ErrCapacity) {
				return
			}
		}
	}()
	defer func() { _ = local.Close(); <-replyDone }()
	for {
		message, err := flow.receive(parent)
		if err != nil {
			return
		}
		deliverErr := endpoint.Deliver(func() error { return writeLocalUDP(local, message.Payload) })
		message.Release()
		if deliverErr != nil {
			return
		}
	}
}
