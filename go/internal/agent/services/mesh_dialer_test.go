package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

var dialerTestTarget = DeliveryTarget{Device: testMeshDevice, AssetUUID: testAssetUUID}
var dialerOtherTarget = DeliveryTarget{Device: mustTestDevice(testAssetUUIDB), AssetUUID: testAssetUUIDB}

func testDialer() (*MeshDialer, *dialerProbes) {
	p := &dialerProbes{}
	d := NewMeshDialer(zap.NewNop(), "", "", "", nil)
	d.lanLookup = func(ctx context.Context, device meshid.Device) (string, bool) {
		p.lookups++
		return p.lanAddr, p.lanAddr != ""
	}
	d.dialLAN = func(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error) {
		p.lanDials++
		if p.lanErr != nil {
			return nil, p.lanErr
		}
		a, _ := net.Pipe()
		return a, nil
	}
	d.serviceDial = func(ctx context.Context, device meshid.Device, port uint16) (net.Conn, error) {
		p.brokerDials++
		if p.brokerErr != nil {
			return nil, p.brokerErr
		}
		a, _ := net.Pipe()
		return a, nil
	}
	return d, p
}

type dialerProbes struct {
	lanAddr                        string
	lanErr, brokerErr              error
	lookups, lanDials, brokerDials int
}

func TestDialDeviceUsesLANWhenAvailable(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = "192.168.1.50:50052"
	conn, mode, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lanDials != 1 || p.brokerDials != 0 {
		t.Fatalf("lan=%d broker=%d, want 1/0", p.lanDials, p.brokerDials)
	}
	if mode != "lan-direct" {
		t.Fatalf("mode = %q, want lan-direct", mode)
	}
}

func TestDialDeviceFallsBackToBroker(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = "192.168.1.50:50052"
	p.lanErr = errors.New("connection refused")
	conn, mode, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lanDials != 1 || p.brokerDials != 1 {
		t.Fatalf("lan=%d broker=%d, want 1/1", p.lanDials, p.brokerDials)
	}
	if mode != "local-mesh" {
		t.Fatalf("mode = %q, want local-mesh", mode)
	}
}

func TestDialDeviceBrokerOnlyWhenNoLANPeer(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = "" // not found on LAN
	conn, mode, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lanDials != 0 || p.brokerDials != 1 {
		t.Fatalf("lan=%d broker=%d, want 0/1", p.lanDials, p.brokerDials)
	}
	if mode != "local-mesh" {
		t.Fatalf("mode = %q, want local-mesh", mode)
	}
}

// TestDialDeviceErrorModeIsEmpty proves that on total dial failure (both LAN
// and broker fail, or broker fails when LAN was never tried) the mode return
// is the zero value, matching the mesh.PeerDialer contract Proxy relies on
// when labeling its "wendy net connection failed" log.
func TestDialDeviceErrorModeIsEmpty(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = ""
	p.brokerErr = errors.New("broker unreachable")
	conn, mode, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err == nil {
		conn.Close()
		t.Fatal("expected error")
	}
	if mode != "" {
		t.Fatalf("mode on error = %q, want empty", mode)
	}
}

// TestDialDeviceRecordsMeshMetrics proves DialDevice reports mesh.connections
// and mesh.dial.duration_ms for each leg it attempts — including the failed
// LAN leg on a fallback — through a real MeshMetrics wired to a fake
// publisher, since MeshMetrics is the authority on dial mode/result/timing
// per the design spec.
func TestDialDeviceRecordsMeshMetrics(t *testing.T) {
	pub := &fakeTelemetryPublisher{}
	metrics := NewMeshMetrics(pub, zap.NewNop())
	d, p := testDialer()
	d.metrics = metrics

	tick := time.Now()
	d.now = func() time.Time { tick = tick.Add(time.Millisecond); return tick }

	// Leg 1: LAN succeeds outright.
	p.lanAddr = "192.168.1.50:50052"
	conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// Leg 2 (different device, so it isn't served by the leg-1 LAN cache
	// entry): LAN fails, falls back to broker which succeeds.
	p.lanErr = errors.New("refused")
	conn, _, err = d.DialTarget(context.Background(), dialerOtherTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	metrics.publish(newAgentResource(), time.Now())
	if len(pub.metricReqs) != 1 {
		t.Fatalf("PublishMetrics called %d times, want 1", len(pub.metricReqs))
	}
	conns := findMetric(pub.metricReqs[0], "mesh.connections")
	if conns == nil {
		t.Fatal("mesh.connections metric missing")
	}
	var sawLANOk, sawLANErr, sawRelayOk bool
	for _, dp := range conns.GetSum().GetDataPoints() {
		mode, _ := attrString(dp.GetAttributes(), "mesh.mode")
		result, _ := attrString(dp.GetAttributes(), "mesh.result")
		switch {
		case mode == "lan-direct" && result == "ok":
			sawLANOk = true
		case mode == "lan-direct" && result == "error":
			sawLANErr = true
		case mode == "local-mesh" && result == "ok":
			sawRelayOk = true
		}
	}
	if !sawLANOk || !sawLANErr || !sawRelayOk {
		t.Fatalf("missing expected connection outcomes: lanOk=%v lanErr=%v relayOk=%v", sawLANOk, sawLANErr, sawRelayOk)
	}
	if findMetric(pub.metricReqs[0], "mesh.dial.duration_ms") == nil {
		t.Fatal("mesh.dial.duration_ms metric missing")
	}
}

func TestDialDeviceCachesLANOutcome(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = "192.168.1.50:50052"
	now := time.Now()
	d.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	if p.lookups != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", p.lookups)
	}

	now = now.Add(2 * lanCacheTTL) // expire
	conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lookups != 2 {
		t.Fatalf("lookups after TTL = %d, want 2", p.lookups)
	}
}

func TestDialDeviceCachesNegativeOutcome(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = ""
	for i := 0; i < 3; i++ {
		conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	if p.lookups != 1 {
		t.Fatalf("lookups = %d, want 1 (negative result cached)", p.lookups)
	}
}

// TestUpdateIdentitySwapsSnapshotAndClearsLANCache covers the live
// re-provisioning path: a dialer constructed unenrolled (empty identity) must
// pick up the fresh identity from UpdateIdentity — dials read the snapshot at
// dial time, not construction time — and the LAN outcome cache must be
// dropped, since re-enrollment can change org and stale positive entries
// could dial peers the new identity cannot authenticate against.
func TestUpdateIdentitySwapsSnapshotAndClearsLANCache(t *testing.T) {
	p := &dialerProbes{}
	d := NewMeshDialer(zap.NewNop(), "", "", "", nil)
	d.lanLookup = func(ctx context.Context, device meshid.Device) (string, bool) {
		p.lookups++
		return p.lanAddr, p.lanAddr != ""
	}
	d.dialLAN = func(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error) {
		p.lanDials++
		a, _ := net.Pipe()
		return a, nil
	}

	// Boot-time snapshot on an unenrolled device is all-empty.
	if got := d.identity(); got != (meshIdentity{}) {
		t.Fatalf("initial identity = %+v, want zero value", got)
	}

	// Prime the LAN cache with a positive entry.
	p.lanAddr = "192.168.1.50:50052"
	conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lookups != 1 {
		t.Fatalf("lookups = %d, want 1", p.lookups)
	}

	d.UpdateIdentity("CERT", "KEY", "CHAIN")

	want := meshIdentity{
		certPEM: "CERT", keyPEM: "KEY", chainPEM: "CHAIN",
	}
	if got := d.identity(); got != want {
		t.Fatalf("identity after UpdateIdentity = %+v, want %+v", got, want)
	}

	// The cached LAN outcome must be gone: the next dial re-runs discovery.
	conn, _, err = d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if p.lookups != 2 {
		t.Fatalf("lookups after UpdateIdentity = %d, want 2 (cache cleared)", p.lookups)
	}
}

// TestDialDeviceReturnsWhenCtxExpires proves DialDevice propagates its ctx
// into the dial path so a caller's dial budget (mesh.Proxy passes 15s)
// actually bounds a stuck dial.
func TestDialDeviceReturnsWhenCtxExpires(t *testing.T) {
	d, p := testDialer()
	p.lanAddr = "" // force the broker path
	d.serviceDial = func(ctx context.Context, device meshid.Device, port uint16) (net.Conn, error) {
		<-ctx.Done() // a well-behaved dial blocks until the caller's ctx expires
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := d.DialTarget(ctx, dialerTestTarget, 8080); err == nil {
		t.Fatal("expected error from expired ctx")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("DialDevice took %v, want prompt return on ctx expiry", elapsed)
	}
}

// TestDialBoundContext covers the two phases of a mesh dial's stream context:
// before established() the caller's ctx bounds it (dial budget); after
// established() the stream must outlive the caller's ctx.
func TestDialBoundContext(t *testing.T) {
	t.Run("caller cancel propagates before established", func(t *testing.T) {
		ctx, cancelCaller := context.WithCancel(context.Background())
		sctx, cancel, _ := dialBoundContext(ctx)
		defer cancel()
		cancelCaller()
		select {
		case <-sctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("stream ctx not cancelled by caller ctx during dial phase")
		}
	})

	t.Run("established stream survives caller cancel", func(t *testing.T) {
		ctx, cancelCaller := context.WithCancel(context.Background())
		sctx, cancel, established := dialBoundContext(ctx)
		established()
		cancelCaller()
		select {
		case <-sctx.Done():
			t.Fatal("stream ctx cancelled by caller ctx after established")
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case <-sctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("stream ctx not cancelled by its own cancel")
		}
	})
}

func TestAdministrativeDialDoesNotInventCloudAuthorization(t *testing.T) {
	d := NewMeshDialer(zap.NewNop(), "", "", "", nil)
	d.lanLookup = func(context.Context, meshid.Device) (string, bool) { return "", false }
	if conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 50052); err == nil {
		conn.Close()
		t.Fatal("unreachable administrative target unexpectedly connected")
	}
}

// --- streamNetConn CloseWrite contract (Task 4 review requirement) ---

// fakeTunnelStream is a tunnelStream test double that records sent frames and
// lets the test control what recv() yields.
type fakeTunnelStream struct {
	mu         sync.Mutex
	sent       []fakeFrame
	closeSends int

	recvCh chan fakeFrame // test pushes frames for recv() to yield
	closed chan struct{}  // closed to make recv() return an error (EOF-like)
}

type fakeFrame struct {
	payload   []byte
	halfClose bool
}

func newFakeTunnelStream() *fakeTunnelStream {
	return &fakeTunnelStream{
		recvCh: make(chan fakeFrame, 16),
		closed: make(chan struct{}),
	}
}

func (f *fakeTunnelStream) send(p []byte, hc bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]byte(nil), p...)
	f.sent = append(f.sent, fakeFrame{payload: cp, halfClose: hc})
	return nil
}

func (f *fakeTunnelStream) recv() ([]byte, bool, error) {
	select {
	case fr := <-f.recvCh:
		return fr.payload, fr.halfClose, nil
	case <-f.closed:
		return nil, false, io.EOF
	}
}

func (f *fakeTunnelStream) closeSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeSends++
	return nil
}

func (f *fakeTunnelStream) sentFrames() []fakeFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeFrame, len(f.sent))
	copy(out, f.sent)
	return out
}

func TestStreamNetConnCloseWriteHalfClosesAndKeepsReading(t *testing.T) {
	stream := newFakeTunnelStream()
	var teardowns int
	conn := streamNetConn(stream, func() { teardowns++ })

	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("streamNetConn result does not implement CloseWrite() error")
	}

	// (a) CloseWrite sends a half_close frame on the underlying stream.
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		frames := stream.sentFrames()
		found := false
		for _, fr := range frames {
			if fr.halfClose {
				found = true
				break
			}
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no half_close frame sent within timeout; frames=%v", frames)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// (b) data still flows stream -> conn after CloseWrite.
	stream.recvCh <- fakeFrame{payload: []byte("hello")}
	buf := make([]byte, 16)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read after CloseWrite: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("Read = %q, want %q", buf[:n], "hello")
	}

	// (c) a subsequent Close tears everything down and runs teardown exactly once.
	close(stream.closed)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Give the stream->pipe goroutine a moment to observe EOF and also call finish.
	time.Sleep(50 * time.Millisecond)
	if teardowns != 1 {
		t.Fatalf("teardowns = %d, want 1", teardowns)
	}
	// Close must be idempotent.
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if teardowns != 1 {
		t.Fatalf("teardowns after second Close = %d, want 1", teardowns)
	}
}

// TestStreamNetConnInboundHalfCloseKeepsWrites is the reviewer's regression
// scenario: a peer that half-closes its response direction must NOT truncate
// our upload direction. Reads drain the buffered data then return io.EOF;
// Writes keep flowing onto the stream.
func TestStreamNetConnInboundHalfCloseKeepsWrites(t *testing.T) {
	stream := newFakeTunnelStream()
	conn := streamNetConn(stream, func() {})
	defer conn.Close()

	// Peer sends final data with half_close set.
	stream.recvCh <- fakeFrame{payload: []byte("resp"), halfClose: true}

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "resp" {
		t.Fatalf("Read = %q, want %q", buf[:n], "resp")
	}
	if _, err := conn.Read(buf); err != io.EOF {
		t.Fatalf("Read after inbound half-close = %v, want io.EOF", err)
	}

	// Upload direction must still work.
	if _, err := conn.Write([]byte("upload")); err != nil {
		t.Fatalf("Write after inbound half-close: %v", err)
	}
	var uploaded bool
	for _, fr := range stream.sentFrames() {
		if string(fr.payload) == "upload" && !fr.halfClose {
			uploaded = true
		}
	}
	if !uploaded {
		t.Fatalf("upload frame not sent after inbound half-close; frames=%+v", stream.sentFrames())
	}

	// And our own CloseWrite still half-closes cleanly afterwards.
	cw := conn.(interface{ CloseWrite() error })
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	stream.mu.Lock()
	closeSends := stream.closeSends
	stream.mu.Unlock()
	if closeSends == 0 {
		t.Fatal("CloseWrite did not call closeSend on the stream")
	}
}

func TestMeshDialTargetCarriesNoAddress(t *testing.T) {
	// The fixed gRPC target must embed no peer address: the address (which may
	// contain an IPv6 zone "%") is supplied via the context dialer instead, so
	// it never reaches gRPC's URL parser.
	if !strings.HasPrefix(meshDialTarget, "passthrough:///") {
		t.Fatalf("meshDialTarget must use the passthrough resolver, got %q", meshDialTarget)
	}
	if strings.ContainsAny(meshDialTarget, "%[") {
		t.Fatalf("meshDialTarget must not embed an address/zone, got %q", meshDialTarget)
	}
}

func TestMeshDialContextDialerDialsVerbatim(t *testing.T) {
	// The dialer must connect to the exact hostport it was given, via the
	// standard library (the only thing that understands IPv6 zone ids), and
	// must ignore the gRPC target argument.
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		ln, err := net.Listen("tcp", host+":0")
		if err != nil {
			t.Skipf("cannot listen on %s (unavailable in this env): %v", host, err)
		}
		dialer := meshDialContextDialer(ln.Addr().String())
		conn, err := dialer(context.Background(), "grpc-target-should-be-ignored")
		if err != nil {
			ln.Close()
			t.Fatalf("dialer(%s) error: %v", ln.Addr(), err)
		}
		conn.Close()
		ln.Close()
	}
}

func TestDialTargetRejectsMismatchedCloudUUIDBeforeDiscovery(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprint("routed=", routed), func(t *testing.T) {
			d, probes := testDialer()
			target := dialerTestTarget
			target.AssetUUID = testAssetUUIDB
			dial := d.DialTarget
			if routed {
				dial = d.DialRoutedTarget
			}
			if conn, _, err := dial(context.Background(), target, 8080); err == nil {
				conn.Close()
				t.Fatal("accepted a Cloud UUID that differs from the device principal")
			}
			if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
				t.Fatalf("mismatched identity reached a transport: %+v", probes)
			}
		})
	}
}

func TestDialTargetRejectsAbsentDeviceBeforeDialing(t *testing.T) {
	d, probes := testDialer()
	_, _, err := d.DialTarget(context.Background(), DeliveryTarget{}, 50052)
	if err == nil || !strings.Contains(err.Error(), "invalid wendy net service target") {
		t.Fatalf("absent target was not rejected at the identity boundary: %v", err)
	}
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatal("absent target triggered network discovery or dialing")
	}
}
