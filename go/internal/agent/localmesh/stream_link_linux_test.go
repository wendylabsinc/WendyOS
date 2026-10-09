//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func priorityTestPacket(t *testing.T, size int, id uint32) [][]byte {
	t.Helper()
	p := make([]byte, size)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	p[20] = byte(id)
	fragments, err := EncodeIP(id, p)
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}

func readStreamTestFrame(t *testing.T, c net.Conn) (byte, []byte) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [5]byte
	if _, err := io.ReadFull(c, header[:]); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(c, p); err != nil {
		t.Fatal(err)
	}
	return header[0], p
}

func TestStreamLinkSmallIPClassification(t *testing.T) {
	small := priorityTestPacket(t, 100, 1)
	if len(small) != 1 || !smallWholeIP(small[0]) {
		t.Fatal("complete small IP packet did not enter latency lane")
	}
	large := priorityTestPacket(t, 1200, 2)
	if len(large) != 2 || smallWholeIP(large[0]) || smallWholeIP(large[1]) {
		t.Fatal("a large packet fragment entered latency lane")
	}
	if smallWholeIP([]byte{PacketIP, 1}) || smallWholeIP([]byte{PacketBabel, 1}) {
		t.Fatal("invalid/non-IP datagram entered latency lane")
	}
}

func TestStreamLinkSmallIPOvertakesBulkWithoutReorderingFragments(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	link := newStreamNodeLink(gate)
	defer link.Close()
	defer b.Close()
	large := priorityTestPacket(t, 1200, 10)
	small := priorityTestPacket(t, 100, 11)
	if err := link.SendDatagram(large[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("first bulk frame did not start")
	}
	for _, d := range [][]byte{large[1], large[0], large[1], small[0], {PacketBabel, 1}} {
		if err := link.SendDatagram(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(link.priority) != 1 || len(link.fastIP) != 1 || len(link.ip) != 3 {
		t.Fatalf("unexpected queue occupancy: priority=%d fast=%d bulk=%d", len(link.priority), len(link.fastIP), len(link.ip))
	}
	close(gate.release)
	want := [][]byte{large[0], {PacketBabel, 1}, small[0], large[1], large[0], large[1]}
	for i, expected := range want {
		kind, got := readStreamTestFrame(t, b)
		if kind != streamPacket || !bytes.Equal(got, expected) {
			t.Fatalf("frame %d = kind %d %x; want packet %x", i, kind, got, expected)
		}
	}
}

func TestStreamLinkSmallIPFairnessAndBoundedQueue(t *testing.T) {
	l := &streamNodeLink{priority: make(chan streamFrame, streamPriorityQueue), fastIP: make(chan streamFrame, streamFastIPQueue), ip: make(chan streamFrame, streamIPQueue), stop: make(chan struct{})}
	small := priorityTestPacket(t, 100, 1)[0]
	bulk := priorityTestPacket(t, 1200, 2)[0]
	for i := 0; i < streamFastIPQueue; i++ {
		l.fastIP <- streamFrame{kind: streamPacket, data: small}
	}
	for i := 0; i < 2; i++ {
		l.ip <- streamFrame{kind: streamPacket, data: bulk}
	}
	l.priority <- streamFrame{kind: streamPacket, data: []byte{PacketBabel, 1}}
	served := 0
	want := [][]byte{{PacketBabel, 1}, small, small, small, small, bulk, bulk}
	for i, expected := range want {
		frame, ok := l.nextFrame(&served)
		if !ok || !bytes.Equal(frame.data, expected) {
			t.Fatalf("frame %d = %x, %v; want %x", i, frame.data, ok, expected)
		}
	}
	for i := 0; i < streamFastIPQueue; i++ {
		if err := l.SendDatagram(small); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SendDatagram(small); err != nil {
		t.Fatal(err)
	}
	if len(l.fastIP) != streamFastIPQueue {
		t.Fatalf("fast IP queue grew beyond bound: %d", len(l.fastIP))
	}
	close(l.stop)
	if err := l.SendDatagram(small); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("send after close = %v", err)
	}
}

type firstWriteGate struct {
	net.Conn
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

type tlsRecordCounter struct {
	net.Conn
	applicationRecords atomic.Uint64
}

func (c *tlsRecordCounter) Write(p []byte) (int, error) {
	if len(p) >= 5 && p[0] == 23 { // TLS application-data record
		c.applicationRecords.Add(1)
	}
	return c.Conn.Write(p)
}

func TestCoalescedFrameKeepsOldWireFormat(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	link := &streamNodeLink{conn: a}
	want := []byte{PacketIP, 1, 2, 3}
	done := make(chan error, 1)
	go func() { done <- link.writeFrameNow(streamPacket, want) }()
	var header [5]byte
	if _, err := io.ReadFull(b, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != streamPacket || binary.BigEndian.Uint32(header[1:]) != uint32(len(want)) {
		t.Fatalf("old reader saw header %v", header)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("old reader got %v, %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	old, newConn := net.Pipe()
	defer old.Close()
	reader := newStreamNodeLink(newConn)
	defer reader.Close()
	go func() {
		_, _ = old.Write(header[:])
		_, _ = old.Write(want)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := reader.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("new reader got old frame %v, %v", got, err)
	}
}

func TestSmallStreamFrameUsesOneTLS13Record(t *testing.T) {
	a, b := testCredentials(t)
	clientConfig, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, err := b.PeerTLS(a.Asset)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig.NextProtos, serverConfig.NextProtos = []string{StreamALPN}, []string{StreamALPN}
	left, right := net.Pipe()
	underlay := &tlsRecordCounter{Conn: left}
	client, server := tls.Client(underlay, clientConfig), tls.Server(right, serverConfig)
	defer func() { _ = left.Close(); _ = right.Close(); _ = client.Close(); _ = server.Close() }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if len(client.ConnectionState().PeerCertificates) == 0 || client.ConnectionState().NegotiatedProtocol != StreamALPN {
		t.Fatal("mutual TLS/ALPN not established")
	}
	clientLink, serverLink := newStreamNodeLink(client), newStreamNodeLink(server)
	defer clientLink.Close()
	defer serverLink.Close()
	before := underlay.applicationRecords.Load()
	want := []byte{PacketIP, 1, 2, 3}
	if err := clientLink.SendDatagram(want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := serverLink.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("mTLS peer received %v, %v", got, err)
	}
	if records := underlay.applicationRecords.Load() - before; records != 1 {
		t.Fatalf("small BLE stream frame used %d TLS records, want 1", records)
	}
}

func (c *firstWriteGate) Write(p []byte) (int, error) {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.Conn.Write(p)
}

func TestStreamLinkPrioritizesBabelWithoutBlockingRouting(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	left, right := newStreamNodeLink(gate), newStreamNodeLink(b)
	defer left.Close()
	defer right.Close()
	if err := left.SendDatagram([]byte{PacketIP, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	// A blocked BLE writer used to block the one Babel event loop on its
	// stream mutex. These sends must only enqueue, and Babel must pass the
	// queued IP packet as soon as the current frame finishes.
	start := time.Now()
	ip := []byte{PacketIP, 2}
	if err := left.SendDatagram(ip); err != nil {
		t.Fatal(err)
	}
	ip[1] = 99 // The queued datagram must own its buffer.
	if err := left.SendDatagram([]byte{PacketBabel, 3}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("routing send blocked behind BLE writer")
	}
	close(gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i, want := range [][]byte{{PacketIP, 1}, {PacketBabel, 3}, {PacketIP, 2}} {
		got, err := right.ReceiveDatagram(ctx)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %v, %v; want %v", i, got, err, want)
		}
	}
}

func TestStreamLinkCongestionHasBoundedIPQueue(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	left, right := newStreamNodeLink(gate), newStreamNodeLink(b)
	defer left.Close()
	defer right.Close()
	if err := left.SendDatagram([]byte{PacketIP, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	start := time.Now()
	for i := 1; i < streamIPQueue+10; i++ {
		if err := left.SendDatagram([]byte{PacketIP, byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("congestion blocked IP ingress")
	}
	close(gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i <= streamIPQueue; i++ {
		got, err := right.ReceiveDatagram(ctx)
		if err != nil || !bytes.Equal(got, []byte{PacketIP, byte(i)}) {
			t.Fatalf("frame %d = %v, %v", i, got, err)
		}
	}
}

func TestStreamLinkLargeControlMakesSlowProgress(t *testing.T) {
	a, b := net.Pipe()
	link := newStreamNodeLink(a)
	defer link.Close()
	defer b.Close()
	// A signed bundle is several KiB and crosses many BLE SDUs. A reader
	// draining 128 bytes every 100 ms needs more than the former fixed 3s.
	message := ControlMessage{Kind: "bundle", Bundle: [][]byte{bytes.Repeat([]byte{0x42}, 4096)}}
	var encoded bytes.Buffer
	if err := WriteControl(&encoded, message); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- link.WriteControl(message) }()
	buf := make([]byte, 128)
	start := time.Now()
	for received, total := 0, encoded.Len()+5; received < total; {
		_ = b.SetReadDeadline(time.Now().Add(20 * time.Second))
		n, err := b.Read(buf[:min(len(buf), total-received)])
		if err != nil {
			t.Fatal(err)
		}
		received += n
		time.Sleep(100 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 3*time.Second {
		t.Fatal("test did not exercise a write longer than 3 seconds")
	}
}

func TestStreamLinkMultiplexAndBounds(t *testing.T) {
	a, b := net.Pipe()
	left, right := newStreamNodeLink(a), newStreamNodeLink(b)
	defer left.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() {
		if err := left.WriteControl(ControlMessage{Kind: "hello", Hello: &LinkHello{Version: 1}}); err != nil {
			written <- err
			return
		}
		written <- left.SendDatagram([]byte{PacketBabel, 1, 2, 3})
	}()
	m, err := right.ReadControl()
	if err != nil || m.Kind != "hello" || m.Hello == nil || m.Hello.Version != 1 {
		t.Fatalf("control: %+v, %v", m, err)
	}
	packet, err := right.ReceiveDatagram(ctx)
	if err != nil || string(packet) != string([]byte{PacketBabel, 1, 2, 3}) {
		t.Fatalf("packet: %v, %v", packet, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := left.SendDatagram(make([]byte, DatagramLimit+1)); err == nil {
		t.Fatal("oversized packet accepted")
	}
}

func TestStreamLinkRejectsOversizedFrame(t *testing.T) {
	a, b := net.Pipe()
	link := newStreamNodeLink(a)
	defer link.Close()
	defer b.Close()
	var header [5]byte
	header[0] = streamPacket
	binary.BigEndian.PutUint32(header[1:], DatagramLimit+1)
	if _, err := b.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := link.ReceiveDatagram(ctx); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestTLSStreamHelloPinsPeerAndCarriesControl(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Asset)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.NextProtos = []string{StreamALPN}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		secure := tls.Server(raw, serverTLS)
		defer secure.Close()
		if err = secure.HandshakeContext(ctx); err == nil {
			err = OpenStreamHello(ctx, secure, b.Org, b.Asset, a.Asset)
		}
		if err == nil {
			link := newStreamNodeLink(secure)
			defer link.Close()
			var m ControlMessage
			m, err = link.ReadControl()
			if err == nil && m.Kind != "bundle" {
				err = context.DeadlineExceeded
			}
		}
		serverResult <- err
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS.NextProtos = []string{StreamALPN}
	secure := tls.Client(raw, clientTLS)
	defer secure.Close()
	if err = secure.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = OpenStreamHello(ctx, secure, a.Org, a.Asset, b.Asset); err != nil {
		t.Fatal(err)
	}
	link := newStreamNodeLink(secure)
	defer link.Close()
	if err = link.WriteControl(ControlMessage{Kind: "bundle", Bundle: a.Certificate.Certificate}); err != nil {
		t.Fatal(err)
	}
	if err = <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestTalkerAttributionTopAndReset(t *testing.T) {
	l := &streamNodeLink{}
	mkTCP := func(dst net.IP, dport uint16, n int) []byte {
		p := make([]byte, 40+n)
		p[0] = 0x45
		binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
		p[9] = 6
		copy(p[12:16], net.ParseIP("10.88.1.204").To4())
		copy(p[16:20], dst.To4())
		binary.BigEndian.PutUint16(p[22:24], dport)
		return p
	}
	dst := net.ParseIP("10.88.2.24")
	l.noteTalker(mkTCP(dst, 43185, 100), true)
	l.noteTalker(mkTCP(dst, 43185, 100), true)
	other := net.ParseIP("10.88.1.185")
	l.noteTalker(mkTCP(other, 80, 10), true)
	outKey, outStat, _, _ := l.topTalker()
	if outKey.dst != "10.88.2.24" || outKey.port != 43185 || outStat.packets != 2 {
		t.Fatalf("top=%v %+v", outKey, outStat)
	}
	// Reset: second call sees only new traffic.
	l.noteTalker(mkTCP(other, 80, 10), true)
	outKey, outStat, _, _ = l.topTalker()
	if outKey.dst != "10.88.1.185" || outStat.packets != 1 {
		t.Fatalf("after reset top=%v %+v", outKey, outStat)
	}
	// Non-first fragment attributes without port.
	frag := mkTCP(dst, 43185, 100)
	frag[6], frag[7] = 0x20, 0x01
	l.noteTalker(frag, true)
	outKey, _, _, _ = l.topTalker()
	if outKey.dst != "10.88.2.24" || outKey.port != -1 {
		t.Fatalf("fragment top=%v", outKey)
	}
	// Inbound attributes the source port.
	inPkt := mkTCP(dst, 43185, 100)
	l.noteTalker(inPkt, false)
	_, _, inKey, inStat := l.topTalker()
	if inKey.dst != "10.88.1.204" || inKey.port != 0 || inStat.packets != 1 {
		t.Fatalf("inbound top=%v %+v", inKey, inStat)
	}
	l.noteTalker([]byte{0x45}, true)
}
