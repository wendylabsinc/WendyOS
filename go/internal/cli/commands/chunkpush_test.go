package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type chunkProgressRecorder struct {
	io.Writer
	events []tui.BuildStepEvent
}

type imagePreparationRecorder struct {
	io.Writer
	once   sync.Once
	called chan struct{}
}

func variedChunkTestData(n int) []byte {
	data := make([]byte, n)
	state := uint32(0x6d2b79f5)
	for i := range data {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		data[i] = byte(state)
	}
	return data
}

func (r *imagePreparationRecorder) ReportImagePreparation() {
	r.once.Do(func() { close(r.called) })
}

func (r *chunkProgressRecorder) ReportChunkIndex(current, total int64, rate float64, done bool) {
	status := tui.BuildStepRunning
	if done {
		status = tui.BuildStepDone
	}
	r.events = append(r.events, tui.BuildStepEvent{
		Status: status,
		Bytes:  tui.ByteProgress{Current: current, Total: total, Rate: rate},
	})
}

func TestChunkIndexProgressDoesNotReportPartialTotal(t *testing.T) {
	recorder := &chunkProgressRecorder{Writer: io.Discard}
	progress := newChunkIndexProgress(recorder)

	progress.startLayer(100)
	progress.addProcessed(100)
	progress.startLayer(0)
	progress.last = time.Time{} // bypass the live-update throttle for this assertion
	progress.addProcessed(50)

	last := recorder.events[len(recorder.events)-1]
	if last.Bytes.Current != 150 || last.Bytes.Total != 0 {
		t.Fatalf("progress with unknown layer = %d/%d, want 150/unknown", last.Bytes.Current, last.Bytes.Total)
	}

	progress.finishLayer(0, 50)
	last = recorder.events[len(recorder.events)-1]
	if last.Bytes.Current != 150 || last.Bytes.Total != 150 {
		t.Fatalf("progress after sizes resolve = %d/%d, want 150/150", last.Bytes.Current, last.Bytes.Total)
	}
}

func TestComposeChunkProgressKeepsUploadVisible(t *testing.T) {
	var events []tui.BuildStepEvent
	w := &imageBuildProgressWriter{
		Writer: io.Discard,
		emit:   func(e tui.BuildStepEvent) { events = append(events, e) },
	}

	w.ReportChunkIndex(50, 100, 10, false)
	w.ReportChunkTransfer(10, 100, 5)
	w.ReportChunkIndex(75, 100, 10, false)
	w.ReportChunkIndex(100, 100, 10, true)

	if len(events) != 3 {
		t.Fatalf("emitted %d events, want initial index, upload, and index completion", len(events))
	}
	if events[0].Display != "indexing changed layer content" || events[0].Kind != tui.BuildVertexSetup {
		t.Fatalf("first event = %#v, want indexing setup event", events[0])
	}
	if events[1].Display != "uploading missing chunks" || events[1].Status != tui.BuildStepRunning {
		t.Fatalf("second event = %#v, want running upload", events[1])
	}
	if events[2].Status != tui.BuildStepDone {
		t.Fatalf("third event = %#v, want index completion", events[2])
	}
}

func TestPushLayersByChunksRoutesStatusToOutput(t *testing.T) {
	diffID := "sha256:" + strings.Repeat("ab", 32)
	fake := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{
				Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: 4096}},
			}
		},
	}
	var output bytes.Buffer
	_, err := pushLayersByChunksWithPrepareOutput(context.Background(), fake, []localLayer{{
		DiffID: diffID,
	}}, nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Reusing 1 layer(s) already on device; chunking 0.") {
		t.Fatalf("routed status = %q", got)
	}
}

// fakeContainerClient satisfies agentpb.WendyContainerServiceClient via the
// embedded-interface trick. Only QueryChunks, QueryLayers, and WriteChunks are
// overridden; all other methods panic (they must not be called by
// pushLayersByChunks).
type fakeContainerClient struct {
	agentpb.WendyContainerServiceClient // embedded nil — satisfies interface
	queryFn                             func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse
	queryLayersFn                       func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse
	writeFn                             func(*agentpb.WriteChunksRequest) error
	closeErr                            error
	chunksWritten                       int
	writeStreams                        int
	writeStarted                        chan struct{}
	blockWrites                         bool
	writeOnce                           sync.Once
}

// TestPushLayersByChunksPreparesDuringUpload proves the preparation RPC is
// started after manifests are known but before WriteChunks finishes. This is
// the wall-clock overlap the optimization exists to create.
func TestPushLayersByChunksPreparesDuringUpload(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := bytes.Repeat([]byte("prewarm-layer-"), 100_000)
	prepareStarted := make(chan struct{})
	allowPrepare := make(chan struct{})
	var once sync.Once
	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			if len(req.GetChunkHashes()) == 0 {
				return &agentpb.QueryChunksResponse{}
			}
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()[:1]}
		},
		writeFn: func(*agentpb.WriteChunksRequest) error {
			<-prepareStarted
			once.Do(func() { close(allowPrepare) })
			return nil
		},
	}
	prepare := func(_ context.Context, headers []*agentpb.RunContainerLayerHeader) error {
		if len(headers) != 1 || len(headers[0].GetChunkHashes()) == 0 {
			t.Fatalf("prepare received incomplete layer manifests: %#v", headers)
		}
		close(prepareStarted)
		<-allowPrepare
		return nil
	}

	headers, err := pushLayersByChunksWithPrepare(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		DiffID:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}, prepare)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 {
		t.Fatalf("headers = %d, want 1", len(headers))
	}
}

func TestPushLayersByChunksPrepareUnimplementedFallsBack(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := []byte("already available layer")
	fake := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
	}
	_, err := pushLayersByChunksWithPrepare(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
		return status.Error(codes.Unimplemented, "old agent")
	})
	if err != nil {
		t.Fatalf("Unimplemented preparation must fall back to RunContainer, got %v", err)
	}
}

func TestPushLayersByChunksStrictPrepareReturnsUnimplemented(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := []byte("already available layer")
	fake := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
	}
	_, err := pushLayersByChunksWithStrictPrepareOutput(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
		return status.Error(codes.Unimplemented, "old agent")
	}, nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("strict preparation error = %v, want Unimplemented", err)
	}
}

func TestPushLayersByChunksReportsPostUploadPreparation(t *testing.T) {
	diffID := "sha256:" + strings.Repeat("ab", 32)
	fake := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: 1}}}
		},
	}
	release := make(chan struct{})
	recorder := &imagePreparationRecorder{Writer: io.Discard, called: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := pushLayersByChunksWithStrictPrepareOutput(context.Background(), fake, []localLayer{{DiffID: diffID}}, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
			<-release
			return nil
		}, recorder)
		done <- err
	}()

	select {
	case <-recorder.called:
		close(release)
	case <-time.After(time.Second):
		t.Fatal("device preparation progress was not reported after upload completed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPushLayersByChunksStrictPrepareCancelsUploadOnPrepareFailure(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := bytes.Repeat([]byte("cancel-on-prepare-failure-"), 100_000)
	refs, err := chunk.Chunk(bytes.NewReader(layerTar))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(refs))
	}

	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			if len(req.GetChunkHashes()) == 0 {
				return &agentpb.QueryChunksResponse{}
			}
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
		},
		writeStarted: make(chan struct{}),
		blockWrites:  true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err = pushLayersByChunksWithStrictPrepareOutput(ctx, fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}}, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
		<-fake.writeStarted
		return status.Error(codes.Unimplemented, "old agent")
	}, nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("strict preparation error = %v, want Unimplemented", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("prepare failure took %s to cancel chunk upload", elapsed)
	}
	if fake.chunksWritten != 1 {
		t.Fatalf("wrote %d chunks after preparation failed, want 1 in-flight chunk", fake.chunksWritten)
	}
}

func (f *fakeContainerClient) QueryChunks(_ context.Context, in *agentpb.QueryChunksRequest, _ ...grpc.CallOption) (*agentpb.QueryChunksResponse, error) {
	return f.queryFn(in), nil
}

// QueryLayers delegates to queryLayersFn when set; otherwise it reports
// Unimplemented, mirroring an agent too old for the layer pre-check so the push
// degrades to chunking every layer.
func (f *fakeContainerClient) QueryLayers(_ context.Context, in *agentpb.QueryLayersRequest, _ ...grpc.CallOption) (*agentpb.QueryLayersResponse, error) {
	if f.queryLayersFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryLayers not implemented")
	}
	return f.queryLayersFn(in), nil
}

func (f *fakeContainerClient) WriteChunks(ctx context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse], error) {
	f.writeStreams++
	return &fakeWriteChunksStream{parent: f, ctx: ctx}, nil
}

// fakeWriteChunksStream satisfies grpc.ClientStreamingClient via embedding.
type fakeWriteChunksStream struct {
	grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse] // embedded nil
	parent                                                                              *fakeContainerClient
	ctx                                                                                 context.Context
}

func (s *fakeWriteChunksStream) Send(req *agentpb.WriteChunksRequest) error {
	s.parent.chunksWritten++
	if s.parent.writeStarted != nil {
		s.parent.writeOnce.Do(func() { close(s.parent.writeStarted) })
	}
	if s.parent.blockWrites {
		<-s.ctx.Done()
		return s.ctx.Err()
	}
	if s.parent.writeFn != nil {
		return s.parent.writeFn(req)
	}
	return nil
}

func (s *fakeWriteChunksStream) CloseAndRecv() (*agentpb.WriteChunksResponse, error) {
	return &agentpb.WriteChunksResponse{}, s.parent.closeErr
}

func TestPushLayerByChunksSurfacesTerminalWriteStatusAfterSendEOF(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := []byte("one missing chunk")
	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
		},
		writeFn: func(*agentpb.WriteChunksRequest) error { return io.EOF },
		closeErr: status.Error(codes.ResourceExhausted,
			"chunk staging exceeds the device limit"),
	}

	_, err := pushLayerByChunks(context.Background(), fake, localLayer{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("error = %v, want ResourceExhausted terminal status", err)
	}
	if !strings.Contains(err.Error(), "chunk staging exceeds the device limit") {
		t.Fatalf("error = %q, want terminal server detail", err)
	}
}

func TestPushLayerByChunksBatchesLongUploads(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := variedChunkTestData((maxChunksPerWriteStream + 8) * int(chunk.MaxSize))
	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
		},
	}
	if _, err := pushLayerByChunks(context.Background(), fake, localLayer{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.chunksWritten <= maxChunksPerWriteStream {
		t.Fatalf("fixture wrote %d chunks, want more than one %d-chunk batch", fake.chunksWritten, maxChunksPerWriteStream)
	}
	wantStreams := (fake.chunksWritten + maxChunksPerWriteStream - 1) / maxChunksPerWriteStream
	if fake.writeStreams != wantStreams {
		t.Fatalf("WriteChunks streams = %d, want %d for %d chunks", fake.writeStreams, wantStreams, fake.chunksWritten)
	}
}

func TestResolvedChunkLayerUploadSendsDuplicateHashOnce(t *testing.T) {
	chunkData := []byte("the same content appears twice")
	f, err := os.CreateTemp(t.TempDir(), "duplicate-layer-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(append([]byte(nil), chunkData...), chunkData...)); err != nil {
		t.Fatal(err)
	}
	dl := &decompressedLayer{f: f, size: int64(2 * len(chunkData)), diffID: "sha256:duplicate-layer"}
	t.Cleanup(dl.Close)
	h := sha256.Sum256(chunkData)
	resolved := &resolvedChunkLayer{
		header: &agentpb.RunContainerLayerHeader{
			DiffId:      dl.diffID,
			ChunkHashes: [][]byte{h[:], h[:]},
		},
		dl: dl,
		refs: []chunk.Ref{
			{Hash: h, Offset: 0, Len: uint64(len(chunkData))},
			{Hash: h, Offset: uint64(len(chunkData)), Len: uint64(len(chunkData))},
		},
	}
	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
		},
	}
	progress := newChunkPushProgress()
	if err := resolved.upload(context.Background(), fake, nil, nil, progress); err != nil {
		t.Fatal(err)
	}
	if fake.chunksWritten != 1 {
		t.Fatalf("device received %d copies of one missing hash, want 1", fake.chunksWritten)
	}
	snapshot := progress.Snapshot()
	if snapshot.TotalChunks != 1 || snapshot.MissingChunks != 1 || snapshot.SentChunks != 1 {
		t.Fatalf("progress total/missing/sent = %d/%d/%d, want 1/1/1", snapshot.TotalChunks, snapshot.MissingChunks, snapshot.SentChunks)
	}
	if snapshot.PlannedBytes != int64(len(chunkData)) || snapshot.SentBytes != int64(len(chunkData)) {
		t.Fatalf("progress planned/sent bytes = %d/%d, want %d/%d", snapshot.PlannedBytes, snapshot.SentBytes, len(chunkData), len(chunkData))
	}
}

func TestPushLayersByChunksWritesOnlyMissing(t *testing.T) {
	// Isolate the manifest cache so the test neither reads nor pollutes the
	// real user cache (and starts from a guaranteed cache miss).
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := variedChunkTestData(900_000) // multi-chunk with distinct hashes
	refs, err := chunk.Chunk(bytes.NewReader(layerTar))
	if err != nil {
		t.Fatalf("chunk.Chunk: %v", err)
	}
	if len(refs) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(refs))
	}

	// Fake device already has every chunk except the first.
	have := map[[32]byte]bool{}
	for _, r := range refs[1:] {
		have[r.Hash] = true
	}
	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			var missing [][]byte
			for _, hb := range req.GetChunkHashes() {
				var h [32]byte
				copy(h[:], hb)
				if !have[h] {
					missing = append(missing, hb)
				}
			}
			return &agentpb.QueryChunksResponse{MissingHashes: missing}
		},
	}
	// An uncompressed layer: the blob bytes are the raw tar, so decompress()
	// returns them as-is. Digest is the (compressed==uncompressed) blob digest.
	headers, err := pushLayersByChunks(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex(layerTar),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      layerTar,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.chunksWritten; got != 1 {
		t.Fatalf("expected exactly 1 chunk written, got %d", got)
	}
	if len(headers) != 1 || len(headers[0].GetChunkHashes()) != len(refs) {
		t.Fatalf("header manifest wrong: got %d headers, chunk hashes %d (want %d)", len(headers), func() int {
			if len(headers) > 0 {
				return len(headers[0].GetChunkHashes())
			}
			return 0
		}(), len(refs))
	}
	if headers[0].GetCompression() != agentpb.RunContainerLayerHeader_COMPRESSION_NONE {
		t.Fatalf("layer must be uncompressed, got %v", headers[0].GetCompression())
	}
}

// TestPushLayersByChunksSkipsPresentLayer verifies that a layer the device
// already has (reported by QueryLayers) is never decompressed, chunked, or
// transferred — its header is built from the diff ID and the device-reported
// size alone.
func TestPushLayersByChunksSkipsPresentLayer(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	diffID := "sha256:" + strings.Repeat("ab", 32)
	const presentSize int64 = 4096

	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			// The only legitimate QueryChunks here is the empty capability probe.
			if len(req.GetChunkHashes()) != 0 {
				t.Errorf("QueryChunks called with %d hashes; a present layer must not be chunked", len(req.GetChunkHashes()))
			}
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(req *agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{
				Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: presentSize}},
			}
		},
	}

	// The blob is intentionally NOT valid gzip: if the push tried to decompress
	// this layer it would fail, proving the present layer was skipped.
	headers, err := pushLayersByChunks(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex([]byte("compressed-bytes")),
		DiffID:    diffID,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Blob:      []byte("this is not gzip"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if fake.chunksWritten != 0 {
		t.Fatalf("expected 0 chunks written for a present layer, got %d", fake.chunksWritten)
	}
	if len(headers) != 1 {
		t.Fatalf("expected 1 header, got %d", len(headers))
	}
	h := headers[0]
	if h.GetDiffId() != diffID || h.GetDigest() != diffID {
		t.Fatalf("present-layer header digest/diffID mismatch: digest=%q diffID=%q want %q", h.GetDigest(), h.GetDiffId(), diffID)
	}
	if h.GetSize() != presentSize {
		t.Fatalf("present-layer header size = %d, want %d (device-reported)", h.GetSize(), presentSize)
	}
	if len(h.GetChunkHashes()) != 0 {
		t.Fatalf("present-layer header must carry no chunk hashes, got %d", len(h.GetChunkHashes()))
	}
	if h.GetCompression() != agentpb.RunContainerLayerHeader_COMPRESSION_NONE {
		t.Fatalf("present-layer header must be uncompressed, got %v", h.GetCompression())
	}
}

func TestPushLayersByChunksOverlapsRemotePreflightAndLocalCacheReads(t *testing.T) {
	diffID := "sha256:" + strings.Repeat("ef", 32)
	capabilityStarted := make(chan struct{})
	layersStarted := make(chan struct{})
	cacheStarted := make(chan struct{})
	capabilityRelease := make(chan struct{})
	layersRelease := make(chan struct{})
	cacheRelease := make(chan struct{})
	var capabilityOnce, layersOnce, cacheOnce sync.Once
	releaseCapability := func() { capabilityOnce.Do(func() { close(capabilityRelease) }) }
	releaseLayers := func() { layersOnce.Do(func() { close(layersRelease) }) }
	releaseCache := func() { cacheOnce.Do(func() { close(cacheRelease) }) }
	defer releaseCapability()
	defer releaseLayers()
	defer releaseCache()

	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			if len(req.GetChunkHashes()) != 0 {
				t.Fatalf("unexpected non-capability QueryChunks call")
			}
			close(capabilityStarted)
			<-capabilityRelease
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			close(layersStarted)
			<-layersRelease
			return &agentpb.QueryLayersResponse{Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: 123}}}
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := pushLayersByChunksWithPrepareModeAndCache(
			context.Background(), fake, []localLayer{{Digest: "sha256:cached", DiffID: diffID}},
			nil, nil, false, nil,
			func(string) (*cachedManifest, bool) {
				close(cacheStarted)
				<-cacheRelease
				return nil, false
			},
		)
		done <- err
	}()

	for name, started := range map[string]<-chan struct{}{
		"capability probe": capabilityStarted,
		"layer query":      layersStarted,
		"manifest cache":   cacheStarted,
	} {
		select {
		case <-started:
		case <-time.After(time.Second):
			releaseCapability()
			releaseLayers()
			releaseCache()
			t.Fatalf("%s did not start while the other preflight operations were blocked", name)
		}
	}
	releaseCapability()
	releaseLayers()
	releaseCache()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPushLayersByChunksProbeUnimplemented verifies that an agent which does not
// support chunk-diff at all (QueryChunks returns Unimplemented) surfaces the
// error before any layer materialization, so the caller can fall back to a
// registry push. The optional QueryLayers request may run concurrently.
func TestPushLayersByChunksProbeUnimplemented(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	fake := &probeUnsupportedClient{}
	_, err := pushLayersByChunks(context.Background(), fake, []localLayer{{
		Digest:    "sha256:" + sha256Hex([]byte("x")),
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Blob:      []byte("not gzip either"),
	}})
	if !isUnimplementedRPCError(err) {
		t.Fatalf("expected Unimplemented error from the capability probe, got %v", err)
	}
}

// probeUnsupportedClient fails QueryChunks with Unimplemented, modelling an agent
// too old for any chunk-diff support.
type probeUnsupportedClient struct {
	agentpb.WendyContainerServiceClient
}

func (probeUnsupportedClient) QueryChunks(_ context.Context, _ *agentpb.QueryChunksRequest, _ ...grpc.CallOption) (*agentpb.QueryChunksResponse, error) {
	return nil, status.Error(codes.Unimplemented, "QueryChunks not implemented")
}

func (probeUnsupportedClient) QueryLayers(_ context.Context, _ *agentpb.QueryLayersRequest, _ ...grpc.CallOption) (*agentpb.QueryLayersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "QueryLayers not implemented")
}

// TestPushLayersByChunksReportsProgress verifies that pushLayersByChunks wires
// a non-nil *chunkPushProgress into the transfer loop: one layer is reused
// whole (skipped by the QueryLayers pre-check) and one layer needs chunking
// with exactly one missing chunk, so the resulting snapshot should show the
// reused layer plus the chunked layer's full manifest size, missing count,
// and the sent chunk's exact byte length.
func TestPushLayersByChunksReportsProgress(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	layerTar := variedChunkTestData(900_000) // multi-chunk with distinct hashes
	refs, err := chunk.Chunk(bytes.NewReader(layerTar))
	if err != nil {
		t.Fatalf("chunk.Chunk: %v", err)
	}
	if len(refs) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(refs))
	}

	// Fake device already has every chunk except the first.
	have := map[[32]byte]bool{}
	for _, r := range refs[1:] {
		have[r.Hash] = true
	}

	reusedDiffID := "sha256:" + strings.Repeat("cd", 32)
	const reusedSize int64 = 2048

	fake := &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			var missing [][]byte
			for _, hb := range req.GetChunkHashes() {
				var h [32]byte
				copy(h[:], hb)
				if !have[h] {
					missing = append(missing, hb)
				}
			}
			return &agentpb.QueryChunksResponse{MissingHashes: missing}
		},
		queryLayersFn: func(req *agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{
				Present: []*agentpb.PresentLayer{{DiffId: reusedDiffID, Size: reusedSize}},
			}
		},
	}

	prog := newChunkPushProgress()
	_, err = pushLayersByChunksWithPrepareMode(context.Background(), fake, []localLayer{
		{
			// Present layer: skipped whole by the pre-check. Blob is
			// intentionally not valid gzip so a decompress attempt would fail.
			Digest:    "sha256:" + sha256Hex([]byte("reused-blob")),
			DiffID:    reusedDiffID,
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Blob:      []byte("this is not gzip"),
		},
		{
			Digest:    "sha256:" + sha256Hex(layerTar),
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Blob:      layerTar,
		},
	}, nil, nil, false, prog)
	if err != nil {
		t.Fatal(err)
	}

	snap := prog.Snapshot()
	if snap.LayersTotal != 2 {
		t.Fatalf("LayersTotal = %d, want 2", snap.LayersTotal)
	}
	if snap.LayersReused != 1 {
		t.Fatalf("LayersReused = %d, want 1", snap.LayersReused)
	}
	if snap.TotalChunks != len(refs) {
		t.Fatalf("TotalChunks = %d, want %d", snap.TotalChunks, len(refs))
	}
	if snap.MissingChunks != 1 {
		t.Fatalf("MissingChunks = %d, want 1", snap.MissingChunks)
	}
	if snap.SentChunks != 1 {
		t.Fatalf("SentChunks = %d, want 1", snap.SentChunks)
	}
	if snap.SentBytes != int64(refs[0].Len) {
		t.Fatalf("SentBytes = %d, want %d", snap.SentBytes, refs[0].Len)
	}
}

// TestPushLayersByChunksReuseLineSkipsInteractive verifies the mid-push
// "Reusing N layer(s)..." status line is suppressed while the interactive
// chunk-push progress bar is live: cliLogln writes straight to os.Stderr, and
// so does the live Bubble Tea program in pushLayersWithProgress, so printing
// it there garbles the bar on every deploy with layer reuse (finding 3,
// WDY-2432/2433 final-review fix wave). The aggregator's Snapshot().Line()
// (ticker) and Summary() (printed once the TUI exits) already carry the same
// reused-layer count, so nothing is lost. The non-interactive/plain path is
// unaffected — nothing else renders to stderr there — so it keeps the line.
func TestPushLayersByChunksReuseLineSkipsInteractive(t *testing.T) {
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	diffID := "sha256:" + strings.Repeat("ab", 32)
	const presentSize int64 = 4096

	newFake := func() *fakeContainerClient {
		return &fakeContainerClient{
			queryFn: func(_ *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
				return &agentpb.QueryChunksResponse{}
			},
			queryLayersFn: func(_ *agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
				return &agentpb.QueryLayersResponse{
					Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: presentSize}},
				}
			},
		}
	}
	// A single present layer: the whole push resolves via the layer pre-check
	// (skipped > 0), which is exactly the case that fires the "Reusing" line.
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex([]byte("compressed-bytes")),
		DiffID:    diffID,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Blob:      []byte("this is not gzip"),
	}}

	t.Run("interactive: suppressed", func(t *testing.T) {
		restore := forceBuildProgressInteractive(true)
		defer restore()
		out := captureStderr(t, func() {
			if _, err := pushLayersByChunksWithPrepareMode(context.Background(), newFake(), layers, nil, nil, false, newChunkPushProgress()); err != nil {
				t.Fatal(err)
			}
		})
		if strings.Contains(out, "Reusing") {
			t.Fatalf("interactive mode must not print the mid-push reuse line (races the live progress bar), got %q", out)
		}
	})

	t.Run("non-interactive/plain: unchanged", func(t *testing.T) {
		restore := forceBuildProgressInteractive(false)
		defer restore()
		out := captureStderr(t, func() {
			if _, err := pushLayersByChunksWithPrepareMode(context.Background(), newFake(), layers, nil, nil, false, newChunkPushProgress()); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "Reusing") {
			t.Fatalf("non-interactive/plain mode should keep the reuse line (nothing else renders to stderr there), got %q", out)
		}
	})
}
