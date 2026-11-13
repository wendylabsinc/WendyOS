package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/containerd/v2/plugins/services/content/contentserver"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

// countingStore counts ReaderAt opens and ReadAt calls against a real store.
type countingStore struct {
	content.Store
	opens, reads atomic.Int32
}

func (s *countingStore) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	ra, err := s.Store.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	s.opens.Add(1)
	return &countingReaderAt{ReaderAt: ra, reads: &s.reads}, nil
}

type countingReaderAt struct {
	content.ReaderAt
	reads *atomic.Int32
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	return r.ReaderAt.ReadAt(p, off)
}

// newCountingStoreClient is newLocalStoreClient with read counting.
func newCountingStoreClient(t *testing.T) (*Client, *countingStore) {
	t.Helper()
	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: store}
	c := newStoreClient(t, cs)
	return c, cs
}

// defaultNS is the namespace the test clients use, for calls made straight to
// a store: the proxy stack is namespaced, and the bare local store ignores it.
var defaultNS = namespaces.WithNamespace(context.Background(), "default")

// newProxyContentStore returns the content store the agent reaches on a
// device: containerd's proxy store, over gRPC, to the content server, then
// the metadata store (namespaces, and the default shared-content policy) over
// a local store. The bare local store the other tests use skips all of that:
// the content server answers a Commit with AlreadyExists before the writer
// hashes a byte, and the metadata store hands out a writer that already holds
// a blob another namespace committed.
func newProxyContentStore(t *testing.T) content.Store {
	t.Helper()
	root := t.TempDir()
	backend, err := local.NewStore(filepath.Join(root, "content"))
	if err != nil {
		t.Fatal(err)
	}
	bdb, err := bolt.Open(filepath.Join(root, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, backend, nil)
	if err := mdb.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
		grpc.MaxSendMsgSize(defaults.DefaultMaxSendMsgSize))
	contentapi.RegisterContentServer(srv, contentserver.New(mdb.ContentStore()))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return proxy.NewContentStore(conn)
}

// newProxyStoreClient is newLocalStoreClient over the proxy stack.
func newProxyStoreClient(t *testing.T) (*Client, content.Store) {
	t.Helper()
	store := newProxyContentStore(t)
	return newStoreClient(t, store), store
}

// storeRigs are the content stores an assembly test can run over.
var storeRigs = []struct {
	name string
	new  func(*testing.T) (*Client, content.Store)
}{
	{"local store", newLocalStoreClient},
	{"proxy", newProxyStoreClient},
}

// writePartialIngest leaves the first n bytes of layer in an ingest under the
// layer's ref, as an attempt that died part way through does.
func writePartialIngest(t *testing.T, cs content.Store, layer []byte, n int) {
	t.Helper()
	diffID := digest.FromBytes(layer)
	w, err := content.OpenWriter(defaultNS, cs, content.WithRef(diffID.String()),
		content.WithDescriptor(ocispec.Descriptor{Digest: diffID, Size: int64(len(layer))}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(layer[:n]); err != nil {
		t.Fatal(err)
	}
	w.Close()
}

// readBlob returns the committed blob dgst.
func readBlob(t *testing.T, cs content.Store, dgst digest.Digest) []byte {
	t.Helper()
	b, err := content.ReadBlob(defaultNS, cs, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		t.Fatalf("reading blob %s: %v", dgst, err)
	}
	return b
}

// requireUnindexedAndStaged fails unless no chunk in hashes is indexed and
// every one is still staged: what an assembly that verified nothing leaves.
func requireUnindexedAndStaged(t *testing.T, c *Client, hashes [][32]byte) {
	t.Helper()
	for i, h := range hashes {
		if loc, ok := c.chunkIndex.Has(h); ok {
			t.Fatalf("chunk %d indexed at %+v by a manifest nothing checked", i, loc)
		}
		if !c.staging.has(h) {
			t.Fatalf("chunk %d unstaged although nothing indexed it", i)
		}
	}
}

// requireIndexedAndUnstaged fails unless every chunk of data, as hashes names
// them, is indexed at its range in the blob dgst and no longer staged.
func requireIndexedAndUnstaged(t *testing.T, c *Client, dgst digest.Digest, data []byte, hashes [][32]byte) {
	t.Helper()
	var off uint64
	for i, h := range hashes {
		loc, ok := c.chunkIndex.Has(h)
		if !ok || loc.Blob != dgst.String() || loc.Offset != off {
			t.Fatalf("chunk %d indexed as %+v (%v), want %s@%d", i, loc, ok, dgst, off)
		}
		if sha256.Sum256(data[loc.Offset:loc.Offset+loc.Len]) != h {
			t.Fatalf("chunk %d's indexed range does not hold its bytes", i)
		}
		if c.staging.has(h) {
			t.Fatalf("chunk %d still staged after it was indexed", i)
		}
		off += loc.Len
	}
}

// hookStore hands out writers whose Write and Commit run onWrite and onCommit
// instead, when set, given the real writer.
type hookStore struct {
	content.Store
	onWrite  func(w content.Writer, p []byte) (int, error)
	onCommit func(ctx context.Context, w content.Writer, size int64, expected digest.Digest, opts ...content.Opt) error
}

func (s *hookStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	w, err := s.Store.Writer(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &hookWriter{Writer: w, s: s}, nil
}

type hookWriter struct {
	content.Writer
	s *hookStore
}

func (w *hookWriter) Write(p []byte) (int, error) {
	if w.s.onWrite == nil {
		return w.Writer.Write(p)
	}
	return w.s.onWrite(w.Writer, p)
}

func (w *hookWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	if w.s.onCommit == nil {
		return w.Writer.Commit(ctx, size, expected, opts...)
	}
	return w.s.onCommit(ctx, w.Writer, size, expected, opts...)
}

// commitRaceStore commits blob under another ref just before a writer it
// handed out commits, as a concurrent assembly of the same layer finishing
// first would.
func commitRaceStore(store content.Store, blob []byte) *hookStore {
	return &hookStore{Store: store, onCommit: func(ctx context.Context, w content.Writer, size int64, expected digest.Digest, opts ...content.Opt) error {
		dgst := digest.FromBytes(blob)
		if err := content.WriteBlob(ctx, store, "concurrent assembly", bytes.NewReader(blob),
			ocispec.Descriptor{Digest: dgst, Size: int64(len(blob))}); err != nil {
			return err
		}
		return w.Commit(ctx, size, expected, opts...)
	}}
}

// TestAssembleLayerFromChunksDoesNotIndexAfterACommitRace: a concurrent
// assembly commits the layer between this one's OpenWriter and its Commit.
// Over the proxy, the content server then answers the Commit with
// AlreadyExists before it hashes a byte of this ingest, so AlreadyExists at
// Commit says nothing about this manifest, and it must not be indexed. The
// local store does check the digest first, but the agent never talks to one.
func TestAssembleLayerFromChunksDoesNotIndexAfterACommitRace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*testing.T) content.Store
		// otherBytes stages a manifest of other bytes than the layer's: the
		// reviewer's probe, which the old code indexed.
		otherBytes bool
	}{
		{"local store, the layer's chunks", func(t *testing.T) content.Store {
			s, err := local.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, false},
		{"proxy, the layer's chunks", newProxyContentStore, false},
		{"proxy, chunks of other bytes", newProxyContentStore, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := randomBytes(20, 300_000)
			store := tc.store(t)
			c := newStoreClient(t, commitRaceStore(store, layer))
			manifest := layer
			if tc.otherBytes {
				manifest = randomBytes(21, len(layer))
			}
			hashes := stageChunks(t, c, manifest)
			diffID := digest.FromBytes(layer)

			if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
				t.Fatal(err)
			}
			if got := readBlob(t, store, diffID); !bytes.Equal(got, layer) {
				t.Fatal("the committed blob is not the layer")
			}
			requireUnindexedAndStaged(t, c, hashes)
		})
	}
}

// TestAssembleLayerFromChunksChecksSharedContentAgainstTheManifest: under
// containerd's default shared-content policy, a layer another namespace
// committed is handed out as a writer that already holds every byte. The
// reader then skips the whole layer and Commit succeeds without reading this
// manifest. The committed blob is read back and checked against it instead.
func TestAssembleLayerFromChunksChecksSharedContentAgainstTheManifest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		otherBytes bool
	}{
		{"the layer's chunks", false},
		{"chunks of other bytes of the same size", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, store := newProxyStoreClient(t)
			layer := randomBytes(30, 300_000)
			diffID := digest.FromBytes(layer)
			other := namespaces.WithNamespace(context.Background(), "other")
			if err := content.WriteBlob(other, store, "other namespace", bytes.NewReader(layer),
				ocispec.Descriptor{Digest: diffID, Size: int64(len(layer))}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Info(defaultNS, diffID); err == nil {
				t.Fatal("fixture: the blob is already visible in the default namespace")
			}
			manifest := layer
			if tc.otherBytes {
				manifest = randomBytes(31, len(layer))
			}
			hashes := stageChunks(t, c, manifest)

			if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
				t.Fatal(err)
			}
			if got := readBlob(t, store, diffID); !bytes.Equal(got, layer) {
				t.Fatal("the committed blob is not the layer")
			}
			if tc.otherBytes {
				requireUnindexedAndStaged(t, c, hashes)
			} else {
				requireIndexedAndUnstaged(t, c, diffID, layer, hashes)
			}
		})
	}
}

// TestAssembleLayerFromChunksChecksAResumedPrefixAgainstTheManifest: the
// reader skips a resumed ingest's prefix unread, and Commit vouches for the
// earlier attempt's bytes, not for this manifest's chunks there. A manifest
// naming other bytes for the prefix must not be indexed.
func TestAssembleLayerFromChunksChecksAResumedPrefixAgainstTheManifest(t *testing.T) {
	for _, rig := range storeRigs {
		t.Run(rig.name, func(t *testing.T) {
			c, cs := rig.new(t)
			prefix, rest := randomBytes(40, 250_000), randomBytes(41, 350_000)
			layer := append(bytes.Clone(prefix), rest...)
			diffID := digest.FromBytes(layer)
			hashes := append(stageChunks(t, c, randomBytes(42, len(prefix))), stageChunks(t, c, rest)...)
			writePartialIngest(t, cs, layer, len(prefix))

			if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
				t.Fatal(err)
			}
			if got := readBlob(t, cs, diffID); !bytes.Equal(got, layer) {
				t.Fatal("the committed blob is not the layer")
			}
			requireUnindexedAndStaged(t, c, hashes)
		})
	}
}

// unreadableStore fails every ReaderAt, as a store that cannot serve reads.
type unreadableStore struct{ content.Store }

func (unreadableStore) ReaderAt(context.Context, ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errors.New("read path down")
}

// TestAssembleLayerFromChunksDoesNotIndexAPrefixItCouldNotReadBack: indexing
// is an optimization. A resumed prefix that cannot be read back leaves the
// committed layer valid and the assembly successful, but unindexed.
func TestAssembleLayerFromChunksDoesNotIndexAPrefixItCouldNotReadBack(t *testing.T) {
	backend, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := newStoreClient(t, unreadableStore{backend})
	core, logs := observer.New(zap.WarnLevel)
	c.logger = zap.New(core)
	layer := randomBytes(43, 600_000)
	hashes := stageChunks(t, c, layer)
	diffID := digest.FromBytes(layer)
	writePartialIngest(t, backend, layer, 250_000)

	if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
		t.Fatal(err)
	}
	if got := readBlob(t, backend, diffID); !bytes.Equal(got, layer) {
		t.Fatal("the committed blob is not the layer")
	}
	requireUnindexedAndStaged(t, c, hashes)
	if n := logs.FilterMessage("Could not read back a resumed layer prefix; not indexing it").Len(); n != 1 {
		t.Fatalf("logged %d read-back failures, want 1", n)
	}
}

func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// commitIndexedBlob writes data to the store as a finished layer blob and
// indexes its chunks, like a layer assembled by an earlier deploy.
func commitIndexedBlob(t *testing.T, c *Client, cs content.Store, data []byte) (digest.Digest, []chunk.Ref) {
	t.Helper()
	dgst := digest.FromBytes(data)
	if err := content.WriteBlob(context.Background(), cs, dgst.String(), bytes.NewReader(data), ocispec.Descriptor{Digest: dgst, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	refs, err := chunk.ChunkBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.chunkIndex.AddLayer(dgst.String(), refs); err != nil {
		t.Fatal(err)
	}
	return dgst, refs
}

func TestPlanAssemblyCoalescesContiguousRuns(t *testing.T) {
	c, _ := newCountingStoreClient(t)
	blob := digest.FromString("previous layer").String()
	var refs []chunk.Ref
	for i := range 5 {
		refs = append(refs, chunk.Ref{Hash: [32]byte{byte(i + 1)}, Offset: uint64(i * 10), Len: 10})
	}
	if err := c.chunkIndex.AddLayer(blob, refs); err != nil {
		t.Fatal(err)
	}
	staged := []byte("a new chunk")
	sh := sha256.Sum256(staged)
	if err := c.staging.write(sh, staged); err != nil {
		t.Fatal(err)
	}

	// r0 r1 r2 | staged | r4: r3 is skipped, so r4 is not contiguous with r2.
	hashes := [][32]byte{refs[0].Hash, refs[1].Hash, refs[2].Hash, sh, refs[4].Hash}
	segs, planned, total, err := c.planAssembly(hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(segs), segs)
	}
	if segs[0].blob != blob || segs[0].offset != 0 || segs[0].size != 30 || len(segs[0].chunks) != 3 {
		t.Fatalf("segment 0 = %+v, want r0..r2 as one 30-byte read", segs[0])
	}
	if segs[1].blob != "" || segs[1].size != uint64(len(staged)) {
		t.Fatalf("segment 1 = %+v, want the staged chunk", segs[1])
	}
	if segs[2].blob != blob || segs[2].offset != 40 || segs[2].size != 10 {
		t.Fatalf("segment 2 = %+v, want r4 alone", segs[2])
	}
	if want := int64(40 + len(staged)); total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	if planned[3].Offset != 30 || planned[4].Offset != uint64(30+len(staged)) {
		t.Fatalf("refs = %+v, want offsets by prefix sum in the new blob", planned)
	}
}

func TestPlanAssemblySplitsRunsAtMaxSegmentBytes(t *testing.T) {
	c, _ := newCountingStoreClient(t)
	blob := digest.FromString("huge layer").String()
	const size = maxSegmentBytes / 2
	refs := []chunk.Ref{
		{Hash: [32]byte{1}, Offset: 0, Len: size},
		{Hash: [32]byte{2}, Offset: size, Len: size},
		{Hash: [32]byte{3}, Offset: 2 * size, Len: size},
	}
	if err := c.chunkIndex.AddLayer(blob, refs); err != nil {
		t.Fatal(err)
	}
	segs, _, _, err := c.planAssembly([][32]byte{refs[0].Hash, refs[1].Hash, refs[2].Hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].size != maxSegmentBytes || segs[1].size != size {
		t.Fatalf("segments = %+v, want one full segment then the rest", segs)
	}
}

func TestPlanAssemblyReportsUnavailableChunk(t *testing.T) {
	c, _ := newCountingStoreClient(t)
	if _, _, _, err := c.planAssembly([][32]byte{{9}}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want an unavailable chunk", err)
	}
}

// TestAssemblyReadsAStagedChunkFromTheIndexOnceAnotherAssemblyConsumedIt
// covers two layers sharing a staged chunk: the first assembly indexes the
// chunk into its blob and removes the staged file while the second is planned.
func TestAssemblyReadsAStagedChunkFromTheIndexOnceAnotherAssemblyConsumedIt(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	shared := randomBytes(6, 10_000) // below chunk.MinSize: indexing it yields this one chunk
	h := sha256.Sum256(shared)
	if err := c.StageChunk(context.Background(), h, shared); err != nil {
		t.Fatal(err)
	}
	segs, _, _, err := c.planAssembly([][32]byte{h})
	if err != nil {
		t.Fatal(err)
	}

	commitIndexedBlob(t, c, cs, shared) // the other assembly's blob holds the chunk…
	c.staging.remove(h)                 // …and it released the staged file

	got, err := collectSegments(c.readSegments(context.Background(), segs, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, shared) {
		t.Fatal("fallback read returned the wrong bytes")
	}
}

// TestReadSegmentsSkipsAResumedPrefix: a resumed ingest's prefix is not
// delivered, whether it ends inside a segment or on a boundary, and a segment
// wholly inside it is not read at all.
func TestReadSegmentsSkipsAResumedPrefix(t *testing.T) {
	for _, skip := range []int64{0, 5_000, 10_000, 25_000, 30_000, 59_999, 60_000} {
		t.Run(fmt.Sprint("skip ", skip), func(t *testing.T) {
			c, _ := newCountingStoreClient(t)
			// Three staged segments of one chunk each: [0,10k) [10k,30k) [30k,60k).
			var (
				layer []byte
				segs  []assemblySegment
			)
			for i, n := range []int{10_000, 20_000, 30_000} {
				data := randomBytes(int64(100+i), n)
				h := sha256.Sum256(data)
				if err := c.StageChunk(context.Background(), h, data); err != nil {
					t.Fatal(err)
				}
				if int64(len(layer)+n) <= skip {
					c.staging.remove(h) // wholly skipped: reading it would fail
				}
				layer = append(layer, data...)
				segs = append(segs, assemblySegment{size: uint64(n), chunks: []assemblyChunk{{hash: h, len: uint64(n)}}})
			}

			var (
				got       []byte
				delivered int
			)
			for seg := range c.readSegments(context.Background(), segs, skip, nil) {
				if seg.err != nil {
					t.Fatal(seg.err)
				}
				got = append(got, seg.data...)
				delivered++
				seg.release()
			}
			if !bytes.Equal(got, layer[skip:]) {
				t.Fatalf("delivered %d bytes, want the %d after the skip", len(got), len(layer)-int(skip))
			}
			want := 0 // segments ending after the skip
			var end int64
			for _, s := range segs {
				if end += int64(s.size); end > skip {
					want++
				}
			}
			if delivered != want {
				t.Fatalf("delivered %d segments, want %d", delivered, want)
			}
		})
	}
}

// TestConcurrentAssembliesOfTheSameLayer: concurrent deploys of one image
// assemble the same layer at once. They serialize on the per-ref ingest lock
// (OpenWriter retries while another holds it), one commits and indexes the
// layer, and the rest find it committed. All succeed.
func TestConcurrentAssembliesOfTheSameLayer(t *testing.T) {
	for _, rig := range storeRigs {
		t.Run(rig.name, func(t *testing.T) {
			c, cs := rig.new(t)
			core, logs := observer.New(zap.InfoLevel)
			c.logger = zap.New(core)
			layer := randomBytes(110, 2<<20)
			hashes := stageChunks(t, c, layer)
			diffID := digest.FromBytes(layer)

			const n = 3
			errs := make(chan error, n)
			for range n {
				go func() { errs <- c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes) }()
			}
			for range n {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}

			if got := readBlob(t, cs, diffID); !bytes.Equal(got, layer) {
				t.Fatal("the committed blob is not the layer")
			}
			requireIndexedAndUnstaged(t, c, diffID, layer, hashes)
			verified := 0
			for _, l := range logs.FilterMessage("Assembled layer").All() {
				if l.ContextMap()["verified"] == true {
					verified++
				}
			}
			if verified != 1 {
				t.Fatalf("%d assemblies verified the layer, want exactly the one that committed it", verified)
			}
		})
	}
}

// collectSegments returns the bytes readSegments delivers, or the error that
// ended them, giving every buffer back as a writer does.
func collectSegments(segments <-chan segmentData) ([]byte, error) {
	var (
		got []byte
		err error
	)
	for seg := range segments {
		if seg.err != nil && err == nil {
			err = seg.err
		}
		got = append(got, seg.data...)
		seg.release()
	}
	return got, err
}

// TestAssembleLayerFromChunksReadsReusedChunksInFewReads is the WDY-3213 case:
// a rebuilt layer that is mostly an earlier layer's chunks plus a few new ones.
func TestAssembleLayerFromChunksReadsReusedChunksInFewReads(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	previous := randomBytes(1, 2<<20)
	_, prevRefs := commitIndexedBlob(t, c, cs, previous)
	if len(prevRefs) < 8 {
		t.Fatalf("fixture has %d chunks, want several", len(prevRefs))
	}

	// New layer: the first half of the old chunks, one new chunk, the rest.
	half := len(prevRefs) / 2
	fresh := randomBytes(2, 40_000)
	var layer []byte
	var hashes [][32]byte
	for _, r := range prevRefs[:half] {
		layer = append(layer, previous[r.Offset:r.Offset+r.Len]...)
		hashes = append(hashes, r.Hash)
	}
	fh := sha256.Sum256(fresh)
	if err := c.StageChunk(context.Background(), fh, fresh); err != nil {
		t.Fatal(err)
	}
	layer = append(layer, fresh...)
	hashes = append(hashes, fh)
	for _, r := range prevRefs[half:] {
		layer = append(layer, previous[r.Offset:r.Offset+r.Len]...)
		hashes = append(hashes, r.Hash)
	}
	diffID := digest.FromBytes(layer)
	cs.opens.Store(0)
	cs.reads.Store(0)

	if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
		t.Fatal(err)
	}

	info, err := cs.Info(context.Background(), diffID)
	if err != nil || info.Size != int64(len(layer)) {
		t.Fatalf("assembled blob: %+v, %v", info, err)
	}
	if got := cs.opens.Load(); got != 1 {
		t.Fatalf("opened the previous blob %d times, want once for the whole assembly", got)
	}
	if got := cs.reads.Load(); got != 2 {
		t.Fatalf("%d reads for %d reused chunks in two runs, want 2", got, len(prevRefs))
	}
}

func TestAssembleLayerFromChunksResumesAPartialIngest(t *testing.T) {
	for _, rig := range storeRigs {
		t.Run(rig.name, func(t *testing.T) {
			c, cs := rig.new(t)
			layer := randomBytes(3, 600_000)
			hashes := stageChunks(t, c, layer)
			diffID := digest.FromBytes(layer)
			// An earlier attempt wrote part of the layer under the same ref, then died.
			writePartialIngest(t, cs, layer, 250_000)

			if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
				t.Fatalf("resuming the partial ingest: %v", err)
			}
			if got := readBlob(t, cs, diffID); !bytes.Equal(got, layer) {
				t.Fatal("resumed blob differs from the layer")
			}
			// The prefix read back matches the manifest, so it is indexed.
			requireIndexedAndUnstaged(t, c, diffID, layer, hashes)
		})
	}
}

func TestAssembleLayerFromChunksRejectsACorruptStagedChunk(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	data := randomBytes(4, 50_000)
	h := sha256.Sum256(data)
	if err := c.StageChunk(context.Background(), h, data); err != nil {
		t.Fatal(err)
	}
	// Bit rot after staging: the file no longer matches its name.
	if err := os.WriteFile(c.staging.path(h), randomBytes(5, len(data)), 0o600); err != nil {
		t.Fatal(err)
	}
	diffID := digest.FromBytes(data)
	err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), [][32]byte{h})
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("error = %v, want a hash mismatch", err)
	}
	if _, err := cs.Info(context.Background(), diffID); err == nil {
		t.Fatal("a layer with a corrupt chunk was committed")
	}
	// staging.write never replaces a file, so a corrupt one left in place
	// would fail every later assembly while MissingChunks reported it present.
	if c.staging.has(h) {
		t.Fatal("the corrupt staged chunk is still staged")
	}
}

// TestAssemblyDropsAnIndexedBlobWhoseRangeFailsItsHash: an index entry whose
// range does not hold its chunk fails every read of it. The assembly fails,
// and the blob's entries go, so the next MissingChunks asks for the chunks
// again.
func TestAssemblyDropsAnIndexedBlobWhoseRangeFailsItsHash(t *testing.T) {
	for _, tc := range []struct {
		name string
		// staged plans the chunk while it is staged and removes the file
		// before the read, so the reader falls back to the index.
		staged bool
	}{
		{"planned from the index", false},
		{"staged file consumed, read from the index", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cs := newCountingStoreClient(t)
			data := randomBytes(95, 10_000)
			h := sha256.Sum256(data)
			other := randomBytes(96, 30_000)
			blob, _ := commitIndexedBlob(t, c, cs, other)
			var segs []assemblySegment
			if tc.staged {
				if err := c.StageChunk(context.Background(), h, data); err != nil {
					t.Fatal(err)
				}
				var err error
				if segs, _, _, err = c.planAssembly([][32]byte{h}); err != nil {
					t.Fatal(err)
				}
				c.staging.remove(h)
			}
			// A wrong entry: the chunk's hash, at a range of a blob holding other bytes.
			if err := c.chunkIndex.AddLayer(blob.String(), []chunk.Ref{{Hash: h, Offset: 100, Len: uint64(len(data))}}); err != nil {
				t.Fatal(err)
			}
			if !tc.staged {
				var err error
				if segs, _, _, err = c.planAssembly([][32]byte{h}); err != nil {
					t.Fatal(err)
				}
			}

			_, err := collectSegments(c.readSegments(context.Background(), segs, 0, nil))
			if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
				t.Fatalf("error = %v, want a hash mismatch", err)
			}
			if blobs, err := c.chunkIndex.Blobs(); err != nil || len(blobs) != 0 {
				t.Fatalf("indexed blobs = %v (%v), want the failing blob's entries dropped", blobs, err)
			}
		})
	}
}

// TestPlanAssemblyRejectsImplausibleIndexedLengths: no chunk is empty or
// larger than StageChunk accepts. A corrupt index length could wrap the
// coalesced segment size and panic the reader goroutine, which no gRPC
// handler's recovery covers, taking the agent down. The chunk is unavailable
// instead, and the blob's entries go, so the next MissingChunks, which
// checks only that a range fits its blob, asks for the chunks again.
func TestPlanAssemblyRejectsImplausibleIndexedLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []chunk.Ref
	}{
		{"empty", []chunk.Ref{{Hash: [32]byte{1}, Len: 0}}},
		{"larger than a staged chunk may be", []chunk.Ref{{Hash: [32]byte{1}, Len: maxStagedChunkBytes + 1}}},
		{"wrapping the segment size", []chunk.Ref{
			{Hash: [32]byte{1}, Offset: 0, Len: 10},
			{Hash: [32]byte{2}, Offset: 10, Len: math.MaxUint64 - 5},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCountingStoreClient(t)
			if err := c.chunkIndex.AddLayer(digest.FromString("corrupt").String(), tc.refs); err != nil {
				t.Fatal(err)
			}
			var hashes [][32]byte
			for _, r := range tc.refs {
				hashes = append(hashes, r.Hash)
			}
			if _, _, _, err := c.planAssembly(hashes); err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("error = %v, want an unavailable chunk", err)
			}
			if blobs, err := c.chunkIndex.Blobs(); err != nil || len(blobs) != 0 {
				t.Fatalf("indexed blobs = %v (%v), want the corrupt blob's entries dropped", blobs, err)
			}
		})
	}
}

// TestPlanAssemblyRemovesAStagedFileNoChunkCouldBe: the staging area writes
// without fsync, so a power cut can leave a chunk's file empty. MissingChunks
// would report it present and staging.write would never replace it; the
// planner removes it, so the CLI sends the chunk again.
func TestPlanAssemblyRemovesAStagedFileNoChunkCouldBe(t *testing.T) {
	c, _ := newCountingStoreClient(t)
	h := sha256.Sum256([]byte("a chunk whose file was cut short"))
	if err := os.MkdirAll(c.staging.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.staging.path(h), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.planAssembly([][32]byte{h}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want an unavailable chunk", err)
	}
	if c.staging.has(h) {
		t.Fatal("the empty staged file is still there")
	}
}

// panickingStore's readers panic on every read.
type panickingStore struct{ content.Store }

func (s panickingStore) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	ra, err := s.Store.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	return panickingReaderAt{ra}, nil
}

type panickingReaderAt struct{ content.ReaderAt }

func (panickingReaderAt) ReadAt([]byte, int64) (int, error) { panic("corrupt read") }

// TestReadSegmentsTurnsAPanicIntoAnError: a panic on the reader goroutine
// fails the assembly and gives its buffer back, instead of killing the agent.
func TestReadSegmentsTurnsAPanicIntoAnError(t *testing.T) {
	pool := useFreshAssemblyBuffers(t)
	backend, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := newStoreClient(t, panickingStore{backend})
	_, refs := commitIndexedBlob(t, c, backend, randomBytes(97, 200_000))
	var hashes [][32]byte
	for _, r := range refs {
		hashes = append(hashes, r.Hash)
	}
	segs, _, _, err := c.planAssembly(hashes)
	if err != nil {
		t.Fatal(err)
	}

	_, err = collectSegments(c.readSegments(context.Background(), segs, 0, nil))
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("error = %v, want the recovered panic", err)
	}
	requireAllBuffersReturned(t, pool)
}

// TestWriteAssembledLayerReportsUnverifiedWhenTheBlobAlreadyExists covers rule
// 1's first bullet directly: content.OpenWriter reports AlreadyExists before
// anything reads a segment, so writeAssembledLayer must say so rather than
// claim it verified a manifest it never looked at.
func TestWriteAssembledLayerReportsUnverifiedWhenTheBlobAlreadyExists(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	data := randomBytes(7, 1_000)
	dgst, _ := commitIndexedBlob(t, c, cs, data)

	stats, err := c.writeAssembledLayer(context.Background(), dgst.String(), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if stats.verified {
		t.Fatal("verified = true for a blob this call never read")
	}
}

// useFreshAssemblyBuffers swaps in an empty process-wide buffer pool for the
// test, so its counters start from zero. No assembly may be running.
func useFreshAssemblyBuffers(t *testing.T) *bufferPool {
	t.Helper()
	p := newBufferPool(assemblyBufferCount, maxSegmentBytes, assemblyBuffersIdle)
	old := assemblyBuffers
	assemblyBuffers = p
	t.Cleanup(func() { assemblyBuffers = old })
	return p
}

// requireAllBuffersReturned fails unless every buffer taken from p is back.
func requireAllBuffersReturned(t *testing.T, p *bufferPool) {
	t.Helper()
	if n := p.outstanding.Load(); n != 0 {
		t.Fatalf("%d segment buffers not returned to the pool", n)
	}
	if n := p.live.Load(); n > assemblyBufferCount {
		t.Fatalf("%d segment buffers exist, want at most %d", n, assemblyBufferCount)
	}
}

// TestAnIdleBufferPoolFreesItsBuffers: the agent runs for months between
// deploys, so the pool does not keep 48 MiB alive once assemblies stop. It
// frees its buffers after sitting idle, and allocates them again on demand.
func TestAnIdleBufferPoolFreesItsBuffers(t *testing.T) {
	p := newBufferPool(assemblyBufferCount, 1<<10, 20*time.Millisecond)
	var bufs [][]byte
	for range 3 {
		b, err := p.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		bufs = append(bufs, b)
	}
	p.release(bufs[0])
	time.Sleep(50 * time.Millisecond) // longer than idle, but buffers are still out
	if n := p.live.Load(); n != 3 {
		t.Fatalf("%d buffers live while two are out, want all 3 kept", n)
	}
	for _, b := range bufs[1:] {
		p.release(b)
	}
	waitFor(t, "the idle pool to free its buffers", func() bool { return p.live.Load() == 0 })

	b, err := p.acquire(context.Background())
	if err != nil || len(b) != 1<<10 {
		t.Fatalf("acquire after a trim = %d bytes, %v", len(b), err)
	}
	p.release(b)
	if n := p.live.Load(); n != 1 {
		t.Fatalf("%d buffers live after one acquire, want 1", n)
	}
}

// waitFor polls cond until it holds, failing the test after 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// stageInterleavedLayer returns a layer alternating an indexed blob's chunks
// with freshly staged ones, so every chunk is a segment of its own: a layer
// many segments long without megabytes of fixture. It returns the layer and
// its chunk hashes, one per planned segment.
func stageInterleavedLayer(t *testing.T, c *Client, cs content.Store, seed int64) ([]byte, [][32]byte) {
	t.Helper()
	prev := randomBytes(seed, 512_000)
	_, prevRefs := commitIndexedBlob(t, c, cs, prev)
	var (
		layer  []byte
		hashes [][32]byte
	)
	for i, r := range prevRefs {
		layer = append(layer, prev[r.Offset:r.Offset+r.Len]...)
		fresh := randomBytes(seed*1000+int64(i), 5_000)
		h := sha256.Sum256(fresh)
		if err := c.StageChunk(context.Background(), h, fresh); err != nil {
			t.Fatal(err)
		}
		layer = append(layer, fresh...)
		hashes = append(hashes, r.Hash, h)
	}
	if len(hashes) < 2*assemblyBufferCount {
		t.Fatalf("fixture plans %d segments, want at least %d", len(hashes), 2*assemblyBufferCount)
	}
	return layer, hashes
}

// TestConcurrentAssembliesShareTheBufferPool: Compose prepares up to four
// services at once, and every assembly draws its segment buffers from one
// pool. Four assemblies of distinct layers, many segments each, with their
// writers held until the readers have drained the pool, all complete, and no
// more than assemblyBufferCount buffers ever exist.
func TestConcurrentAssembliesShareTheBufferPool(t *testing.T) {
	pool := useFreshAssemblyBuffers(t)
	backend, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	c := newStoreClient(t, &hookStore{Store: backend, onWrite: func(w content.Writer, p []byte) (int, error) {
		<-gate
		return w.Write(p)
	}})
	type fixture struct {
		layer  []byte
		hashes [][32]byte
	}
	var layers []fixture
	for i := range 4 {
		layer, hashes := stageInterleavedLayer(t, c, backend, int64(60+i))
		layers = append(layers, fixture{layer, hashes})
	}

	errs := make(chan error, len(layers))
	for _, l := range layers {
		go func() {
			errs <- c.AssembleLayerFromChunks(context.Background(), digest.FromBytes(l.layer).String(), l.hashes)
		}()
	}
	waitFor(t, "the readers to drain the pool", func() bool { return pool.outstanding.Load() == assemblyBufferCount })
	close(gate)
	for range layers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	for i, l := range layers {
		if got := readBlob(t, backend, digest.FromBytes(l.layer)); !bytes.Equal(got, l.layer) {
			t.Fatalf("layer %d: the committed blob is not the layer", i)
		}
	}
	if n := pool.peak.Load(); n != assemblyBufferCount {
		t.Fatalf("at most %d segment buffers out at once, want the pool's %d", n, assemblyBufferCount)
	}
	requireAllBuffersReturned(t, pool)
}

// TestACancelledAssemblyReturnsItsBuffers: a deploy cancelled mid-layer, with
// the reader's pipeline full, gives every buffer back and reports the
// cancellation, not whatever the interrupted Write returned.
func TestACancelledAssemblyReturnsItsBuffers(t *testing.T) {
	pool := useFreshAssemblyBuffers(t)
	backend, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newStoreClient(t, &hookStore{Store: backend, onWrite: func(content.Writer, []byte) (int, error) {
		waitFor(t, "the reader to fill its pipeline", func() bool { return pool.outstanding.Load() == assemblyBufferCount })
		cancel()
		return 0, errors.New("failed to send write: transport is closing")
	}})
	layer, hashes := stageInterleavedLayer(t, c, backend, 70)
	diffID := digest.FromBytes(layer)

	err = c.AssembleLayerFromChunks(ctx, diffID.String(), hashes)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), diffID.String()) {
		t.Fatalf("error = %v, want the cancellation, naming the layer", err)
	}
	if _, err := backend.Info(context.Background(), diffID); err == nil {
		t.Fatal("a cancelled assembly committed its layer")
	}
	requireAllBuffersReturned(t, pool)
}

// TestACancelledCommitReportsTheCancellation: a Commit that fails because
// the deploy was cancelled reports the cancellation too.
func TestACancelledCommitReportsTheCancellation(t *testing.T) {
	backend, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newStoreClient(t, &hookStore{Store: backend, onCommit: func(context.Context, content.Writer, int64, digest.Digest, ...content.Opt) error {
		cancel()
		return errors.New("commit failed: transport is closing")
	}})
	diffID, hashes := stageLayer(t, c, 300_000, 71)

	err = c.AssembleLayerFromChunks(ctx, diffID.String(), hashes)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), diffID.String()) {
		t.Fatalf("error = %v, want the cancellation, naming the layer", err)
	}
}

// TestReadSegmentsReadsNothingOnceCancelled: the reader checks its context
// before each segment rather than reading on until its next send.
func TestReadSegmentsReadsNothingOnceCancelled(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	blob := randomBytes(72, 512_000)
	_, refs := commitIndexedBlob(t, c, cs, blob)
	// Every other chunk: runs that are not contiguous, one segment each.
	var hashes [][32]byte
	for i := 0; i < len(refs); i += 2 {
		hashes = append(hashes, refs[i].Hash)
	}
	segs, _, _, err := c.planAssembly(hashes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A pool with a buffer free and a closed Done are both ready, and select
	// picks between them at random: without the check, a run reads with
	// probability 1/2 or more.
	for range 20 {
		if got, err := collectSegments(c.readSegments(ctx, segs, 0, nil)); len(got) != 0 || err != nil {
			t.Fatalf("a cancelled reader delivered %d bytes, %v", len(got), err)
		}
	}
	if n := cs.reads.Load(); n != 0 {
		t.Fatalf("a cancelled reader read %d times", n)
	}
}

// TestAssembleLayerFromChunksLogsEachAssembly: WDY-3213's acceptance is
// measured on hardware from the one "Assembled layer" line each assembly
// logs. The fast path, which assembles nothing, logs none.
func TestAssembleLayerFromChunksLogsEachAssembly(t *testing.T) {
	c, cs := newCountingStoreClient(t)
	core, logs := observer.New(zap.InfoLevel)
	c.logger = zap.New(core)
	previous := randomBytes(90, 1<<20)
	_, prevRefs := commitIndexedBlob(t, c, cs, previous)
	fresh := randomBytes(91, 40_000)
	var hashes [][32]byte
	for _, r := range prevRefs {
		hashes = append(hashes, r.Hash)
	}
	hashes = append(hashes, stageChunks(t, c, fresh)...)
	layer := append(bytes.Clone(previous), fresh...)
	diffID := digest.FromBytes(layer)
	writePartialIngest(t, cs, layer, 100_000)

	for range 2 { // the second call takes the fast path
		if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
			t.Fatal(err)
		}
	}

	lines := logs.FilterMessage("Assembled layer").All()
	if len(lines) != 1 {
		t.Fatalf("logged %d assembly lines, want 1", len(lines))
	}
	fields := lines[0].ContextMap()
	for k, want := range map[string]any{
		"diff_id":       diffID.String(),
		"size":          int64(len(layer)),
		"segments":      int64(2), // the previous blob's run, then the staged chunks
		"staged_bytes":  int64(len(fresh)),
		"indexed_bytes": int64(len(previous)),
		"resumed_bytes": int64(100_000),
		"blobs":         int64(1),
		"verified":      true,
	} {
		if fields[k] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", k, fields[k], fields[k], want, want)
		}
	}
	if took, ok := fields["took"].(time.Duration); !ok || took <= 0 {
		t.Errorf("took = %v, want the assembly's duration", fields["took"])
	}
}

// TestAFailedAssemblyReturnsItsBuffers: a corrupt staged chunk mid-layer
// fails the assembly and gives every buffer back.
func TestAFailedAssemblyReturnsItsBuffers(t *testing.T) {
	pool := useFreshAssemblyBuffers(t)
	c, backend := newLocalStoreClient(t)
	layer, hashes := stageInterleavedLayer(t, c, backend, 80)
	corrupt := hashes[len(hashes)/2|1] // odd positions are staged chunks
	if err := os.WriteFile(c.staging.path(corrupt), randomBytes(81, 5_000), 0o600); err != nil {
		t.Fatal(err)
	}
	diffID := digest.FromBytes(layer)

	err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("error = %v, want a hash mismatch", err)
	}
	if _, err := backend.Info(context.Background(), diffID); err == nil {
		t.Fatal("a layer with a corrupt chunk was committed")
	}
	requireAllBuffersReturned(t, pool)
}
