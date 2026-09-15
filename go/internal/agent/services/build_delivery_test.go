package services

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// fakeTargetAgent stands in for the target device's container service: a
// chunk store keyed by hash, the layers it already holds, and knobs to behave
// like an old or flaky agent. It implements only what delivery calls.
type fakeTargetAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer

	mu           sync.Mutex
	chunks       map[[32]byte][]byte
	layers       map[string]int64 // diff ID -> size, for QueryLayers
	prepared     []*agentpb.RunContainerLayersRequest
	written      int   // chunks received over every stream
	writtenBytes int64 // bytes of chunk data received over every stream
	streams      int   // WriteChunks streams opened

	noChunks   bool  // QueryChunks answers Unimplemented: an agent with no chunk store
	noPrepare  bool  // PrepareImage answers Unimplemented: an agent that cannot register by name
	prepareErr error // PrepareImage fails with this immediately
	noLayers   bool  // QueryLayers answers Unimplemented: an agent that cannot say which layers it holds
	// dropAfter, when > 0, fails the WriteChunks stream that receives the Nth
	// chunk overall with Unavailable, once — a link dying mid-transfer.
	dropAfter int
	dropped   bool
}

func newFakeTargetAgent() *fakeTargetAgent {
	return &fakeTargetAgent{chunks: map[[32]byte][]byte{}, layers: map[string]int64{}}
}

func (f *fakeTargetAgent) QueryChunks(_ context.Context, req *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	if f.noChunks {
		return nil, status.Error(codes.Unimplemented, "unknown method QueryChunks")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var missing [][]byte
	for _, hb := range req.GetChunkHashes() {
		var h [32]byte
		copy(h[:], hb)
		if _, ok := f.chunks[h]; !ok {
			missing = append(missing, hb)
		}
	}
	return &agentpb.QueryChunksResponse{MissingHashes: missing}, nil
}

func (f *fakeTargetAgent) QueryLayers(_ context.Context, req *agentpb.QueryLayersRequest) (*agentpb.QueryLayersResponse, error) {
	if f.noLayers {
		return nil, status.Error(codes.Unimplemented, "unknown method QueryLayers")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &agentpb.QueryLayersResponse{}
	for _, id := range req.GetDiffIds() {
		if size, ok := f.layers[id]; ok {
			resp.Present = append(resp.Present, &agentpb.PresentLayer{DiffId: id, Size: size})
		}
	}
	return resp, nil
}

func (f *fakeTargetAgent) WriteChunks(stream grpc.ClientStreamingServer[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]) error {
	f.mu.Lock()
	f.streams++
	f.mu.Unlock()
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
		if sha256.Sum256(msg.GetData()) != h {
			return status.Error(codes.InvalidArgument, "staged chunk hash mismatch")
		}
		f.mu.Lock()
		f.chunks[h] = msg.GetData()
		f.written++
		f.writtenBytes += int64(len(msg.GetData()))
		drop := f.dropAfter > 0 && !f.dropped && f.written >= f.dropAfter
		if drop {
			f.dropped = true
		}
		f.mu.Unlock()
		if drop {
			return status.Error(codes.Unavailable, "error reading from server: EOF")
		}
	}
}

func (f *fakeTargetAgent) PrepareImage(ctx context.Context, req *agentpb.RunContainerLayersRequest) (*agentpb.PrepareImageResponse, error) {
	if f.noPrepare {
		return nil, status.Error(codes.Unimplemented, "unknown method PrepareImage")
	}
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	// Like the real agent: block until every chunk the headers reference has
	// landed, then register the image.
	for !f.hasAllChunks(req.GetLayers()) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, req)
	for _, l := range req.GetLayers() {
		f.layers[l.GetDiffId()] = l.GetSize()
	}
	return &agentpb.PrepareImageResponse{}, nil
}

func (f *fakeTargetAgent) hasAllChunks(layers []*agentpb.RunContainerLayerHeader) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range layers {
		for _, hb := range l.GetChunkHashes() {
			var h [32]byte
			copy(h[:], hb)
			if _, ok := f.chunks[h]; !ok {
				return false
			}
		}
	}
	return true
}

func (f *fakeTargetAgent) stage(refs []chunk.Ref, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range refs {
		f.chunks[r.Hash] = content[r.Offset : r.Offset+r.Len]
	}
}

func (f *fakeTargetAgent) bytesWritten() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writtenBytes
}

func (f *fakeTargetAgent) snapshot() (written, streams int, prepared []*agentpb.RunContainerLayersRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, f.streams, append([]*agentpb.RunContainerLayersRequest(nil), f.prepared...)
}

// serveFakeTargets runs one gRPC server per asset id over bufconn and returns a
// targetDialer that routes by the push target's asset id, the way the mesh
// dialer would.
func serveFakeTargets(t *testing.T, fakes map[int32]*fakeTargetAgent) targetDialer {
	t.Helper()
	listeners := make(map[int32]*bufconn.Listener, len(fakes))
	for assetID, fake := range fakes {
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		agentpb.RegisterWendyContainerServiceServer(srv, fake)
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
		listeners[assetID] = lis
	}
	return func(ctx context.Context, target *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		lis, ok := listeners[target.GetAssetId()]
		if !ok {
			return nil, fmt.Errorf("no fake device %d", target.GetAssetId())
		}
		return grpc.NewClient("passthrough:///fake-target",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

func noDeliveryBackoff(t *testing.T) {
	t.Helper()
	original := deliveryBackoff
	deliveryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { deliveryBackoff = original })
}

// deliveryFixture writes the standard two-layer test image and reads it back
// the way BuildImage does.
func deliveryFixture(t *testing.T) (*exportedImage, testImage) {
	t.Helper()
	path := fixtureTarPath(t)
	ti := writeTestOCITar(t, path, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64"})
	img, err := readExportedImage(path, "linux/arm64")
	if err != nil {
		t.Fatalf("readExportedImage: %v", err)
	}
	return img, ti
}

func fixtureTarPath(t *testing.T) string {
	return t.TempDir() + "/image.tar"
}

func deliveryService(t *testing.T, dial targetDialer) (*BuildService, *stubBuildStream) {
	t.Helper()
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{ConfigPath: enabledConfigDir(t), StateDir: t.TempDir()})
	svc.dialTarget = dial
	return svc, &stubBuildStream{}
}

// freshResolvedLayers is the per-build cache of decompressed layers that
// BuildImage owns and deliverByChunks fills, for a test that delivers to one
// device and does not care about reuse.
func freshResolvedLayers(t *testing.T) map[string]*deliveryLayer {
	t.Helper()
	resolved := map[string]*deliveryLayer{}
	t.Cleanup(func() { closeDeliveryLayers(resolved) })
	return resolved
}

func chunkCount(t *testing.T, layers []testImageLayer) int {
	t.Helper()
	total := 0
	for _, l := range layers {
		refs, err := chunk.ChunkBytes(l.content)
		if err != nil {
			t.Fatal(err)
		}
		total += len(refs)
	}
	return total
}

func scratchFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "deliver-layer-") {
			names = append(names, e.Name())
		}
	}
	return names
}

func progressLines(s *stubBuildStream) []string {
	var lines []string
	for _, ev := range s.sent {
		if l := ev.GetLogLine(); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func containsLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// The core property: only what the device lacks crosses the link, and the image
// is registered under the name the CLI will create the container from.
func TestDeliverByChunks_SendsOnlyMissingChunksAndRegistersImage(t *testing.T) {
	img, ti := deliveryFixture(t)
	layers := testImageLayers()

	fake := newFakeTargetAgent()
	// Layer 0 is already on the device in full; half of layer 1's chunks are
	// staged from an interrupted earlier deploy.
	fake.layers[ti.diffIDs[0]] = int64(len(layers[0].content))
	refs1, err := chunk.ChunkBytes(layers[1].content)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs1) < 2 {
		t.Fatalf("fixture layer 1 chunks into %d chunk(s); need several to test a partial transfer", len(refs1))
	}
	staged := refs1[:len(refs1)/2]
	fake.stage(staged, layers[1].content)

	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))
	target := &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}
	if err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, target, freshResolvedLayers(t)); err != nil {
		t.Fatalf("deliverByChunks: %v", err)
	}

	written, _, prepared := fake.snapshot()
	if want := len(refs1) - len(staged); written != want {
		t.Fatalf("device received %d chunks, want exactly the %d it lacked: a present layer or a staged chunk must never be re-sent", written, want)
	}
	if len(prepared) != 1 {
		t.Fatalf("PrepareImage called %d times, want 1", len(prepared))
	}
	req := prepared[0]
	if got := req.GetImageName(); got != "localhost:5000/myapp:latest" {
		t.Fatalf("image registered as %q, want localhost:5000/myapp:latest — the name the CLI's CreateContainer asks for", got)
	}
	if string(req.GetImageConfig()) != string(ti.config) {
		t.Fatal("the image config must reach the device unchanged, or Cmd/Env are lost in reassembly")
	}
	if len(req.GetLayers()) != 2 {
		t.Fatalf("got %d layer headers, want 2", len(req.GetLayers()))
	}
	if h := req.GetLayers()[0]; len(h.GetChunkHashes()) != 0 || h.GetSize() != int64(len(layers[0].content)) || h.GetDiffId() != ti.diffIDs[0] {
		t.Fatalf("present layer header = %v; want no chunk hashes and the device's own size, so the device reuses its blob", h)
	}
	if h := req.GetLayers()[1]; len(h.GetChunkHashes()) != len(refs1) || h.GetDiffId() != ti.diffIDs[1] || h.GetCompression() != agentpb.RunContainerLayerHeader_COMPRESSION_NONE {
		t.Fatalf("pushed layer header = %v; want every chunk hash in order, uncompressed", h)
	}
	for i, ref := range refs1 {
		if string(req.GetLayers()[1].GetChunkHashes()[i]) != string(ref.Hash[:]) {
			t.Fatalf("chunk hash %d out of order", i)
		}
	}
	lines := progressLines(stream)
	if !containsLine(lines, "#900 pushing layers to device 214 by chunks") {
		t.Fatalf("progress must open a BuildKit-style vertex the CLI renderer can show; got %q", lines)
	}
	if !containsLine(lines, "#900 DONE") {
		t.Fatalf("progress must close its vertex; got %q", lines)
	}
}

// The whole point of WDY-2605: a link that drops mid-transfer costs a resume,
// not a restart — nothing the device already staged crosses the link again.
func TestDeliverByChunks_ResumesAfterTransportDropWithoutResending(t *testing.T) {
	noDeliveryBackoff(t)
	img, _ := deliveryFixture(t)
	total := 0
	for _, l := range testImageLayers() {
		refs, err := chunk.ChunkBytes(l.content)
		if err != nil {
			t.Fatal(err)
		}
		total += len(refs)
	}

	fake := newFakeTargetAgent()
	fake.dropAfter = 2
	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))
	target := &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}
	if err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, target, freshResolvedLayers(t)); err != nil {
		t.Fatalf("deliverByChunks should have resumed past one drop, got: %v", err)
	}

	written, streams, prepared := fake.snapshot()
	if !fake.dropped {
		t.Fatal("the fake never dropped the link; the test did not exercise resume")
	}
	if written != total {
		t.Fatalf("device received %d chunks for an image of %d: a resume must skip staged chunks, not start over", written, total)
	}
	if streams < 2 {
		t.Fatalf("only %d WriteChunks stream(s) opened; a resume opens a fresh one", streams)
	}
	if len(prepared) != 1 {
		t.Fatalf("the image was registered %d times, want once by the attempt that completed", len(prepared))
	}
	if lines := progressLines(stream); !containsLine(lines, "resuming") {
		t.Fatalf("the CLI must be told a resume happened; got %q", lines)
	}
}

func TestUploadLayerChunks_SendsDuplicateHashOnce(t *testing.T) {
	chunkData := []byte("the same content appears twice")
	layerData := append(append([]byte(nil), chunkData...), chunkData...)
	path := t.TempDir() + "/layer.tar"
	if err := os.WriteFile(path, layerData, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	h := sha256.Sum256(chunkData)
	dl := &deliveryLayer{
		header: &agentpb.RunContainerLayerHeader{
			DiffId:      "sha256:duplicate-layer",
			ChunkHashes: [][]byte{h[:], h[:]},
		},
		refs: []chunk.Ref{
			{Hash: h, Offset: 0, Len: uint64(len(chunkData))},
			{Hash: h, Offset: uint64(len(chunkData)), Len: uint64(len(chunkData))},
		},
		f: f,
	}

	fake := newFakeTargetAgent()
	dial := serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake})
	conn, err := dial(context.Background(), &agentpbv2.PushTarget{AssetId: 214})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, stream := deliveryService(t, nil)
	rep := newDeliveryReporter(&buildProgress{stream: stream}, 0, 214)
	if err := uploadLayerChunks(context.Background(), agentpb.NewWendyContainerServiceClient(conn), dl, rep); err != nil {
		t.Fatal(err)
	}

	written, _, _ := fake.snapshot()
	if written != 1 {
		t.Fatalf("device received %d copies of one missing hash, want 1", written)
	}
	if got := fake.bytesWritten(); got != int64(len(chunkData)) {
		t.Fatalf("device received %d bytes, want one %d-byte chunk", got, len(chunkData))
	}
	if got := rep.sentBytes(); got != int64(len(chunkData)) {
		t.Fatalf("reported %d sent bytes, want %d", got, len(chunkData))
	}
}

func TestDeliveryTargetAgentPortUsesEachTargetsPort(t *testing.T) {
	tests := []struct {
		name     string
		target   *agentpbv2.PushTarget
		fallback uint16
		want     uint16
		wantErr  bool
	}{
		{name: "target override", target: &agentpbv2.PushTarget{AssetId: 1, AgentPort: 51002}, fallback: 50052, want: 51002},
		{name: "legacy fallback", target: &agentpbv2.PushTarget{AssetId: 2}, fallback: 50052, want: 50052},
		{name: "out of range", target: &agentpbv2.PushTarget{AssetId: 3, AgentPort: 70000}, fallback: 50052, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deliveryTargetAgentPort(tt.target, tt.fallback)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("port = %d, want %d", got, tt.want)
			}
		})
	}
}

// A device whose agent has no chunk store is the fallback case, recognised
// before any layer is decompressed or any byte sent.
func TestDeliverByChunks_ReportsUnsupportedAgentBeforeSendingAnything(t *testing.T) {
	img, _ := deliveryFixture(t)
	fake := newFakeTargetAgent()
	fake.noChunks = true
	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}, freshResolvedLayers(t))
	if !errors.Is(err, errChunkDeliveryUnsupported) {
		t.Fatalf("got %v, want errChunkDeliveryUnsupported so the caller falls back to a registry push", err)
	}
	if written, _, _ := fake.snapshot(); written != 0 {
		t.Fatalf("%d chunks sent to an agent that cannot use them", written)
	}
}

// An agent that stages chunks but cannot register an image by name (predates
// PrepareImage) cannot complete a delivery the CLI will then create a container
// from by name. That is also the fallback case, and must stop the upload.
func TestDeliverByChunks_PrepareUnimplementedIsTheFallbackCase(t *testing.T) {
	img, _ := deliveryFixture(t)
	fake := newFakeTargetAgent()
	fake.noPrepare = true
	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}, freshResolvedLayers(t))
	if !errors.Is(err, errChunkDeliveryUnsupported) {
		t.Fatalf("got %v, want errChunkDeliveryUnsupported", err)
	}
}

// A device refusing the image for a reason of its own — here an unsigned image
// where a signature is required — is neither resumed nor pushed through the
// registry instead: both would deliver an image the device said no to.
func TestDeliverByChunks_DeviceRefusalIsNeitherRetriedNorFallenBack(t *testing.T) {
	noDeliveryBackoff(t)
	img, _ := deliveryFixture(t)
	fake := newFakeTargetAgent()
	fake.prepareErr = status.Error(codes.FailedPrecondition, "container image is unsigned; refusing to run")
	dials := 0
	inner := serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake})
	svc, stream := deliveryService(t, func(ctx context.Context, target *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		dials++
		return inner(ctx, target)
	})

	err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}, freshResolvedLayers(t))
	if err == nil || errors.Is(err, errChunkDeliveryUnsupported) {
		t.Fatalf("got %v, want the device's refusal reported as this device's failure", err)
	}
	if code, _ := grpcStatusCode(err); code != codes.FailedPrecondition {
		t.Fatalf("the device's status code must survive the wrap, got %v", err)
	}
	if !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("the device's reason must reach the developer, got %v", err)
	}
	if dials != 1 {
		t.Fatalf("dialled %d times; a refusal is not a transport problem and must not be retried", dials)
	}
}

// A peer that cannot be reached at all is retried like a drop — a dial has no
// side effects — and then reported with the reason.
func TestDeliverByChunks_UnreachablePeerIsRetriedThenReported(t *testing.T) {
	noDeliveryBackoff(t)
	img, _ := deliveryFixture(t)
	dials := 0
	svc, stream := deliveryService(t, func(context.Context, *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		dials++
		return nil, errors.New("no route to peer")
	})

	err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}, freshResolvedLayers(t))
	if err == nil || !strings.Contains(err.Error(), "no route to peer") {
		t.Fatalf("got %v, want the dial failure reported", err)
	}
	if dials != deliveryAttempts {
		t.Fatalf("dialled %d times, want %d attempts before giving up", dials, deliveryAttempts)
	}
}

func TestRetryableDeliveryError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unavailable", status.Error(codes.Unavailable, "gone"), true},
		{"wrapped unavailable", fmt.Errorf("sending chunk: %w", status.Error(codes.Unavailable, "gone")), true},
		{"wrapped EOF", fmt.Errorf("sending chunk: %w", io.EOF), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"dial failure", &transientDeliveryError{errors.New("no route")}, true},
		{"cancelled", context.Canceled, false},
		{"wrapped cancelled", fmt.Errorf("querying: %w", context.Canceled), false},
		{"deadline", context.DeadlineExceeded, false},
		{"unsupported agent", errChunkDeliveryUnsupported, false},
		{"device refusal", fmt.Errorf("device 1: %w", status.Error(codes.FailedPrecondition, "unsigned")), false},
		{"too large", status.Error(codes.ResourceExhausted, "chunk too large"), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := retryableDeliveryError(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormatBuildBytes(t *testing.T) {
	cases := map[int64]string{
		0:                "0B",
		512:              "512B",
		1536:             "1.5KiB",
		3 << 20:          "3.0MiB",
		1932735283:       "1.8GiB",
		5 * (1 << 40):    "5.0TiB",
		(1 << 30) * 1024: "1.0TiB",
	}
	for n, want := range cases {
		if got := formatBuildBytes(n); got != want {
			t.Errorf("formatBuildBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTargetImageName_IsWhatTheCLICreatesFrom(t *testing.T) {
	got := targetImageName(&agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"})
	if got != "localhost:5000/myapp:latest" {
		t.Fatalf("got %q; the CLI's localRegistryReference builds localhost:<port>/<appid>:latest", got)
	}
}

// --- BuildImage end to end, with a buildctl that exports the fixture ---

// stubBuildctlExport replaces buildctl with the test binary in oci-export mode:
// an invocation asked for an OCI export writes the fixture image to dest; any
// other invocation (the registry-push fallback) exits 0. It records every
// argument list so tests can count passes.
func stubBuildctlExport(t *testing.T) *[][]string {
	t.Helper()
	original := buildctlCommandContext
	t.Cleanup(func() { buildctlCommandContext = original })
	t.Setenv("WENDY_BUILDCTL_TEST_HELPER", "oci-export")
	var invocations [][]string
	buildctlCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		invocations = append(invocations, args)
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestBuildctlHelperProcess$", "--"}, args...)...)
	}
	return &invocations
}

// buildctlHelperExport is the oci-export mode of TestBuildctlHelperProcess.
func buildctlHelperExport(t *testing.T) {
	args := flag.Args()
	for i, a := range args {
		if a != "--output" || i+1 >= len(args) {
			continue
		}
		if dest, ok := strings.CutPrefix(args[i+1], "type=oci,dest="); ok {
			writeTestOCITar(t, dest, testImageLayers(), testImageOptions{OS: "linux", Arch: "arm64"})
			fmt.Println("#1 [internal] load build definition from Dockerfile")
			fmt.Println("#1 DONE 0.0s")
		}
	}
	os.Exit(0)
}

func hasOutputArg(args []string, prefix string) bool {
	for i, a := range args {
		if a == "--output" && i+1 < len(args) && strings.HasPrefix(args[i+1], prefix) {
			return true
		}
	}
	return false
}

func chunkDeliveryService(t *testing.T, dial targetDialer) *BuildService {
	t.Helper()
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{
		ConfigPath:      enabledConfigDir(t),
		StateDir:        t.TempDir(),
		Peers:           stubPeerDialer{err: errors.New("the registry hop is not dialled in this test")},
		BuildkitAddress: presentBuildkitAddress(t),
		Chunks:          staticChunkSource{data: tarWith(t, "Dockerfile")},
		PushTLS:         func(int32) (*tls.Config, error) { return &tls.Config{MinVersion: tls.VersionTLS12}, nil },
	})
	svc.dialTarget = dial
	return svc
}

// One build, one export, N deliveries: a fleet deploy must not rebuild per
// device now that delivery no longer rides the build's own push.
func TestBuildImage_BuildsOnceAndDeliversByChunksToEveryDevice(t *testing.T) {
	invocations := stubBuildctlExport(t)
	fakes := map[int32]*fakeTargetAgent{214: newFakeTargetAgent(), 215: newFakeTargetAgent()}
	svc := chunkDeliveryService(t, serveFakeTargets(t, fakes))

	stream := &stubBuildStream{spec: &agentpbv2.BuildSpec{
		AppId:    "app",
		Platform: "linux/arm64",
		PushTargets: []*agentpbv2.PushTarget{
			{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"},
			{AssetId: 215, RegistryPort: 5000, Repository: "myapp:latest"},
		},
		Context: &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"},
		},
	}}
	if err := svc.BuildImage(stream); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if len(*invocations) != 1 {
		t.Fatalf("buildctl ran %d times for two devices, want once: delivery is fed from one export", len(*invocations))
	}
	if !hasOutputArg((*invocations)[0], "type=oci,dest=") {
		t.Fatalf("the build must export an OCI layout for delivery to read, got %q", (*invocations)[0])
	}
	for assetID, fake := range fakes {
		_, _, prepared := fake.snapshot()
		if len(prepared) != 1 || prepared[0].GetImageName() != "localhost:5000/myapp:latest" {
			t.Fatalf("device %d: image registered %d time(s) as %v, want once as localhost:5000/myapp:latest", assetID, len(prepared), prepared)
		}
	}

	var result *agentpbv2.BuildImageResult
	for _, ev := range stream.sent {
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
	if result == nil {
		t.Fatal("the stream must end with a result event")
	}
	if len(result.GetDeliveries()) != 2 || !result.GetDeliveries()[0].GetDelivered() || !result.GetDeliveries()[1].GetDelivered() {
		t.Fatalf("deliveries = %v, want both delivered", result.GetDeliveries())
	}
	if !strings.HasPrefix(result.GetImageDigest(), "sha256:") {
		t.Fatalf("image digest = %q; the export knows the manifest digest, so report it", result.GetImageDigest())
	}
	dir, err := svc.contextDir("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + exportedImageSuffix); !os.IsNotExist(err) {
		t.Fatalf("the exported image tar must be removed after delivery (stat: %v); it is the whole image on the build host's disk", err)
	}
	if entries, _ := os.ReadDir(svc.stateDir); len(entries) != 1 {
		t.Fatalf("state dir holds %v after delivery; decompressed layers must be cleaned up", entries)
	}
}

// A device whose agent predates chunked delivery still gets its image, the way
// it always did: a registry push, from a second buildctl pass.
func TestBuildImage_FallsBackToRegistryPushForAgentWithoutChunks(t *testing.T) {
	invocations := stubBuildctlExport(t)
	fake := newFakeTargetAgent()
	fake.noChunks = true
	svc := chunkDeliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	stream := &stubBuildStream{spec: &agentpbv2.BuildSpec{
		AppId:      "app",
		Platform:   "linux/arm64",
		PushTarget: &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"},
		Context:    &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"},
		},
	}}
	if err := svc.BuildImage(stream); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if len(*invocations) != 2 {
		t.Fatalf("buildctl ran %d times, want 2: the export, then the push pass for the old agent", len(*invocations))
	}
	if !hasOutputArg((*invocations)[1], "type=image,name=127.0.0.1:") || !strings.HasSuffix((*invocations)[1][len((*invocations)[1])-1], ",push=true") {
		t.Fatalf("the fallback pass must push through the loopback proxy, got %q", (*invocations)[1])
	}
	if lines := progressLines(stream); !containsLine(lines, "predates chunked delivery") {
		t.Fatalf("the developer must be told why delivery took the slow path; got %q", lines)
	}
}

// A delivery failure on the only device keeps the code and message prefix the
// CLI's classifyRemoteBuildError keys on, so an older CLI still tells "built
// but not delivered" from "did not build".
func TestBuildImage_SingleTargetDeliveryFailureKeepsTheCLIContract(t *testing.T) {
	noDeliveryBackoff(t)
	stubBuildctlExport(t)
	fake := newFakeTargetAgent()
	fake.prepareErr = status.Error(codes.ResourceExhausted, "disk full")
	svc := chunkDeliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	err := svc.BuildImage(&stubBuildStream{spec: &agentpbv2.BuildSpec{
		AppId:      "app",
		Platform:   "linux/arm64",
		PushTarget: &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"},
		Context:    &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"},
		},
	}})
	if status.Code(err) != codes.Unavailable || !strings.HasPrefix(status.Convert(err).Message(), "pushing the built image to the target device failed:") {
		t.Fatalf("got %v, want Unavailable with the delivery-failure prefix the CLI recognises", err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("the device's reason must reach the developer, got %v", err)
	}
}

func TestBuildctlOCIArgs_ExportsToDest(t *testing.T) {
	args, err := buildctlOCIArgs("/ctx", "Dockerfile", "linux/arm64", map[string]string{"B": "2", "A": "1"}, "/state/app.image.tar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasOutputArg(args, "type=oci,dest=/state/app.image.tar") {
		t.Fatalf("args = %q, want an OCI export to dest", args)
	}
	if hasOutputArg(args, "type=image") {
		t.Fatalf("args = %q, must not also push", args)
	}
	if _, err := buildctlOCIArgs("/ctx", "../Dockerfile", "linux/arm64", nil, "/x.tar"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want the same shape checks as the push variant", err)
	}
}

// A device that cannot say which layers it holds costs the optimisation, not
// the delivery: every layer is chunked, and the build log says so. A silent
// fallback here would hide a link problem behind a slow transfer.
func TestDeliverByChunks_LayerPreCheckFailureIsLoggedAndChunksEveryLayer(t *testing.T) {
	img, ti := deliveryFixture(t)
	layers := testImageLayers()
	total := 0
	for _, l := range layers {
		refs, err := chunk.ChunkBytes(l.content)
		if err != nil {
			t.Fatal(err)
		}
		total += len(refs)
	}

	fake := newFakeTargetAgent()
	fake.noLayers = true
	// The device does hold layer 0 in full; it just cannot be asked.
	fake.layers[ti.diffIDs[0]] = int64(len(layers[0].content))
	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))
	target := &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}
	if err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, target, freshResolvedLayers(t)); err != nil {
		t.Fatalf("deliverByChunks: a failed layer pre-check must not fail the delivery, got %v", err)
	}

	written, _, prepared := fake.snapshot()
	if written != total {
		t.Fatalf("device received %d chunks, want all %d: without a pre-check every layer is chunked", written, total)
	}
	if len(prepared) != 1 {
		t.Fatalf("PrepareImage called %d times, want 1", len(prepared))
	}
	if lines := progressLines(stream); !containsLine(lines, "which layers it already holds") {
		t.Fatalf("the build log must say the pre-check failed and why; got %q", lines)
	}
}

// An upload can fail after PrepareImage has already returned: when every chunk
// was staged by an earlier attempt the device registers the image at once, and
// the upload's own QueryChunks or CloseAndRecv then loses the link. The
// preparation goroutine reports once; settling must not wait for it twice.
func TestSettleUploadFailure_PrepareAlreadyFinishedDoesNotHang(t *testing.T) {
	uploadErr := errors.New("querying missing chunks of layer sha256:abc: EOF")
	prepareDone := make(chan error, 1)
	prepareDone <- nil
	got := settleWithin(t, 214, uploadErr, prepareDone, func() {})
	if !errors.Is(got, uploadErr) {
		t.Fatalf("got %v, want the upload's own error once preparation has succeeded", got)
	}
}

func TestSettleUploadFailure_PrepareFailedFirstIsTheRealError(t *testing.T) {
	prepareDone := make(chan error, 1)
	prepareDone <- status.Error(codes.FailedPrecondition, "image config rejected")
	got := settleWithin(t, 214, context.Canceled, prepareDone, func() {})
	if code, _ := grpcStatusCode(got); code != codes.FailedPrecondition {
		t.Fatalf("got %v, want the device's refusal, not the upload's cancellation", got)
	}
}

func TestSettleUploadFailure_PrepareStillRunningIsCancelledAndAwaited(t *testing.T) {
	uploadErr := errors.New("opening chunk upload for layer sha256:abc: EOF")
	prepareDone := make(chan error, 1)
	cancelled := make(chan struct{})
	go func() {
		<-cancelled
		prepareDone <- context.Canceled
	}()
	got := settleWithin(t, 214, uploadErr, prepareDone, func() { close(cancelled) })
	if !errors.Is(got, uploadErr) {
		t.Fatalf("got %v, want the upload's error after preparation was cancelled", got)
	}
}

// settleWithin bounds settleUploadFailure so a regression fails the test in
// seconds instead of hanging the suite the way it would hang the RPC.
func settleWithin(t *testing.T, assetID int32, uploadErr error, prepareDone <-chan error, cancelPrepare context.CancelFunc) error {
	t.Helper()
	out := make(chan error, 1)
	go func() { out <- settleUploadFailure(assetID, uploadErr, prepareDone, cancelPrepare) }()
	select {
	case err := <-out:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("settleUploadFailure hung: it waited for a preparation result that had already been taken")
		return nil
	}
}

// One decompress-and-chunk per layer per build, not per device: the second
// device of a fleet reuses the scratch the first one resolved.
func TestDeliverByChunks_ReusesResolvedLayersAcrossDevices(t *testing.T) {
	img, _ := deliveryFixture(t)
	total := chunkCount(t, testImageLayers())
	fakes := map[int32]*fakeTargetAgent{214: newFakeTargetAgent(), 215: newFakeTargetAgent()}
	svc, stream := deliveryService(t, serveFakeTargets(t, fakes))
	resolved := freshResolvedLayers(t)

	deliverTo := func(id int32) {
		t.Helper()
		target := &agentpbv2.PushTarget{AssetId: id, RegistryPort: 5000, Repository: "myapp:latest"}
		if err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, target, resolved); err != nil {
			t.Fatalf("device %d: %v", id, err)
		}
	}
	deliverTo(214)
	if len(resolved) != len(img.layers) {
		t.Fatalf("after the first device %d layer(s) are resolved, want %d", len(resolved), len(img.layers))
	}
	first := map[string]*deliveryLayer{}
	for k, v := range resolved {
		first[k] = v
	}
	scratchBefore := scratchFiles(t, svc.stateDir)

	deliverTo(215)
	for k, v := range resolved {
		if first[k] != v {
			t.Fatalf("layer %s was resolved again for the second device; the fleet must share one decompression", k)
		}
	}
	if scratchAfter := scratchFiles(t, svc.stateDir); strings.Join(scratchAfter, ",") != strings.Join(scratchBefore, ",") {
		t.Fatalf("scratch files changed between devices: %v -> %v", scratchBefore, scratchAfter)
	}
	for id, fake := range fakes {
		written, _, prepared := fake.snapshot()
		if written != total || len(prepared) != 1 {
			t.Fatalf("device %d received %d chunks (want %d) and %d registrations (want 1)", id, written, total, len(prepared))
		}
	}
}

// The property WDY-2564 asked for: a small edit inside a large layer costs a
// few chunks, not the layer.
func TestDeliverByChunks_SmallEditInLargeLayerSendsAFraction(t *testing.T) {
	const layerSize = 2 << 20
	before := pseudoRandomBytes(7, layerSize)
	after := append([]byte(nil), before...)
	copy(after[layerSize/2:], pseudoRandomBytes(8, 1024))

	fake := newFakeTargetAgent()
	refs, err := chunk.ChunkBytes(before)
	if err != nil {
		t.Fatal(err)
	}
	fake.stage(refs, before) // the previous version's chunks, from an earlier deploy

	path := fixtureTarPath(t)
	writeTestOCITar(t, path, []testImageLayer{{content: after}}, testImageOptions{OS: "linux", Arch: "arm64"})
	img, err := readExportedImage(path, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	svc, stream := deliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))
	target := &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"}
	if err := svc.deliverByChunks(context.Background(), &buildProgress{stream: stream}, 0, img, target, freshResolvedLayers(t)); err != nil {
		t.Fatalf("deliverByChunks: %v", err)
	}

	sent := fake.bytesWritten()
	if sent == 0 {
		t.Fatal("the edit never reached the device")
	}
	if sent > layerSize/4 {
		t.Fatalf("a 1 KiB edit in a %d-byte layer sent %d bytes; content-defined chunking should resend a few chunks, not the layer", layerSize, sent)
	}
}

// --chunking=force means what it says on a build host too: an agent that
// cannot receive chunks is a delivery failure, not a quiet registry push.
func TestBuildImage_ForceRefusesRegistryFallbackForOldAgent(t *testing.T) {
	invocations := stubBuildctlExport(t)
	fake := newFakeTargetAgent()
	fake.noChunks = true
	svc := chunkDeliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	err := svc.BuildImage(&stubBuildStream{spec: &agentpbv2.BuildSpec{
		AppId:      "app",
		Platform:   "linux/arm64",
		Chunking:   agentpbv2.ChunkingMode_CHUNKING_MODE_FORCE,
		PushTarget: &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"},
		Context:    &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"},
		},
	}})
	if status.Code(err) != codes.Unavailable || !strings.HasPrefix(status.Convert(err).Message(), "pushing the built image to the target device failed:") {
		t.Fatalf("got %v, want a delivery failure the CLI classifies as built-but-not-delivered", err)
	}
	if !strings.Contains(err.Error(), "force") {
		t.Fatalf("the error must say the registry push was refused by the chunking mode, got %v", err)
	}
	if len(*invocations) != 1 {
		t.Fatalf("buildctl ran %d times, want 1: force must not start the registry-push pass", len(*invocations))
	}
}

// --chunking=off keeps the route this feature shipped with: one buildctl pass
// per device pushing through its registry, with no export and no chunk store
// involved at all.
func TestBuildImage_OffTakesTheRegistryRouteWithoutExporting(t *testing.T) {
	invocations := stubBuildctlExport(t)
	fake := newFakeTargetAgent()
	svc := chunkDeliveryService(t, serveFakeTargets(t, map[int32]*fakeTargetAgent{214: fake}))

	stream := &stubBuildStream{spec: &agentpbv2.BuildSpec{
		AppId:      "app",
		Platform:   "linux/arm64",
		Chunking:   agentpbv2.ChunkingMode_CHUNKING_MODE_OFF,
		PushTarget: &agentpbv2.PushTarget{AssetId: 214, RegistryPort: 5000, Repository: "myapp:latest"},
		Context:    &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}},
		Definition: &agentpbv2.BuildSpec_DockerfileBuild{
			DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"},
		},
	}}
	if err := svc.BuildImage(stream); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if len(*invocations) != 1 || !hasOutputArg((*invocations)[0], "type=image,name=127.0.0.1:") {
		t.Fatalf("off must run exactly one pass per device, pushing through the loopback proxy; got %q", *invocations)
	}
	if written, streams, prepared := fake.snapshot(); written != 0 || streams != 0 || len(prepared) != 0 {
		t.Fatalf("off must not touch the device's chunk store: written=%d streams=%d prepared=%d", written, streams, len(prepared))
	}
	if lines := progressLines(stream); !containsLine(lines, "registry") {
		t.Fatalf("the build log must say the registry route was taken and why; got %q", lines)
	}
	dir, err := svc.contextDir("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + exportedImageSuffix); !os.IsNotExist(err) {
		t.Fatalf("off must not export an OCI tar it will never read (stat: %v)", err)
	}
}

// A CLI sending force must be able to tell this agent from one that would
// silently discard the field and push through the registry.
func TestGetBuildCapabilities_AdvertisesChunkDelivery(t *testing.T) {
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{ConfigPath: enabledConfigDir(t)})
	resp, err := svc.GetBuildCapabilities(context.Background(), &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetChunkDelivery() {
		t.Fatal("an agent that honours BuildSpec.chunking must say so, independently of buildkit being present")
	}
}
