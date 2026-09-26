package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunkupload"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// unavailableContainerClient always fails QueryChunks with a gRPC Unavailable
// status, modelling a tunnel that's already down by the time the chunk
// push's capability probe goes out. calls counts invocations so tests can
// assert the authoritative capability probe runs exactly once per attempt.
// The optional whole-layer query may start alongside it, but no layer is ever
// materialized or uploaded after the capability probe fails.
type unavailableContainerClient struct {
	agentpb.WendyContainerServiceClient // embedded nil
	calls                               int
}

func (c *unavailableContainerClient) QueryChunks(_ context.Context, _ *agentpb.QueryChunksRequest, _ ...grpc.CallOption) (*agentpb.QueryChunksResponse, error) {
	c.calls++
	return nil, status.Error(codes.Unavailable, "tunnel dropped")
}

func (c *unavailableContainerClient) QueryLayers(_ context.Context, _ *agentpb.QueryLayersRequest, _ ...grpc.CallOption) (*agentpb.QueryLayersResponse, error) {
	return nil, status.Error(codes.Unavailable, "tunnel dropped")
}

// closeTracker is an io.Closer that records whether it was closed, via
// AgentConnection.ExtraClosers, so tests can assert exactly which
// connections pushLayersResumingTunnelDrops closed as "intermediate"
// (reconnected away from, but neither the original conn passed in nor the
// one ultimately returned).
type closeTracker struct{ closed *bool }

func (c closeTracker) Close() error {
	*c.closed = true
	return nil
}

// TestRetryableTunnelError is a table test over retryableTunnelError's
// classification: gRPC Unavailable (bare or wrapped) and an EOF-shaped
// stream death are retryable; Unimplemented, context cancellation, and an
// imageBuildFailedError are not.
func TestRetryableTunnelError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"Unavailable bare", status.Error(codes.Unavailable, "tunnel dropped"), true},
		{"Unavailable wrapped", fmt.Errorf("writechunks: %w", status.Error(codes.Unavailable, "tunnel dropped")), true},
		{"Unimplemented", status.Error(codes.Unimplemented, "no chunk-diff support"), false},
		{"Unimplemented wrapped", fmt.Errorf("querychunks: %w", status.Error(codes.Unimplemented, "no chunk-diff support")), false},
		{"context.Canceled", context.Canceled, false},
		{"context.Canceled wrapped", fmt.Errorf("push: %w", context.Canceled), false},
		{"imageBuildFailedError", &imageBuildFailedError{errors.New("build failed")}, false},
		{"wrapped io.EOF", fmt.Errorf("recv: %w", io.EOF), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableTunnelError(tc.err); got != tc.want {
				t.Errorf("retryableTunnelError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPushLayersResumingTunnelDropsReconnectsAndRetries drives the resume
// loop through exactly one drop: connA's capability probe reports
// Unavailable, connA.Reconnect hands back connB (a fakeContainerClient from
// chunkpush_test.go, configured so the single layer resolves as
// already-present on the device — no chunking needed), and attempt 2 on
// connB succeeds outright. Asserts the reconnected connection (connB) is
// what's returned, exactly one reconnect happened, and the resulting header
// carries the device-reported diff ID/size.
func TestPushLayersResumingTunnelDropsReconnectsAndRetries(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	diffID := "sha256:" + strings.Repeat("ab", 32)
	const presentSize int64 = 4096
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex([]byte("compressed-bytes")),
		DiffID:    diffID,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		// Intentionally not valid gzip: if the push tried to decompress this
		// layer (rather than resolving it as already-present) it would fail.
		Blob: []byte("this is not gzip"),
	}}

	unavailable := &unavailableContainerClient{}
	connA := &grpcclient.AgentConnection{ContainerService: unavailable}

	fakeB := &fakeContainerClient{
		queryFn: func(_ *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(_ *agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{
				Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: presentSize}},
			}
		},
	}
	connB := &grpcclient.AgentConnection{ContainerService: fakeB}

	reconnects := 0
	connA.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		return connB, nil
	}
	connB.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		t.Fatal("connB.Reconnect should not be called; attempt 2 (on connB) must succeed")
		return nil, nil
	}

	gotConn, headers, err := pushLayersResumingTunnelDrops(context.Background(), connA, layers, nil, gzipChunkUploadConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotConn != connB {
		t.Fatalf("returned conn = %p, want connB (%p)", gotConn, connB)
	}
	if reconnects != 1 {
		t.Fatalf("reconnects = %d, want 1", reconnects)
	}
	if unavailable.calls != 1 {
		t.Fatalf("connA probe calls = %d, want 1 (one failed attempt, then reconnect)", unavailable.calls)
	}
	if len(headers) != 1 {
		t.Fatalf("expected 1 header, got %d", len(headers))
	}
	if h := headers[0]; h.GetDiffId() != diffID || h.GetSize() != presentSize {
		t.Fatalf("header = {diffID:%q size:%d}, want {diffID:%q size:%d}", h.GetDiffId(), h.GetSize(), diffID, presentSize)
	}
}

// TestPushLayersResumingTunnelDropsDoesNotRetryUnimplemented verifies that an
// Unimplemented probe error (an agent with no chunk-diff support at all)
// bubbles straight up without ever calling Reconnect, so deployByChunkDiff's
// caller can fall back to the registry-push ladder instead of retrying a
// push that can never succeed.
func TestPushLayersResumingTunnelDropsDoesNotRetryUnimplemented(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	conn := &grpcclient.AgentConnection{ContainerService: &probeUnsupportedClient{}}
	conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		t.Fatal("Reconnect must not be called for an Unimplemented probe error")
		return nil, nil
	}

	gotConn, headers, err := pushLayersResumingTunnelDrops(context.Background(), conn, nil, nil, gzipChunkUploadConfig, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isUnimplementedRPCError(err) {
		t.Fatalf("expected an Unimplemented error, got %v", err)
	}
	if headers != nil {
		t.Fatalf("expected nil headers on failure, got %v", headers)
	}
	if gotConn != conn {
		t.Fatalf("expected the original conn returned unchanged, got %p want %p", gotConn, conn)
	}
}

// TestPushLayersResumingTunnelDropsGivesUpAfterAttempts drives three straight
// Unavailable failures (connA -> connB -> connC, one probe failure each) and
// asserts the loop stops at chunkPushResumeAttempts: exactly
// chunkPushResumeAttempts-1 reconnects happen, connC.Reconnect is never
// reached, the final attempt's connection (connC) is what's returned even on
// failure (so the caller can still close it), and connB — reconnected away
// from mid-loop, neither the original conn nor the one finally returned — is
// closed by the loop itself.
func TestPushLayersResumingTunnelDropsGivesUpAfterAttempts(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	connA := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}
	connB := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}
	connC := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}

	var bClosed, cClosed bool
	connB.ExtraClosers = append(connB.ExtraClosers, closeTracker{&bClosed})
	connC.ExtraClosers = append(connC.ExtraClosers, closeTracker{&cClosed})

	reconnects := 0
	connA.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		return connB, nil
	}
	connB.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		return connC, nil
	}
	connC.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		t.Fatal("Reconnect called beyond the attempts cap")
		return nil, nil
	}

	gotConn, headers, err := pushLayersResumingTunnelDrops(context.Background(), connA, nil, nil, gzipChunkUploadConfig, nil)
	if err == nil {
		t.Fatal("expected an error after exhausting all attempts")
	}
	if headers != nil {
		t.Fatalf("expected nil headers on failure, got %v", headers)
	}
	if gotConn != connC {
		t.Fatalf("returned conn = %p, want connC (%p) — the final attempt's connection", gotConn, connC)
	}
	if reconnects != chunkPushResumeAttempts-1 {
		t.Fatalf("reconnects = %d, want %d (chunkPushResumeAttempts-1)", reconnects, chunkPushResumeAttempts-1)
	}
	if !bClosed {
		t.Error("expected connB (an intermediate conn) to be closed by the resume loop")
	}
	if cClosed {
		t.Error("the final conn is the caller's to close, not pushLayersResumingTunnelDrops's")
	}
}

// TestPushLayersResumingTunnelDropsWrapsAFailedReconnectAfterAStall is M3: the
// reconnect that follows a stall can itself fail (the agent is slow to come
// back, or unreachable). The returned error must still say the upload
// stalled — errors.Is(err, chunkupload.ErrStalled) — rather than surfacing
// only the reconnect failure and hiding what actually triggered it.
func TestPushLayersResumingTunnelDropsWrapsAFailedReconnectAfterAStall(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	t.Setenv("WENDY_CHUNK_UPLOAD_BATCH", "16")
	manifestCacheTestDir = t.TempDir()
	chunkStallTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = ""; chunkStallTestDir = "" })

	layerTar := variedChunkTestData(200 * 64 << 10)
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}
	agent := &probeAgent{dev: &probeDevice{staged: map[[32]byte]int{}}, stallAfter: 5}
	conn, _ := startProbeAgent(t, agent)
	// No Reconnect closure: reconnectAgentAfterRestart falls to the LAN
	// redial path (waitForAgentRestart), which re-dials this bogus address,
	// never finds an agent, and fails once ctx expires.
	conn.Addr = "127.0.0.1:1"

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	cfg := chunkUploadConfig{stallTimeout: 30 * time.Millisecond, stallKey: "0123abcd@0.19.3"}

	_, _, err := pushLayersResumingTunnelDrops(ctx, conn, layers, nil, cfg, nil)
	if !errors.Is(err, chunkupload.ErrStalled) {
		t.Fatalf("error = %v, want it to still satisfy errors.Is(err, chunkupload.ErrStalled)\n%s", err, out.String())
	}
	if !chunkUploadStalledRecently(cfg.stallKey, time.Now()) {
		t.Fatal("the stall was not remembered even though the reconnect failed")
	}
}

// TestPushLayersResumingTunnelDropsRemembersAStallOnTheFinalAttempt is M7:
// attempts 1 and 2 fail the capability probe outright (a tunnel drop, so the
// loop reconnects without ever touching cfg), and attempt 3 — the loop's
// last, chunkPushResumeAttempts — stalls. The attempt-exhausted return must
// still remember the stall; before the M7 fix it returned straight past the
// block that does, and the device's next deploy would try uncompressed again
// instead of skipping straight to gzip.
func TestPushLayersResumingTunnelDropsRemembersAStallOnTheFinalAttempt(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	t.Setenv("WENDY_CHUNK_UPLOAD_BATCH", "16")
	manifestCacheTestDir = t.TempDir()
	chunkStallTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = ""; chunkStallTestDir = "" })

	layerTar := variedChunkTestData(200 * 64 << 10)
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}

	connA := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}
	connB := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}
	agentC := &probeAgent{dev: &probeDevice{staged: map[[32]byte]int{}}, stallAfter: 5}
	connC, _ := startProbeAgent(t, agentC)
	connA.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) { return connB, nil }
	connB.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) { return connC, nil }

	cfg := chunkUploadConfig{stallTimeout: 200 * time.Millisecond, stallKey: "0123abcd@0.19.3"}
	if chunkPushResumeAttempts != 3 {
		t.Fatalf("this test assumes chunkPushResumeAttempts == 3 (connA -> connB -> connC), got %d", chunkPushResumeAttempts)
	}

	_, _, err := pushLayersResumingTunnelDrops(context.Background(), connA, layers, nil, cfg, nil)
	if !errors.Is(err, chunkupload.ErrStalled) {
		t.Fatalf("error = %v, want ErrStalled\n%s", err, out.String())
	}
	if !chunkUploadStalledRecently(cfg.stallKey, time.Now()) {
		t.Fatal("the stall on the final (attempt-exhausted) attempt was not remembered")
	}
}

// probeDevice is the device-side chunk store backing probeAgent. It survives
// a simulated "reconnect" (a fresh probeAgent pointed at the same
// probeDevice), the way a real device's chunk store survives the CLI
// reconnecting to the same agent.
type probeDevice struct {
	mu     sync.Mutex
	staged map[[32]byte]int // hash -> times received
}

// probeAgent is a minimal WendyContainerServiceServer for driving
// pushLayersResumingTunnelDrops over a real gRPC transport (bufconn), rather
// than the fakeContainerClient used elsewhere in this package: this test
// needs several concurrently open WriteChunks streams and a mid-transfer
// server Stop(), which a hand-rolled fake can't reproduce faithfully.
type probeAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer
	dev       *probeDevice
	received  atomic.Int64
	openNow   atomic.Int64
	maxOpen   atomic.Int64
	dropAfter int64
	drop      func()
	dropOnce  sync.Once

	// stallAfter > 0 wedges this agent like the #1765 link: once it has
	// received stallAfter chunks, every WriteChunks handler stops reading and
	// waits for its stream to end, so no stream makes progress.
	stallAfter int64
	encMu      sync.Mutex
	encs       []string // grpc-encoding of each WriteChunks stream, in arrival order
}

// encodings returns the grpc-encoding each WriteChunks stream arrived with
// ("" for uncompressed).
func (a *probeAgent) encodings() []string {
	a.encMu.Lock()
	defer a.encMu.Unlock()
	return append([]string(nil), a.encs...)
}

// encodingRecorder is a server stats.Handler that records the compression
// each WriteChunks stream arrived with.
type encodingRecorder struct{ a *probeAgent }

func (r encodingRecorder) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (r encodingRecorder) HandleRPC(_ context.Context, s stats.RPCStats) {
	if in, ok := s.(*stats.InHeader); ok && strings.HasSuffix(in.FullMethod, "/WriteChunks") {
		r.a.encMu.Lock()
		r.a.encs = append(r.a.encs, in.Compression)
		r.a.encMu.Unlock()
	}
}
func (r encodingRecorder) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (r encodingRecorder) HandleConn(context.Context, stats.ConnStats) {}

func (a *probeAgent) QueryChunks(_ context.Context, req *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	a.dev.mu.Lock()
	defer a.dev.mu.Unlock()
	var missing [][]byte
	for _, hb := range req.GetChunkHashes() {
		var h [32]byte
		copy(h[:], hb)
		if a.dev.staged[h] == 0 {
			missing = append(missing, hb)
		}
	}
	return &agentpb.QueryChunksResponse{MissingHashes: missing}, nil
}

func (a *probeAgent) QueryLayers(context.Context, *agentpb.QueryLayersRequest) (*agentpb.QueryLayersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "no")
}

func (a *probeAgent) WriteChunks(stream grpc.ClientStreamingServer[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]) error {
	n := a.openNow.Add(1)
	for {
		m := a.maxOpen.Load()
		if n <= m || a.maxOpen.CompareAndSwap(m, n) {
			break
		}
	}
	defer a.openNow.Add(-1)
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&agentpb.WriteChunksResponse{})
		}
		if err != nil {
			return err
		}
		var h [32]byte
		copy(h[:], msg.GetHash())
		if a.stallAfter > 0 && a.received.Load() >= a.stallAfter {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		a.dev.mu.Lock()
		a.dev.staged[h]++
		a.dev.mu.Unlock()
		if total := a.received.Add(1); a.dropAfter > 0 && total == a.dropAfter {
			a.dropOnce.Do(func() { go a.drop() })
		}
		time.Sleep(100 * time.Microsecond) // device slower than the link, so streams overlap
	}
}

// startProbeAgent starts the given probeAgent as a gRPC server behind a
// bufconn listener, and returns an AgentConnection wired to it the way
// grpcclient's real dialers do.
func startProbeAgent(t *testing.T, a *probeAgent) (*grpcclient.AgentConnection, *grpc.Server) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.InitialWindowSize(8<<20), grpc.InitialConnWindowSize(16<<20), grpc.StatsHandler(encodingRecorder{a: a}))
	agentpb.RegisterWendyContainerServiceServer(srv, a)
	go func() { _ = srv.Serve(lis) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close(); srv.Stop(); lis.Close() })
	return &grpcclient.AgentConnection{Conn: cc, ContainerService: agentpb.NewWendyContainerServiceClient(cc)}, srv
}

// TestPushLayersResumingTunnelDropsResumesWithSeveralStreamsOpen covers Review
// Focus #4 over a real gRPC transport: a multi-batch layer pushed with several
// WriteChunks streams open, dropped once mid-transfer. The plan's Task 4
// claimed this was "covered by the existing chunkresume_test.go", but every
// other test in this file fails at the QueryChunks capability probe or finds
// the layer already present — none of them reaches WriteChunks, let alone
// with more than one stream open. This one does: it requires the resume to
// send only the chunks the device is still missing, and never send a chunk
// twice.
func TestPushLayersResumingTunnelDropsResumesWithSeveralStreamsOpen(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	t.Setenv("WENDY_CHUNK_UPLOAD_BATCH", "16")
	t.Setenv("WENDY_CHUNK_UPLOAD_STREAMS", "")
	t.Cleanup(func() { manifestCacheTestDir = "" })

	for iter := 0; iter < 5; iter++ {
		manifestCacheTestDir = t.TempDir()
		layerTar := variedChunkTestData(300 * 64 << 10)
		dev := &probeDevice{staged: map[[32]byte]int{}}
		agentA := &probeAgent{dev: dev, dropAfter: 120}
		connA, srvA := startProbeAgent(t, agentA)
		agentA.drop = srvA.Stop
		agentB := &probeAgent{dev: dev}
		connB, _ := startProbeAgent(t, agentB)
		reconnects := 0
		connA.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
			reconnects++
			return connB, nil
		}
		layers := []localLayer{{
			Digest:    "sha256:" + sha256Hex(layerTar),
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Blob:      layerTar,
		}}
		got, headers, err := pushLayersResumingTunnelDrops(context.Background(), connA, layers, nil, gzipChunkUploadConfig, nil)
		if err != nil {
			t.Fatalf("iter %d: push failed: %v\n%s", iter, err, out.String())
		}
		if got != connB || reconnects != 1 || len(headers) != 1 {
			t.Fatalf("iter %d: conn=%v reconnects=%d headers=%d", iter, got == connB, reconnects, len(headers))
		}
		unique := map[[32]byte]bool{}
		for _, hb := range headers[0].GetChunkHashes() {
			var h [32]byte
			copy(h[:], hb)
			unique[h] = true
		}
		dev.mu.Lock()
		dups := 0
		for h := range unique {
			if dev.staged[h] == 0 {
				t.Fatalf("iter %d: chunk never staged", iter)
			}
			dups += dev.staged[h] - 1
		}
		dev.mu.Unlock()
		t.Logf("iter %d: unique=%d A received=%d (max %d streams open) B received=%d re-sent duplicates=%d",
			iter, len(unique), agentA.received.Load(), agentA.maxOpen.Load(), agentB.received.Load(), dups)
		if agentA.maxOpen.Load() < 2 {
			t.Fatalf("iter %d: the drop happened with %d stream(s) open; this test did not exercise several streams", iter, agentA.maxOpen.Load())
		}
		if agentB.received.Load() >= int64(len(unique)) {
			t.Fatalf("iter %d: the resume re-sent everything", iter)
		}
		if dups != 0 {
			t.Fatalf("iter %d: the resume re-sent %d chunk(s) the device already had", iter, dups)
		}
	}
}

// TestPushLayersResumingTunnelDropsFallsBackToGzipAfterAStall is Review Focus
// #1: an uncompressed push wedges with several streams open. The watchdog
// fires, the device's stall is remembered, and the retry reconnects and sends
// only what the device is still missing, with gzip.
func TestPushLayersResumingTunnelDropsFallsBackToGzipAfterAStall(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	t.Setenv("WENDY_CHUNK_UPLOAD_BATCH", "16")
	manifestCacheTestDir = t.TempDir()
	chunkStallTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = ""; chunkStallTestDir = "" })

	layerTar := variedChunkTestData(300 * 64 << 10)
	dev := &probeDevice{staged: map[[32]byte]int{}}
	agentA := &probeAgent{dev: dev, stallAfter: 100}
	connA, _ := startProbeAgent(t, agentA)
	agentB := &probeAgent{dev: dev}
	connB, _ := startProbeAgent(t, agentB)
	reconnects := 0
	connA.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		return connB, nil
	}
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}
	cfg := chunkUploadConfig{stallTimeout: 300 * time.Millisecond, stallKey: "0123abcd@0.19.3"}

	got, headers, err := pushLayersResumingTunnelDrops(context.Background(), connA, layers, nil, cfg, nil)
	if err != nil {
		t.Fatalf("push failed: %v\n%s", err, out.String())
	}
	if got != connB || reconnects != 1 || len(headers) != 1 {
		t.Fatalf("conn=%v reconnects=%d headers=%d", got == connB, reconnects, len(headers))
	}
	for _, enc := range agentA.encodings() {
		if enc != "" {
			t.Fatalf("the first attempt opened a %q stream, want uncompressed", enc)
		}
	}
	encs := agentB.encodings()
	if len(encs) == 0 {
		t.Fatal("the retry opened no streams")
	}
	for _, enc := range encs {
		if enc != "gzip" {
			t.Fatalf("the retry opened a %q stream, want gzip", enc)
		}
	}
	if !chunkUploadStalledRecently("0123abcd@0.19.3", time.Now()) {
		t.Fatal("the stall was not remembered")
	}
	// cliNotice writes the fallback notice to os.Stderr, not to out; Task 6's
	// hardware run checks it by eye.
	dev.mu.Lock()
	defer dev.mu.Unlock()
	for h, n := range dev.staged {
		if n > 1 {
			t.Fatalf("chunk %x was sent %d times; the retry must skip staged chunks", h[:4], n)
		}
	}
	if agentB.received.Load() >= int64(len(dev.staged)) {
		t.Fatal("the retry re-sent everything")
	}
}
