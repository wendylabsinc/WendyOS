package chunkupload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeClient records WriteChunks traffic and is safe for concurrent streams.
type fakeClient struct {
	agentpb.WendyContainerServiceClient

	sendDelay  time.Duration // per Send, to make concurrent streams overlap
	failAfter  int           // when > 0, the Send that would receive chunk N+1 fails with failErr
	failErr    error
	closeErr   error
	stallAfter int // when > 0, every Send after the first stallAfter sends blocks until its stream's ctx ends

	mu          sync.Mutex
	received    map[[32]byte][]byte
	sends       int
	streams     int
	open        int
	maxOpen     int
	batchSizes  []int
	compressors []string
}

func newFakeClient() *fakeClient { return &fakeClient{received: map[[32]byte][]byte{}} }

func (f *fakeClient) WriteChunks(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse], error) {
	compressor := ""
	for _, o := range opts {
		if c, ok := o.(grpc.CompressorCallOption); ok {
			compressor = c.CompressorType
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streams++
	f.open++
	f.maxOpen = max(f.maxOpen, f.open)
	f.compressors = append(f.compressors, compressor)
	return &fakeStream{f: f, ctx: ctx}, nil
}

type fakeStream struct {
	grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]
	f      *fakeClient
	ctx    context.Context
	sent   int
	closed bool
}

func (s *fakeStream) Send(req *agentpb.WriteChunksRequest) error {
	if s.f.stallAfter > 0 {
		s.f.mu.Lock()
		stall := s.f.sends >= s.f.stallAfter
		s.f.mu.Unlock()
		if stall {
			<-s.ctx.Done()
			return s.ctx.Err()
		}
	}
	if s.f.sendDelay > 0 {
		select {
		case <-time.After(s.f.sendDelay):
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.failErr != nil && s.f.sends >= s.f.failAfter {
		return s.f.failErr
	}
	var h [32]byte
	copy(h[:], req.GetHash())
	s.f.received[h] = append([]byte(nil), req.GetData()...)
	s.f.sends++
	s.sent++
	return nil
}

func (s *fakeStream) CloseAndRecv() (*agentpb.WriteChunksResponse, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.f.open--
		s.f.batchSizes = append(s.f.batchSizes, s.sent)
	}
	return &agentpb.WriteChunksResponse{}, s.f.closeErr
}

// layerFixture writes n distinct chunkLen-byte chunks to a temp file.
func layerFixture(t *testing.T, n, chunkLen int) (*os.File, []chunk.Ref) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "layer-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	refs := make([]chunk.Ref, n)
	for i := range n {
		data := bytes.Repeat([]byte{byte(i), byte(i >> 8), 0x5a}, chunkLen/3)
		off := int64(i) * int64(len(data))
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
		refs[i] = chunk.Ref{Hash: sha256.Sum256(data), Offset: uint64(off), Len: uint64(len(data))}
	}
	return f, refs
}

func allMissing(refs []chunk.Ref) map[[32]byte]bool {
	m := make(map[[32]byte]bool, len(refs))
	for _, r := range refs {
		m[r.Hash] = true
	}
	return m
}

func TestPlanKeepsFirstOccurrenceOfEachMissingHash(t *testing.T) {
	a, b, c := [32]byte{1}, [32]byte{2}, [32]byte{3}
	refs := []chunk.Ref{
		{Hash: a, Offset: 0, Len: 10},
		{Hash: b, Offset: 10, Len: 20},
		{Hash: a, Offset: 30, Len: 10}, // same content again
		{Hash: c, Offset: 40, Len: 5},
	}
	plan, total := Plan(refs, map[[32]byte]bool{a: true, c: true})
	if len(plan) != 2 || plan[0].Hash != a || plan[0].Offset != 0 || plan[1].Hash != c {
		t.Fatalf("plan = %+v, want a@0 then c", plan)
	}
	if total != 15 {
		t.Fatalf("total = %d, want 15", total)
	}
}

func TestUploadSendsEveryPlannedChunkExactlyOnce(t *testing.T) {
	src, refs := layerFixture(t, 1000, 3*100)
	f := newFakeClient()
	f.sendDelay = 100 * time.Microsecond
	plan, total := Plan(refs, allMissing(refs))
	var reported atomic.Int64
	err := Upload(context.Background(), f, src, plan, Options{
		Layer: "sha256:test", BatchChunks: 64, Streams: 4, Compressor: Gzip,
		OnSent: func(n int) { reported.Add(int64(n)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.received) != len(refs) {
		t.Fatalf("received %d distinct chunks, want %d", len(f.received), len(refs))
	}
	if f.sends != len(refs) {
		t.Fatalf("sent %d chunks, want %d (each exactly once)", f.sends, len(refs))
	}
	buf := make([]byte, 300)
	for _, r := range refs {
		if _, err := src.ReadAt(buf[:r.Len], int64(r.Offset)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.received[r.Hash], buf[:r.Len]) {
			t.Fatalf("chunk %x bytes differ", r.Hash)
		}
	}
	if want := (len(refs) + 63) / 64; f.streams != want {
		t.Fatalf("opened %d streams, want one per batch (%d)", f.streams, want)
	}
	for _, n := range f.batchSizes {
		if n > 64 {
			t.Fatalf("a batch carried %d chunks, want <= 64", n)
		}
	}
	if f.maxOpen < 2 {
		t.Fatalf("max concurrent streams = %d, want >= 2", f.maxOpen)
	}
	if reported.Load() != total {
		t.Fatalf("OnSent reported %d bytes, want %d", reported.Load(), total)
	}
}

func TestUploadCapsStreamsAtBatchCount(t *testing.T) {
	src, refs := layerFixture(t, 10, 300)
	f := newFakeClient()
	if err := Upload(context.Background(), f, src, refs, Options{Streams: 4}); err != nil {
		t.Fatal(err)
	}
	if f.streams != 1 {
		t.Fatalf("opened %d streams for a single batch, want 1", f.streams)
	}
}

func TestUploadRespectsProcessWideStreamCap(t *testing.T) {
	f := newFakeClient()
	f.sendDelay = 200 * time.Microsecond
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		src, refs := layerFixture(t, 400, 300)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Upload(context.Background(), f, src, refs, Options{BatchChunks: 16, Streams: DefaultStreams})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.maxOpen > maxConcurrentStreams {
		t.Fatalf("max concurrent streams = %d, want <= %d", f.maxOpen, maxConcurrentStreams)
	}
	if f.maxOpen <= DefaultStreams {
		t.Fatalf("max concurrent streams = %d; three layers should share more than one layer's %d", f.maxOpen, DefaultStreams)
	}
}

func TestUploadReportsTerminalStatusAfterSendEOF(t *testing.T) {
	src, refs := layerFixture(t, 3, 300)
	f := newFakeClient()
	f.failErr = io.EOF
	f.closeErr = status.Error(codes.ResourceExhausted, "chunk staging exceeds the device limit")
	err := Upload(context.Background(), f, src, refs, Options{Layer: "sha256:big"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("error = %v, want ResourceExhausted", err)
	}
	if !strings.Contains(err.Error(), "sha256:big") || !strings.Contains(err.Error(), "staging exceeds") {
		t.Fatalf("error %q should name the layer and carry the server detail", err)
	}
}

// oneStreamFailClient fails only the Nth WriteChunks stream it opens (1-based,
// in the order streams are opened); every other stream keeps succeeding
// indefinitely. fakeClient's failAfter/failErr can't test sibling
// cancellation: they fail every stream once the cumulative send count crosses
// a threshold, so a test built on it can't tell "the other streams stopped
// promptly" apart from "every stream eventually fails on its own" — which is
// exactly the difference between errgroup.WithContext and a plain
// errgroup.Group.
type oneStreamFailClient struct {
	agentpb.WendyContainerServiceClient
	failStream int
	failErr    error

	mu     sync.Mutex
	opened int
	sends  int
}

func (c *oneStreamFailClient) WriteChunks(ctx context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened++
	return &oneStreamFailStream{c: c, ctx: ctx, fail: c.opened == c.failStream}, nil
}

type oneStreamFailStream struct {
	grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]
	c    *oneStreamFailClient
	ctx  context.Context
	fail bool
	n    int
}

func (s *oneStreamFailStream) Send(*agentpb.WriteChunksRequest) error {
	select {
	case <-time.After(100 * time.Microsecond):
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	s.n++
	if s.fail && s.n == 5 {
		return s.c.failErr
	}
	s.c.mu.Lock()
	s.c.sends++
	s.c.mu.Unlock()
	return nil
}

func (s *oneStreamFailStream) CloseAndRecv() (*agentpb.WriteChunksResponse, error) {
	return &agentpb.WriteChunksResponse{}, nil
}

func TestUploadStopsOtherStreamsOnFirstError(t *testing.T) {
	src, refs := layerFixture(t, 2000, 300)
	injected := errors.New("device refused this batch")
	c := &oneStreamFailClient{failStream: 2, failErr: injected}
	err := Upload(context.Background(), c, src, refs, Options{BatchChunks: 32, Streams: 4})
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	if c.sends > len(refs)/4 {
		t.Fatalf("siblings kept uploading after the failure: %d of %d chunks sent", c.sends, len(refs))
	}
	t.Logf("sent %d of %d chunks before stopping; %d streams opened", c.sends, len(refs), c.opened)
}

func TestUploadPassesCompressor(t *testing.T) {
	for _, compressor := range []string{Gzip, ""} {
		src, refs := layerFixture(t, 40, 300)
		f := newFakeClient()
		if err := Upload(context.Background(), f, src, refs, Options{BatchChunks: 8, Compressor: compressor}); err != nil {
			t.Fatal(err)
		}
		for _, got := range f.compressors {
			if got != compressor {
				t.Fatalf("stream compressor = %q, want %q", got, compressor)
			}
		}
	}
}

// eofAtEndReaderAt is an io.ReaderAt that, for a read reaching the end of
// data, returns io.EOF alongside the full read (n == len(p)) rather than
// waiting for a separate zero-byte read to report it — a shape the
// io.ReaderAt contract explicitly permits and that Upload must still accept.
type eofAtEndReaderAt struct{ data []byte }

func (r *eofAtEndReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, r.data[off:])
	if off+int64(n) >= int64(len(r.data)) {
		return n, io.EOF
	}
	return n, nil
}

func TestUploadAcceptsReadAtEOFOnLastChunk(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, 300)
	hash := sha256.Sum256(data)
	refs := []chunk.Ref{{Hash: hash, Offset: 0, Len: uint64(len(data))}}
	f := newFakeClient()
	if err := Upload(context.Background(), f, &eofAtEndReaderAt{data: data}, refs, Options{}); err != nil {
		t.Fatalf("Upload with a ReadAt that reports io.EOF alongside the final full read: %v", err)
	}
	if !bytes.Equal(f.received[hash], data) {
		t.Fatal("received chunk bytes differ from the source")
	}
}
