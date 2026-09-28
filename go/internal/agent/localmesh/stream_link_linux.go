//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StreamALPN         = "wendy-local-mesh-ble/1"
	streamControl byte = 1
	streamPacket  byte = 2
	// A complete IP packet is queued internally, then split by the writer.
	// It is never a wire frame kind.
	streamWholeIP byte = 3
	// A BLE control bundle can span hundreds of 128-byte L2CAP SDUs. Give
	// the entire frame a bounded budget based on its size; the L2CAP adapter
	// separately fails a write that stops making progress for five seconds.
	streamWriteBase = 8 * time.Second
	streamWriteMax  = 45 * time.Second
	// Keep carrier buffering bounded. Babel and directory traffic must be able
	// to pass a busy IP tunnel after the current frame completes.
	streamPriorityQueue = 32
	// Reserve a few places for complete small IP packets such as QUIC ACKs
	// and app port requests. Large packets use the bulk queue, including
	// their short second fragment, so a fragment cannot jump ahead of its
	// first half merely because it is short.
	streamFastIPQueue = 4
	streamFastIPMax   = 256
	streamFastIPBurst = 4
	// At BLE rates, 64 queued IP fragments can mean many seconds of stale
	// traffic after a burst. Keep a few packets buffered and drop overload.
	streamIPQueue = 8
)

type streamFrame struct {
	kind     byte
	data     []byte
	sequence uint32
	ack      chan error
}

func streamWriteBudget(kind byte, size int) time.Duration {
	if kind == streamPacket {
		return streamWriteBase
	}
	// Allow roughly 512 bytes per second after the base allowance. This is
	// intentionally conservative for a busy shared BLE ACL, while the cap
	// still bounds a peer that keeps accepting bytes too slowly.
	budget := streamWriteBase + time.Duration((size+1023)/1024)*2*time.Second
	return min(budget, streamWriteMax)
}

// OpenStreamHello binds a TLS byte stream to the expected mesh peer. The TLS
// config must have pinned the enrolled peer certificate before this call.
func OpenStreamHello(ctx context.Context, conn net.Conn, org, self, peer int32) error {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return errors.New("mesh stream requires TLS")
	}
	state := tlsConn.ConnectionState()
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != StreamALPN || len(state.PeerCertificates) == 0 {
		return errors.New("mesh stream TLS is not authenticated")
	}
	if _, _, err := Addresses(org, self); err != nil {
		return err
	}
	if _, _, err := Addresses(org, peer); err != nil {
		return err
	}
	if self == peer {
		return errors.New("self mesh stream")
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})
	if err := WriteControl(conn, ControlMessage{Kind: "hello", Hello: &LinkHello{Version: 1, Org: org, Asset: self, Peer: peer, MTU: TunnelMTU, Datagram: DatagramLimit}}); err != nil {
		return err
	}
	m, err := ReadControl(conn)
	if err != nil {
		return err
	}
	if m.Kind != "hello" || m.Hello == nil {
		return errors.New("missing mesh stream hello")
	}
	h := m.Hello
	if h.Version != 1 || h.Org != org || h.Asset != peer || h.Peer != self || h.MTU != TunnelMTU || h.Datagram != DatagramLimit {
		return errors.New("invalid mesh stream hello")
	}
	return nil
}

// streamNodeLink multiplexes directory control and Babel/IP packets on one
// lossless TLS stream. Frame lengths are bounded before allocation. A single
// reader and a single priority writer preserve frame boundaries. The writer
// drops congested IP datagrams rather than blocking the Babel event loop or
// accumulating unbounded latency on a slow BLE ACL.
type streamNodeLink struct {
	conn          net.Conn
	once          sync.Once
	controls      chan ControlMessage
	packets       chan []byte
	priority      chan streamFrame
	fastIP        chan streamFrame
	ip            chan streamFrame
	ipPackets     *streamIPSendQueue
	stop          chan struct{}
	done          chan struct{}
	errMu         sync.Mutex
	err           error
	activeIPStart atomic.Int64
	activeIPBytes atomic.Int64
	lastIPWriteMS atomic.Int64
	rxBytes       atomic.Int64
	lastRxUnixNS  atomic.Int64
	diagIP        bool
}

// sockQueueProber is implemented by CoC connections that can report kernel
// socket queue depths (see bleprovider packetConn.SockQueue). Absent on
// non-BLE links; the diagnostic reports -1s there.
type sockQueueProber interface {
	SockQueue() (inQ, outQ int, err error)
}

func newStreamNodeLink(conn net.Conn) *streamNodeLink {
	l := &streamNodeLink{conn: conn, controls: make(chan ControlMessage, 128), packets: make(chan []byte, 128), priority: make(chan streamFrame, streamPriorityQueue), fastIP: make(chan streamFrame, streamFastIPQueue), ip: make(chan streamFrame, streamIPQueue), ipPackets: newStreamIPSendQueue(), stop: make(chan struct{}), done: make(chan struct{})}
	go l.readLoop()
	go l.writeLoop()
	if os.Getenv("WENDY_MESH_BLE_QUEUE_DIAG") == "1" {
		l.diagIP = true
		go l.queueDiagnosticLoop()
	}
	return l
}

// The opt-in diagnostic is capped at one line per second per BLE link. It
// reports waiting packets only; the active frame and CoC kernel FIFO must be
// measured separately when checking total hop latency.
func (l *streamNodeLink) queueDiagnosticLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := IPQueueStats{}
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			stats := l.ipPackets.snapshot()
			activeBytes := l.activeIPBytes.Load()
			if stats == last && stats.Bytes == 0 && activeBytes == 0 {
				continue
			}
			var activeMS int64
			if started := l.activeIPStart.Load(); started > 0 {
				activeMS = time.Since(time.Unix(0, started)).Milliseconds()
			}
			inQ, outQ := l.sockQueueDepths()
			log.Printf("localmesh BLE queue peer=%s queued_bytes=%d byte_limit=%d queued_packets=%d enqueued=%d dequeued=%d dropped_capacity=%d dropped_expired=%d last_wait_ms=%d max_wait_ms=%d age_limit_ms=%d write_rate_bytes_s=%d active_bytes=%d active_ms=%d last_write_ms=%d rx_bytes=%d rx_last_ms_ago=%d sock_inq=%d sock_outq=%d", l.conn.RemoteAddr(), stats.Bytes, stats.ByteLimit, stats.Packets, stats.Enqueued, stats.Dequeued, stats.DroppedCapacity, stats.DroppedExpired, stats.LastDequeuedWaitMS, stats.MaxDequeuedWaitMS, stats.AgeLimitMS, stats.WriteRateBytesPerSec, activeBytes, activeMS, l.lastIPWriteMS.Load(), l.rxBytes.Load(), l.rxLastMSAgo(), inQ, outQ)
			last = stats
		}
	}
}

func (l *streamNodeLink) Close() error {
	var err error
	l.once.Do(func() {
		l.recordTerminalError(net.ErrClosed)
		close(l.stop)
		err = l.conn.Close()
	})
	return err
}

func (l *streamNodeLink) writeFrame(kind byte, data []byte) error {
	if len(data) == 0 || len(data) > MaxControlMessage+4 || kind == streamPacket && len(data) > DatagramLimit {
		return errors.New("invalid mesh stream frame size")
	}
	frame := streamFrame{kind: kind, data: append([]byte(nil), data...)}
	if kind == streamControl {
		frame.ack = make(chan error, 1)
		select {
		case <-l.stop:
			return l.terminalError()
		case l.priority <- frame:
		}
		select {
		case err := <-frame.ack:
			return err
		case <-l.stop:
			return l.terminalError()
		}
	}
	queue := l.ip
	if data[0] == PacketBabel {
		queue = l.priority
	} else if smallWholeIP(data) {
		queue = l.fastIP
	}
	select {
	case <-l.stop:
		return l.terminalError()
	default:
	}
	select {
	case queue <- frame:
		return nil
	default:
		// A datagram has no delivery guarantee. The next Babel hello or IP
		// retransmission repairs congestion without stalling routing.
		return nil
	}
}

func smallWholeIP(data []byte) bool {
	if len(data) <= fragmentHeader || data[0] != PacketIP {
		return false
	}
	total := int(binary.BigEndian.Uint16(data[5:7]))
	return total <= streamFastIPMax && total+fragmentHeader == len(data) &&
		binary.BigEndian.Uint16(data[7:9]) == 0 && validIP(data[fragmentHeader:])
}

// nextFrame keeps directory/Babel first, lets latency-sensitive IP pass a
// burst of large packets, and serves bulk after at most four small packets.
func (l *streamNodeLink) nextFrame(fastServed *int) (streamFrame, bool) {
	for {
		select {
		case <-l.stop:
			return streamFrame{}, false
		case frame := <-l.priority:
			return frame, true
		default:
		}
		if l.ipPackets != nil {
			if packet, ok := l.ipPackets.pop(time.Now(), fastServed); ok {
				return streamFrame{kind: streamWholeIP, data: packet.data, sequence: packet.sequence}, true
			}
		}
		if *fastServed >= streamFastIPBurst {
			select {
			case <-l.stop:
				return streamFrame{}, false
			case frame := <-l.ip:
				*fastServed = 0
				return frame, true
			default:
			}
		}
		select {
		case <-l.stop:
			return streamFrame{}, false
		case frame := <-l.fastIP:
			*fastServed++
			return frame, true
		default:
		}
		select {
		case <-l.stop:
			return streamFrame{}, false
		case frame := <-l.ip:
			*fastServed = 0
			return frame, true
		default:
		}
		select {
		case <-l.stop:
			return streamFrame{}, false
		case frame := <-l.priority:
			return frame, true
		case frame := <-l.fastIP:
			*fastServed++
			return frame, true
		case frame := <-l.ip:
			*fastServed = 0
			return frame, true
		case <-l.ipPacketWake():
		}
	}
}

func (l *streamNodeLink) ipPacketWake() <-chan struct{} {
	if l.ipPackets == nil {
		return nil
	}
	return l.ipPackets.wake
}

func (l *streamNodeLink) writeLoop() {
	fastServed := 0
	for {
		frame, ok := l.nextFrame(&fastServed)
		if !ok {
			return
		}
		var err error
		if frame.kind == streamWholeIP {
			started := time.Now()
			l.activeIPBytes.Store(int64(len(frame.data)))
			l.activeIPStart.Store(started.UnixNano())
			var fragments [][]byte
			fragments, err = EncodeIP(frame.sequence, frame.data)
			for _, fragment := range fragments {
				if err != nil {
					break
				}
				err = l.writeFrameNow(streamPacket, fragment)
			}
			if err == nil {
				l.ipPackets.recordWrite(len(frame.data), time.Since(started))
			}
			l.lastIPWriteMS.Store(time.Since(started).Milliseconds())
			l.activeIPStart.Store(0)
			l.activeIPBytes.Store(0)
		} else {
			err = l.writeFrameNow(frame.kind, frame.data)
		}
		if err != nil {
			l.recordTerminalError(fmt.Errorf("mesh stream write: %w", err))
			if frame.ack != nil {
				frame.ack <- l.terminalError()
			}
			_ = l.Close()
			return
		}
		if frame.ack != nil {
			frame.ack <- nil
		}
	}
}

func (l *streamNodeLink) writeFrameNow(kind byte, data []byte) error {
	// A separate Write of the five-byte header creates a separate TLS record.
	// On a BLE CoC that record consumes an entire SDU and a radio credit for
	// every IP/Babel/control frame. Keep the wire format, but submit the whole
	// frame in one TLS Write so small packets fit in one encrypted SDU.
	frame := make([]byte, 5+len(data))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	if err := l.conn.SetWriteDeadline(time.Now().Add(streamWriteBudget(kind, len(data)))); err != nil {
		return err
	}
	defer l.conn.SetWriteDeadline(time.Time{})
	return writeFull(l.conn, frame)
}

func (l *streamNodeLink) WriteControl(m ControlMessage) error {
	var b bytes.Buffer
	if err := WriteControl(&b, m); err != nil {
		return err
	}
	return l.writeFrame(streamControl, b.Bytes())
}

func (l *streamNodeLink) SendDatagram(data []byte) error {
	return l.writeFrame(streamPacket, data)
}

// SendIP admits an entire routed packet before any fragment is emitted. IP
// congestion is ordinary packet loss, so an expired or evicted packet is
// dropped intact and the next IP/TCP/QUIC retransmission can repair it.
func (l *streamNodeLink) SendIP(sequence uint32, packet []byte) error {
	if !validIP(packet) || packet[0]>>4 != 4 {
		return errors.New("invalid mesh stream IP packet")
	}
	select {
	case <-l.stop:
		return l.terminalError()
	default:
	}
	now := time.Now()
	admitted := l.ipPackets.enqueue(sequence, packet, now)
	if l.diagIP && sequence%16 == 0 && len(packet) >= 256 {
		log.Printf("localmesh BLE IP event=admit peer=%s packet_id=%d source=%s bytes=%d admitted=%t ts_unix_ns=%d", l.conn.RemoteAddr(), sequence, net.IP(packet[12:16]), len(packet), admitted, now.UnixNano())
	}
	return nil
}

func (l *streamNodeLink) readLoop() {
	defer close(l.done)
	for {
		var header [5]byte
		if _, err := io.ReadFull(l.conn, header[:]); err != nil {
			l.recordTerminalError(err)
			return
		}
		n := binary.BigEndian.Uint32(header[1:])
		if n == 0 || n > MaxControlMessage+4 || header[0] == streamPacket && n > DatagramLimit {
			l.recordTerminalError(errors.New("invalid mesh stream frame"))
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(l.conn, buf); err != nil {
			l.recordTerminalError(err)
			return
		}
		l.rxBytes.Add(int64(len(header) + len(buf)))
		l.lastRxUnixNS.Store(time.Now().UnixNano())
		switch header[0] {
		case streamControl:
			m, err := ReadControl(bytes.NewReader(buf))
			if err != nil {
				l.recordTerminalError(err)
				return
			}
			select {
			case l.controls <- m:
			case <-l.stop:
				return
			}
		case streamPacket:
			select {
			case l.packets <- buf:
			case <-l.stop:
				return
			}
		default:
			l.recordTerminalError(fmt.Errorf("unknown mesh stream frame kind %d", header[0]))
			return
		}
	}
}

func (l *streamNodeLink) ReadControl() (ControlMessage, error) {
	select {
	case m := <-l.controls:
		return m, nil
	case <-l.done:
		return ControlMessage{}, l.terminalError()
	}
}

func (l *streamNodeLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case packet := <-l.packets:
		return packet, nil
	case <-l.done:
		return nil, l.terminalError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// rxLastMSAgo reports milliseconds since the last received stream byte,
// or -1 when nothing has been received yet.
func (l *streamNodeLink) rxLastMSAgo() int64 {
	last := l.lastRxUnixNS.Load()
	if last == 0 {
		return -1
	}
	return time.Since(time.Unix(0, last)).Milliseconds()
}

// sockQueueDepths returns kernel socket queue depths (unread RX, unsent TX)
// for instrumented connections, or -1, -1 when unavailable.
func (l *streamNodeLink) sockQueueDepths() (int, int) {
	prober, ok := l.conn.(sockQueueProber)
	if !ok {
		return -1, -1
	}
	inQ, outQ, err := prober.SockQueue()
	if err != nil {
		return -1, -1
	}
	return inQ, outQ
}

// Keep the first locally observed terminal cause. In particular, a writer
// failure must survive the read error induced by closing the same connection.
// Explicit Close records net.ErrClosed first so a later interrupted write is
// not mislabeled as the initiating failure. This does not establish wire order.
func (l *streamNodeLink) recordTerminalError(err error) {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.err == nil {
		l.err = err
	}
}

func (l *streamNodeLink) terminalError() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.err != nil {
		return l.err
	}
	return net.ErrClosed
}
