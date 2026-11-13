package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"math/rand"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	grpcgzip "google.golang.org/grpc/encoding/gzip"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunkupload"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeContainerd embeds mockContainerdClient and adds hooks for MissingChunks
// and StageChunk so chunk-related tests can inject custom behaviour without
// touching the shared mock.
type fakeContainerd struct {
	mockContainerdClient
	missingFn    func(ctx context.Context, hashes [][32]byte) ([][32]byte, error)
	presentFn    func(ctx context.Context, diffIDs []string) (map[string]int64, error)
	stageChunkFn func(ctx context.Context, h [32]byte, data []byte) error

	stageMu      sync.Mutex // WriteChunks streams run concurrently
	stagedChunks []stagedChunk
}

type stagedChunk struct {
	hash [32]byte
	data []byte
}

func newFakeContainerd() *fakeContainerd {
	return &fakeContainerd{}
}

// MissingChunks delegates to missingFn when set; otherwise returns all hashes unchanged.
func (f *fakeContainerd) MissingChunks(ctx context.Context, hashes [][32]byte) ([][32]byte, error) {
	if f.missingFn != nil {
		return f.missingFn(ctx, hashes)
	}
	return hashes, nil
}

// PresentLayers delegates to presentFn when set; otherwise reports nothing present.
func (f *fakeContainerd) PresentLayers(ctx context.Context, diffIDs []string) (map[string]int64, error) {
	if f.presentFn != nil {
		return f.presentFn(ctx, diffIDs)
	}
	return nil, nil
}

// StageChunk records the (hash, data) pair and delegates to stageChunkFn when set.
func (f *fakeContainerd) StageChunk(ctx context.Context, h [32]byte, data []byte) error {
	f.stageMu.Lock()
	f.stagedChunks = append(f.stagedChunks, stagedChunk{hash: h, data: data})
	f.stageMu.Unlock()
	if f.stageChunkFn != nil {
		return f.stageChunkFn(ctx, h, data)
	}
	return nil
}

func (f *fakeContainerd) GetResourceStats(context.Context) ([]*agentpb.ResourceContainerStats, error) {
	return nil, nil
}
func (f *fakeContainerd) GetListeningPorts(context.Context, string) ([]*agentpb.PortEntry, error) {
	return nil, nil
}

func TestQueryChunksReturnsMissing(t *testing.T) {
	fake := newFakeContainerd()
	fake.missingFn = func(_ context.Context, hs [][32]byte) ([][32]byte, error) {
		return hs[1:], nil // pretend the first is present
	}
	svc := NewContainerService(zap.NewNop(), fake)

	h0 := bytes.Repeat([]byte{0}, 32)
	h1 := bytes.Repeat([]byte{1}, 32)
	resp, err := svc.QueryChunks(context.Background(), &agentpb.QueryChunksRequest{
		ChunkHashes: [][]byte{h0, h1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetMissingHashes()) != 1 || !bytes.Equal(resp.GetMissingHashes()[0], h1) {
		t.Fatalf("expected only h1 missing, got %v", resp.GetMissingHashes())
	}
}

func TestQueryLayersReportsPresent(t *testing.T) {
	fake := newFakeContainerd()
	fake.presentFn = func(_ context.Context, diffIDs []string) (map[string]int64, error) {
		// Pretend only the first requested layer is present, with a known size.
		if len(diffIDs) == 0 {
			return nil, nil
		}
		return map[string]int64{diffIDs[0]: 4096}, nil
	}
	svc := NewContainerService(zap.NewNop(), fake)

	resp, err := svc.QueryLayers(context.Background(), &agentpb.QueryLayersRequest{
		DiffIds: []string{"sha256:aaa", "sha256:bbb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetPresent()) != 1 {
		t.Fatalf("expected 1 present layer, got %d", len(resp.GetPresent()))
	}
	if got := resp.GetPresent()[0]; got.GetDiffId() != "sha256:aaa" || got.GetSize() != 4096 {
		t.Fatalf("present layer mismatch: got %q size %d", got.GetDiffId(), got.GetSize())
	}
}

// writeChunksStream is a fake grpc.ClientStreamingServer for unit-testing WriteChunks.
type writeChunksStream struct {
	grpc.ServerStream
	msgs []*agentpb.WriteChunksRequest
	pos  int
	sent *agentpb.WriteChunksResponse
	ctx  context.Context
}

func (s *writeChunksStream) Context() context.Context { return s.ctx }
func (s *writeChunksStream) Recv() (*agentpb.WriteChunksRequest, error) {
	if s.pos >= len(s.msgs) {
		return nil, io.EOF
	}
	m := s.msgs[s.pos]
	s.pos++
	return m, nil
}
func (s *writeChunksStream) SendAndClose(r *agentpb.WriteChunksResponse) error {
	s.sent = r
	return nil
}

func TestWriteChunks(t *testing.T) {
	fake := newFakeContainerd()
	svc := NewContainerService(zap.NewNop(), fake)

	var h0 [32]byte
	copy(h0[:], bytes.Repeat([]byte{0x11}, 32))
	data0 := []byte("hello-chunk-data")

	h0bytes := h0[:]
	stream := &writeChunksStream{
		ctx: context.Background(),
		msgs: []*agentpb.WriteChunksRequest{
			{Hash: h0bytes, Data: data0},
		},
	}
	if err := svc.WriteChunks(stream); err != nil {
		t.Fatal(err)
	}
	if stream.sent == nil {
		t.Fatal("SendAndClose was never called")
	}

	// Assert that the server actually staged the chunk with the correct hash and data.
	if len(fake.stagedChunks) != 1 {
		t.Fatalf("expected 1 staged chunk, got %d", len(fake.stagedChunks))
	}
	if fake.stagedChunks[0].hash != h0 {
		t.Fatalf("staged hash mismatch: got %x, want %x", fake.stagedChunks[0].hash, h0)
	}
	if !bytes.Equal(fake.stagedChunks[0].data, data0) {
		t.Fatalf("staged data mismatch: got %q, want %q", fake.stagedChunks[0].data, data0)
	}
}

func TestWriteChunksAcceptsGzipOverGRPC(t *testing.T) {
	fake := newFakeContainerd()
	client, cleanup := startContainerServer(t, fake)
	defer cleanup()

	data := bytes.Repeat([]byte("compressed-chunk-payload"), 4096)
	hash := sha256.Sum256(data)
	stream, err := client.WriteChunks(context.Background(), grpc.UseCompressor(grpcgzip.Name))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpb.WriteChunksRequest{Hash: hash[:], Data: data}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if len(fake.stagedChunks) != 1 {
		t.Fatalf("staged chunks = %d, want 1", len(fake.stagedChunks))
	}
	if fake.stagedChunks[0].hash != hash || !bytes.Equal(fake.stagedChunks[0].data, data) {
		t.Fatal("gzip WriteChunks did not preserve the original hash and data")
	}
}

// TestWriteChunksAcceptsParallelChunkUploads drives the shared uploader against
// the real WriteChunks handler over gRPC, with and without gzip, using more
// streams than one. Every chunk must be staged with its original bytes.
func TestWriteChunksAcceptsParallelChunkUploads(t *testing.T) {
	for _, compressor := range []string{chunkupload.Gzip, ""} {
		t.Run("compressor="+compressor, func(t *testing.T) {
			fake := newFakeContainerd()
			client, cleanup := startContainerServer(t, fake)
			defer cleanup()

			layer := make([]byte, 1<<20)
			rand.New(rand.NewSource(7)).Read(layer)
			refs, err := chunk.ChunkBytes(layer)
			if err != nil {
				t.Fatal(err)
			}
			missing := make(map[[32]byte]bool, len(refs))
			for _, r := range refs {
				missing[r.Hash] = true
			}
			plan, _ := chunkupload.Plan(refs, missing)
			if err := chunkupload.Upload(context.Background(), client, bytes.NewReader(layer), plan, chunkupload.Options{
				Layer: "sha256:parallel", BatchChunks: 2, Streams: 4, Compressor: compressor,
			}); err != nil {
				t.Fatal(err)
			}

			staged := map[[32]byte][]byte{}
			for _, sc := range fake.stagedChunks {
				staged[sc.hash] = sc.data
			}
			for _, r := range plan {
				if !bytes.Equal(staged[r.Hash], layer[r.Offset:r.Offset+r.Len]) {
					t.Fatalf("chunk at %d was not staged with its original bytes", r.Offset)
				}
			}
		})
	}
}
