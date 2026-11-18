//go:build regression_chunk_stall

package commands

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
)

type chunkStallConnectionKey struct{}

type chunkStallStore struct {
	*containerd.Client
	mu     sync.Mutex
	staged map[[32]byte]int
	count  atomic.Int64
}

func (s *chunkStallStore) StageChunk(ctx context.Context, hash [32]byte, data []byte) error {
	if err := s.Client.StageChunk(ctx, hash, data); err != nil {
		return err
	}
	s.mu.Lock()
	s.staged[hash]++
	s.mu.Unlock()
	s.count.Add(1)
	return nil
}

type chunkStallWire struct {
	store          *chunkStallStore
	connections    atomic.Int64
	active         atomic.Int64
	mu             sync.Mutex
	encodings      map[int64][]string
	blockedAt      time.Time
	firstCancelled time.Time
	blocked        chan struct{}
	once           sync.Once
}

func (w *chunkStallWire) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return context.WithValue(ctx, chunkStallConnectionKey{}, w.connections.Add(1))
}
func (w *chunkStallWire) HandleConn(context.Context, stats.ConnStats)                     {}
func (w *chunkStallWire) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (w *chunkStallWire) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if header, ok := event.(*stats.InHeader); ok && strings.HasSuffix(header.FullMethod, "/WriteChunks") {
		id := ctx.Value(chunkStallConnectionKey{}).(int64)
		w.mu.Lock()
		w.encodings[id] = append(w.encodings[id], header.Compression)
		w.mu.Unlock()
	}
}

type stalledChunkServerStream struct {
	grpc.ServerStream
	wire *chunkStallWire
}

func (s *stalledChunkServerStream) RecvMsg(message any) error {
	// The first real TCP connection stops consuming payload after some chunks
	// have reached disk. HTTP/2 flow control and CloseAndRecv now really block;
	// the service, store, retry planner and watchdog remain production code.
	if s.wire.store.count.Load() >= 8 {
		s.wire.once.Do(func() {
			s.wire.mu.Lock()
			s.wire.blockedAt = time.Now()
			s.wire.mu.Unlock()
			close(s.wire.blocked)
		})
		<-s.Context().Done()
		return s.Context().Err()
	}
	return s.ServerStream.RecvMsg(message)
}

func (w *chunkStallWire) intercept(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if !strings.HasSuffix(info.FullMethod, "/WriteChunks") {
		return handler(srv, stream)
	}
	w.active.Add(1)
	defer w.active.Add(-1)
	if stream.Context().Value(chunkStallConnectionKey{}).(int64) != 1 {
		return handler(srv, stream)
	}
	err := handler(srv, &stalledChunkServerStream{ServerStream: stream, wire: w})
	w.mu.Lock()
	if w.firstCancelled.IsZero() {
		w.firstCancelled = time.Now()
	}
	w.mu.Unlock()
	return err
}

// Audit 2086: real TCP/HTTP2, full production watchdog, production RPC service
// and disk staging. This does not claim USB hardware, container execution or
// the separate elevated-user ownership acceptance.
func TestChunkStallTCPResumesGzipFromDiskWithoutDuplicateContent(t *testing.T) {
	t.Setenv("WENDY_CHUNK_UPLOAD_STREAMS", "")
	t.Setenv("WENDY_CHUNK_UPLOAD_BATCH", "")
	manifestCacheTestDir, chunkStallTestDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir, chunkStallTestDir = "", "" })
	restore := forceBuildProgressInteractive(false)
	defer restore()
	restoreOut := setBuildProgressOut(io.Discard)
	defer restoreOut()
	client, closeStore, err := containerd.NewChunkStallFixture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeStore(); err != nil {
			t.Error(err)
		}
	})
	store := &chunkStallStore{Client: client, staged: make(map[[32]byte]int)}
	wire := &chunkStallWire{store: store, encodings: make(map[int64][]string), blocked: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.StatsHandler(wire), grpc.StreamInterceptor(wire.intercept))
	agentpb.RegisterWendyContainerServiceServer(server, services.NewContainerService(zap.NewNop(), store))
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		listener.Close()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Error("TCP server did not stop")
		}
	})
	connect := func() *grpcclient.AgentConnection {
		cc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		return &grpcclient.AgentConnection{Conn: cc, ContainerService: agentpb.NewWendyContainerServiceClient(cc)}
	}
	first := connect()
	var reconnects int
	var stagedBeforeRetry map[[32]byte]int
	first.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		store.mu.Lock()
		stagedBeforeRetry = make(map[[32]byte]int, len(store.staged))
		for hash, count := range store.staged {
			stagedBeforeRetry[hash] = count
		}
		store.mu.Unlock()
		return connect(), nil
	}
	payload := variedChunkTestData(24 << 20)
	refs, err := chunk.Chunk(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	layers := []localLayer{{Digest: "sha256:" + sha256Hex(payload), MediaType: "application/vnd.oci.image.layer.v1.tar", Blob: payload}}
	ctx, cancel := context.WithTimeout(context.Background(), chunkStallTimeout+25*time.Second)
	defer cancel()
	cfg := chunkUploadConfig{stallTimeout: chunkStallTimeout, stallKey: "physical-free-tcp-fixture@0.19.3"}
	var snapshots []chunkPushSnapshot
	returned, headers, err := pushLayersResumingTunnelDrops(ctx, first, layers, nil, cfg,
		func(snapshot chunkPushSnapshot) { snapshots = append(snapshots, snapshot) })
	if err != nil {
		t.Fatalf("production watchdog did not resume blocked TCP upload: %v", err)
	}
	if returned == first || reconnects != 1 || wire.connections.Load() != 2 || len(headers) != 1 {
		t.Fatalf("expected exactly one real reconnect: reconnects=%d TCP=%d headers=%d", reconnects, wire.connections.Load(), len(headers))
	}
	if len(snapshots) != 2 || snapshots[0].Compressor != "" || snapshots[1].Compressor != "gzip" {
		t.Fatalf("blocked transport did not switch to gzip: %+v", snapshots)
	}
	wire.mu.Lock()
	blocked, cancelled := wire.blockedAt, wire.firstCancelled
	encodings := wire.encodings
	wire.mu.Unlock()
	if blocked.IsZero() || cancelled.Sub(blocked) < chunkStallTimeout-time.Second || cancelled.Sub(blocked) > chunkStallTimeout+15*time.Second {
		t.Fatalf("full production stall interval not observed: blocked=%v cancelled=%v delta=%v", blocked, cancelled, cancelled.Sub(blocked))
	}
	for id, expected := range map[int64]string{1: "", 2: "gzip"} {
		if len(encodings[id]) == 0 {
			t.Fatalf("TCP connection %d had no upload streams", id)
		}
		for _, encoding := range encodings[id] {
			if encoding != expected {
				t.Fatalf("wire compression on TCP %d = %q, expected %q", id, encoding, expected)
			}
		}
	}
	if len(stagedBeforeRetry) < 8 || len(stagedBeforeRetry) >= len(refs) {
		t.Fatalf("no real partial durable staging before reconnect: %d/%d", len(stagedBeforeRetry), len(refs))
	}
	order := make([][32]byte, len(refs))
	store.mu.Lock()
	for i, ref := range refs {
		order[i] = ref.Hash
		if store.staged[ref.Hash] != 1 {
			t.Errorf("chunk %x staged %d times; retry must reuse disk content", ref.Hash, store.staged[ref.Hash])
		}
	}
	store.mu.Unlock()
	got, err := io.ReadAll(client.OpenChunkStream(ctx, order))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("production disk readback changed content: length=%d err=%v", len(got), err)
	}
	if headers[0].GetDiffId() != "sha256:"+sha256Hex(payload) || headers[0].GetSize() != int64(len(payload)) {
		t.Fatal("retry returned wrong layer identity")
	}
	if !chunkUploadStalledRecently(cfg.stallKey, time.Now()) {
		t.Fatal("completed retry did not persist gzip preference")
	}
	deadline := time.Now().Add(3 * time.Second)
	for wire.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if wire.active.Load() != 0 {
		t.Fatal("cancelled/finished server upload streams did not drain")
	}
	t.Logf("full production stall recovered over TCP with gzip; exact disk bytes=%d retained chunks=%d streams drained", len(got), len(stagedBeforeRetry))
}
