package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"testing"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

// stagedSource returns a chunkSource backed by an in-memory map, mirroring how
// AssembleLayerFromChunks resolves staged/indexed chunks to bytes.
func stagedSource(m map[[32]byte][]byte) chunkSource {
	return func(h [32]byte) ([]byte, error) {
		if b, ok := m[h]; ok {
			return b, nil
		}
		return nil, nil
	}
}

func TestChunkStreamReassembles(t *testing.T) {
	full := bytes.Repeat([]byte("wendy-layer-"), 50_000) // ~600 KiB, multi-chunk
	refs, err := chunk.Chunk(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}

	src := map[[32]byte][]byte{}
	var order [][32]byte
	off := 0
	for _, r := range refs {
		src[r.Hash] = full[off : off+int(r.Len)]
		order = append(order, r.Hash)
		off += int(r.Len)
	}

	got, err := io.ReadAll(&chunkStream{order: order, src: stagedSource(src)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("reassembled bytes differ (len %d vs %d)", len(got), len(full))
	}
	if sha256.Sum256(got) != sha256.Sum256(full) {
		t.Fatal("digest mismatch")
	}
}

func TestChunkStreamDetectsCorruptChunk(t *testing.T) {
	good := []byte("good-chunk-bytes")
	h := sha256.Sum256(good)
	// Source returns bytes that do not match the requested hash.
	src := func([32]byte) ([]byte, error) { return []byte("tampered"), nil }

	_, err := io.ReadAll(&chunkStream{order: [][32]byte{h}, src: src})
	if err == nil {
		t.Fatal("expected hash-mismatch error for tampered chunk")
	}
}

func TestChunkStreamReportsMissingChunk(t *testing.T) {
	h := sha256.Sum256([]byte("absent"))
	src := func([32]byte) ([]byte, error) { return nil, nil } // unavailable

	_, err := io.ReadAll(&chunkStream{order: [][32]byte{h}, src: src})
	if err == nil {
		t.Fatal("expected error for unavailable chunk")
	}
}

func TestStageChunkRejectsOversizedChunk(t *testing.T) {
	c := &Client{staging: newStaging(t.TempDir())}
	big := bytes.Repeat([]byte{0xab}, maxStagedChunkBytes+1)
	err := c.StageChunk(context.Background(), sha256.Sum256(big), big)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for oversized chunk, got %v", err)
	}
}

func TestStageChunkRejectsHashMismatch(t *testing.T) {
	c := &Client{staging: newStaging(t.TempDir())}
	data := []byte("real-bytes")
	wrong := sha256.Sum256([]byte("something-else"))
	if err := c.StageChunk(context.Background(), wrong, data); err == nil {
		t.Fatal("expected error when data does not match the claimed hash")
	}
	if c.staging.has(wrong) {
		t.Fatal("a hash-mismatched chunk must not be staged")
	}
}

func TestStagingWriteIsDiskBackedAndIdempotent(t *testing.T) {
	s := newStaging(t.TempDir())
	data := []byte("chunk-payload")
	h := sha256.Sum256(data)

	if s.has(h) {
		t.Fatal("chunk should not exist before staging")
	}
	if err := s.write(h, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !s.has(h) {
		t.Fatal("chunk should exist on disk after staging")
	}
	if n, ok := s.statLen(h); !ok || n != int64(len(data)) {
		t.Fatalf("statLen = (%d, %v), want (%d, true)", n, ok, len(data))
	}
	got, err := s.read(h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read = (%q, %v), want %q", got, err, data)
	}

	// Re-staging the same chunk is a no-op and must not error.
	if err := s.write(h, data); err != nil {
		t.Fatalf("idempotent re-write: %v", err)
	}

	s.remove(h)
	if s.has(h) {
		t.Fatal("chunk should be gone after remove")
	}
	// Removing an absent chunk is safe.
	s.remove(h)
}

type chunkAvailabilityContentStore struct {
	content.Store
	blobs map[digest.Digest]content.Info
	// errs fails Info for a digest with an error other than NotFound, as
	// containerd does while it cannot answer.
	errs map[digest.Digest]error
}

func (s *chunkAvailabilityContentStore) Info(_ context.Context, dgst digest.Digest) (content.Info, error) {
	if err, ok := s.errs[dgst]; ok {
		return content.Info{}, err
	}
	if info, ok := s.blobs[dgst]; ok {
		return info, nil
	}
	return content.Info{}, errdefs.ErrNotFound
}

func newChunkAvailabilityClient(t *testing.T, cs content.Store, index *ChunkIndex, stagingDir string) *Client {
	t.Helper()
	client, err := containerdclient.New("",
		containerdclient.WithDefaultNamespace("default"),
		containerdclient.WithServices(containerdclient.WithContentStore(cs)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &Client{client: client, logger: zap.NewNop(), chunkIndex: index, staging: newStaging(stagingDir)}
}

func TestMissingChunksPrunesIndexEntriesWhoseBlobWasGarbageCollected(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "chunk-index.db")
	index, err := OpenChunkIndex(indexPath, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("chunk whose indexed layer was garbage collected")
	hash := sha256.Sum256(data)
	staleBlob := digest.FromString("stale uncompressed layer")
	if err := index.AddLayer(staleBlob.String(), []chunk.Ref{{Hash: hash, Len: uint64(len(data))}}); err != nil {
		t.Fatal(err)
	}

	cs := &chunkAvailabilityContentStore{blobs: map[digest.Digest]content.Info{}}
	c := newChunkAvailabilityClient(t, cs, index, filepath.Join(dir, "staging"))
	missing, err := c.MissingChunks(context.Background(), [][32]byte{hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != hash {
		t.Fatalf("missing = %x, want %x", missing, hash)
	}
	if _, ok := index.Has(hash); ok {
		t.Fatal("stale chunk entry was not pruned")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenChunkIndex(indexPath, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Has(hash); ok {
		t.Fatal("pruned chunk entry remained in the persisted index")
	}
}

// TestMissingChunksDropsEveryStaleBlobInOneTransaction: the first query after
// the legacy import can find hundreds of stale blobs, under the sweep lock's
// read side. They go in one batch, not a transaction each.
func TestMissingChunksDropsEveryStaleBlobInOneTransaction(t *testing.T) {
	index := newTestChunkIndex(t)
	var hashes [][32]byte
	for i := range 3 {
		data := fmt.Appendf(nil, "chunk %d whose indexed layer was garbage collected", i)
		hash := sha256.Sum256(data)
		hashes = append(hashes, hash)
		blob := digest.FromString(fmt.Sprint("stale layer ", i)).String()
		if err := index.AddLayer(blob, []chunk.Ref{{Hash: hash, Len: uint64(len(data))}}); err != nil {
			t.Fatal(err)
		}
	}
	cs := &chunkAvailabilityContentStore{blobs: map[digest.Digest]content.Info{}}
	c := newChunkAvailabilityClient(t, cs, index, filepath.Join(t.TempDir(), "staging"))

	before := committedTxID(t, index)
	missing, err := c.MissingChunks(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(hashes) {
		t.Fatalf("missing = %x, want all %d chunks", missing, len(hashes))
	}
	if n := committedTxID(t, index) - before; n != 1 {
		t.Fatalf("dropping %d stale blobs took %d transactions, want 1", len(hashes), n)
	}
	if blobs, err := index.Blobs(); err != nil || len(blobs) != 0 {
		t.Fatalf("Blobs = %v, %v; want every stale blob dropped", blobs, err)
	}
}

func TestMissingChunksKeepsIndexEntryBackedByContentBlob(t *testing.T) {
	dir := t.TempDir()
	index := newTestChunkIndex(t)
	data := []byte("available indexed chunk")
	hash := sha256.Sum256(data)
	blob := digest.FromString("available uncompressed layer")
	if err := index.AddLayer(blob.String(), []chunk.Ref{{Hash: hash, Offset: 4, Len: uint64(len(data))}}); err != nil {
		t.Fatal(err)
	}

	cs := &chunkAvailabilityContentStore{blobs: map[digest.Digest]content.Info{
		blob: {Digest: blob, Size: int64(len(data)) + 4},
	}}
	c := newChunkAvailabilityClient(t, cs, index, filepath.Join(dir, "staging"))
	missing, err := c.MissingChunks(context.Background(), [][32]byte{hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %x, want none", missing)
	}
}

// TestMissingChunksTreatsIndexLookupErrorAsNotIndexed proves the chunk index
// is purely a cache: a Lookup failure (here, a closed index) must not fail the
// call. Every candidate is reported missing, as if the index held none of it,
// so the CLI simply re-sends the chunk instead of the deploy erroring out.
func TestMissingChunksTreatsIndexLookupErrorAsNotIndexed(t *testing.T) {
	dir := t.TempDir()
	index := newTestChunkIndex(t)
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	cs := &chunkAvailabilityContentStore{blobs: map[digest.Digest]content.Info{}}
	c := newChunkAvailabilityClient(t, cs, index, filepath.Join(dir, "staging"))
	data := []byte("chunk whose index lookup errors because the index is closed")
	hash := sha256.Sum256(data)

	missing, err := c.MissingChunks(context.Background(), [][32]byte{hash})
	if err != nil {
		t.Fatalf("MissingChunks must not fail when the chunk index lookup errors: %v", err)
	}
	if len(missing) != 1 || missing[0] != hash {
		t.Fatalf("missing = %x, want the one candidate reported missing", missing)
	}
}

// readOnlyChunkIndex returns an index holding refs as ranges of blob, on which
// every write fails, as on a full or failing disk.
func readOnlyChunkIndex(t *testing.T, blob string, refs ...chunk.Ref) *ChunkIndex {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chunk-index.db")
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.AddLayer(blob, refs); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &ChunkIndex{db: db, path: path}
}

// TestMissingChunksSurvivesAFailedDrop: pruning stale entries is best effort.
// A drop that fails (here on a read-only index; on a device ENOSPC or EIO) must
// not fail the query, since the stale chunks are reported missing anyway.
func TestMissingChunksSurvivesAFailedDrop(t *testing.T) {
	data := []byte("chunk whose indexed layer was garbage collected")
	hash := sha256.Sum256(data)
	index := readOnlyChunkIndex(t, digest.FromString("stale layer").String(), chunk.Ref{Hash: hash, Len: uint64(len(data))})
	cs := &chunkAvailabilityContentStore{blobs: map[digest.Digest]content.Info{}}
	c := newChunkAvailabilityClient(t, cs, index, filepath.Join(t.TempDir(), "staging"))
	core, logs := observer.New(zap.WarnLevel)
	c.logger = zap.New(core)

	missing, err := c.MissingChunks(context.Background(), [][32]byte{hash})
	if err != nil {
		t.Fatalf("a failed drop must not fail the query: %v", err)
	}
	if len(missing) != 1 || missing[0] != hash {
		t.Fatalf("missing = %x, want the stale chunk %x", missing, hash)
	}
	if n := logs.FilterMessage("Dropping stale chunk-index entries failed").Len(); n != 1 {
		t.Fatalf("logged %d failed drops, want 1", n)
	}
}

// newStoreClient builds a Client over the provided content store.
func newStoreClient(t *testing.T, store content.Store) *Client {
	t.Helper()
	client, err := containerdclient.New("",
		containerdclient.WithDefaultNamespace("default"),
		containerdclient.WithServices(containerdclient.WithContentStore(store)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &Client{
		client:     client,
		logger:     zap.NewNop(),
		namespace:  "default",
		chunkIndex: newTestChunkIndex(t),
		staging:    newStaging(filepath.Join(t.TempDir(), "staging")),
	}
}

// newLocalStoreClient builds a Client over containerd's on-disk content store,
// so assembly runs the real WriteBlob/Commit path without a daemon.
func newLocalStoreClient(t *testing.T) (*Client, content.Store) {
	t.Helper()
	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newStoreClient(t, store), store
}

// TestAssembleLayerFromChunksIndexesTheManifestRanges proves the index entries
// derived from the manifest (prefix sums of chunk lengths) name exactly the
// bytes of each chunk in the committed blob, so no re-chunking is needed.
func TestAssembleLayerFromChunksIndexesTheManifestRanges(t *testing.T) {
	c, store := newLocalStoreClient(t)
	layer := make([]byte, 700_000)
	rand.New(rand.NewSource(3)).Read(layer)
	refs, err := chunk.ChunkBytes(layer)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][32]byte, len(refs))
	for i, r := range refs {
		hashes[i] = r.Hash
		if err := c.StageChunk(context.Background(), r.Hash, layer[r.Offset:r.Offset+r.Len]); err != nil {
			t.Fatal(err)
		}
	}
	diffID := digest.FromBytes(layer)

	if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
		t.Fatal(err)
	}

	ra, err := store.ReaderAt(context.Background(), ocispec.Descriptor{Digest: diffID})
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()
	for i, r := range refs {
		loc, ok := c.chunkIndex.Has(r.Hash)
		if !ok || loc.Blob != diffID.String() || loc.Offset != r.Offset || loc.Len != r.Len {
			t.Fatalf("chunk %d indexed as %+v (%v), want %s@%d+%d", i, loc, ok, diffID, r.Offset, r.Len)
		}
		buf := make([]byte, loc.Len)
		if _, err := ra.ReadAt(buf, int64(loc.Offset)); err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(buf) != r.Hash {
			t.Fatalf("chunk %d's indexed range does not hold its bytes", i)
		}
		if c.staging.has(r.Hash) {
			t.Fatalf("chunk %d still staged after assembly", i)
		}
	}
}

// stageLayer stages a random layer's chunks and returns its diff ID and chunk
// hashes, in order.
func stageLayer(t *testing.T, c *Client, size int, seed int64) (digest.Digest, [][32]byte) {
	t.Helper()
	layer := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(layer)
	return digest.FromBytes(layer), stageChunks(t, c, layer)
}

// stageChunks stages data's chunks and returns their hashes, in order.
func stageChunks(t *testing.T, c *Client, data []byte) [][32]byte {
	t.Helper()
	refs, err := chunk.ChunkBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][32]byte, len(refs))
	for i, r := range refs {
		hashes[i] = r.Hash
		if err := c.StageChunk(context.Background(), r.Hash, data[r.Offset:r.Offset+r.Len]); err != nil {
			t.Fatal(err)
		}
	}
	return hashes
}

// TestAssembleLayerFromChunksKeepsStagedChunksItCouldNotIndex: staged chunks
// go only once the index holds them. A concurrent assembly of another layer
// sharing them may already count on them, and would otherwise find them in
// neither staging nor the index.
func TestAssembleLayerFromChunksKeepsStagedChunksItCouldNotIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index func(t *testing.T) *ChunkIndex
	}{
		{"disabled index", func(*testing.T) *ChunkIndex { return &ChunkIndex{} }},
		{"index broken by a corrupt page", func(t *testing.T) *ChunkIndex {
			ix := newTestChunkIndex(t)
			ix.broken.Store(true)
			return ix
		}},
		{"index write fails", func(t *testing.T) *ChunkIndex {
			return readOnlyChunkIndex(t, digest.FromString("another layer").String(), chunk.Ref{Hash: [32]byte{9}, Len: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, store := newLocalStoreClient(t)
			c.chunkIndex = tc.index(t)
			diffID, hashes := stageLayer(t, c, 300_000, 5)

			if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), hashes); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Info(context.Background(), diffID); err != nil {
				t.Fatalf("layer not committed: %v", err)
			}
			for i, h := range hashes {
				if !c.staging.has(h) {
					t.Fatalf("chunk %d unstaged although the index did not record it", i)
				}
			}
		})
	}
}

// racedContentStore acts as if a concurrent assembly of the same diff ID
// committed the blob between the fast-path check and the write: the first Info
// misses, and the writer then finds the blob already there.
type racedContentStore struct {
	content.Store
	infoCalls int
	size      int64
}

func (s *racedContentStore) Info(_ context.Context, d digest.Digest) (content.Info, error) {
	s.infoCalls++
	if s.infoCalls == 1 {
		return content.Info{}, errdefs.ErrNotFound
	}
	return content.Info{Digest: d, Size: s.size}, nil
}

func (s *racedContentStore) Writer(context.Context, ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrAlreadyExists
}

// TestAssembleLayerFromChunksDoesNotIndexAManifestItNeverVerified: when a
// concurrent assembly commits the same layer first, WriteLayer returns without
// reading the chunks, so nothing checked this manifest against the blob. A
// wrong one, from a buggy or hostile client, must not be indexed: MissingChunks
// would report its chunks present, and every later assembly would then fail
// to verify them.
func TestAssembleLayerFromChunksDoesNotIndexAManifestItNeverVerified(t *testing.T) {
	a, b := []byte("chunk A, which is not in the layer"), []byte("chunk B, which is not in it either")
	committed := make([]byte, len(a)+len(b)) // the winning assembly wrote other bytes
	diffID := digest.FromBytes(committed)
	c := newChunkAvailabilityClient(t, &racedContentStore{size: int64(len(committed))}, newTestChunkIndex(t), filepath.Join(t.TempDir(), "staging"))
	ha, hb := sha256.Sum256(a), sha256.Sum256(b)
	for h, data := range map[[32]byte][]byte{ha: a, hb: b} {
		if err := c.StageChunk(context.Background(), h, data); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.AssembleLayerFromChunks(context.Background(), diffID.String(), [][32]byte{ha, hb}); err != nil {
		t.Fatal(err)
	}
	for _, h := range [][32]byte{ha, hb} {
		if loc, ok := c.chunkIndex.Has(h); ok {
			t.Fatalf("an unverified manifest was indexed: chunk %x at %+v", h, loc)
		}
		if !c.staging.has(h) {
			t.Fatalf("chunk %x unstaged although nothing indexed it", h)
		}
	}
}
