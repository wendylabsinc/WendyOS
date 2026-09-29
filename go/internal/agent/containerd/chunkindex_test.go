package containerd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

// newTestChunkIndex opens an index in a temp dir, closed when the test ends.
func newTestChunkIndex(t *testing.T) *ChunkIndex {
	t.Helper()
	ix, err := OpenChunkIndex(filepath.Join(t.TempDir(), "chunk-index.db"), "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestChunkIndexAddLookup(t *testing.T) {
	ix := newTestChunkIndex(t)
	blob := digest.FromString("layer a").String()
	h1, h2 := [32]byte{1}, [32]byte{2}
	if err := ix.AddLayer(blob, []chunk.Ref{{Hash: h1, Offset: 7, Len: 10}}); err != nil {
		t.Fatal(err)
	}

	loc, ok := ix.Has(h1)
	if !ok || loc != (chunkLoc{Blob: blob, Offset: 7, Len: 10}) {
		t.Fatalf("Has(h1) = %+v, %v", loc, ok)
	}
	locs, found, err := ix.Lookup([][32]byte{h1, h2})
	if err != nil {
		t.Fatal(err)
	}
	if !found[0] || found[1] || locs[0].Offset != 7 {
		t.Fatalf("Lookup = %+v %v", locs, found)
	}
}

func TestChunkIndexPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk-index.db")
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	blob := digest.FromString("layer b").String()
	h := [32]byte{9}
	if err := ix.AddLayer(blob, []chunk.Ref{{Hash: h, Offset: 5, Len: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Has(h); !ok {
		t.Fatal("entry lost across reopen")
	}
}

func TestChunkIndexDropRemovesOnlyThatBlobsEntries(t *testing.T) {
	ix := newTestChunkIndex(t)
	older, newer := digest.FromString("old layer").String(), digest.FromString("new layer").String()
	shared, onlyOld, onlyNew := [32]byte{1}, [32]byte{2}, [32]byte{3}
	if err := ix.AddLayer(older, []chunk.Ref{{Hash: shared, Len: 1}, {Hash: onlyOld, Offset: 1, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	// The newer blob re-indexes the shared chunk: last writer wins.
	if err := ix.AddLayer(newer, []chunk.Ref{{Hash: shared, Offset: 4, Len: 1}, {Hash: onlyNew, Offset: 5, Len: 1}}); err != nil {
		t.Fatal(err)
	}

	if err := ix.Drop(older); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Has(onlyOld); ok {
		t.Fatal("entry of the dropped blob survived")
	}
	if loc, ok := ix.Has(shared); !ok || loc.Blob != newer || loc.Offset != 4 {
		t.Fatalf("shared chunk = %+v, %v; want it still in the newer blob", loc, ok)
	}
	if _, ok := ix.Has(onlyNew); !ok {
		t.Fatal("entry of the surviving blob was removed")
	}
	blobs, err := ix.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || blobs[0] != newer {
		t.Fatalf("Blobs = %v, want [%s]", blobs, newer)
	}
	if n, err := ix.Len(); err != nil || n != 2 {
		t.Fatalf("Len = %d, %v; want 2", n, err)
	}
}

// committedTxID is the id of the index's last committed write transaction;
// each one adds 1.
func committedTxID(t *testing.T, ix *ChunkIndex) int {
	t.Helper()
	var id int
	if err := ix.db.View(func(tx *bolt.Tx) error {
		id = tx.ID()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestChunkIndexAddLayerAndDropSpanTransactions: indexing or dropping a layer
// of more than chunkIndexTxEntries chunks, as a multi-GB one has, takes
// several bounded transactions rather than one huge one.
func TestChunkIndexAddLayerAndDropSpanTransactions(t *testing.T) {
	ix := newTestChunkIndex(t)
	blob := digest.FromString("big layer").String()
	refs := make([]chunk.Ref, chunkIndexTxEntries+10)
	for i := range refs {
		refs[i] = chunk.Ref{Hash: [32]byte{byte(i), byte(i >> 8), byte(i >> 16), 0xaa}, Offset: uint64(i), Len: 1}
	}
	before := committedTxID(t, ix)
	if err := ix.AddLayer(blob, refs); err != nil {
		t.Fatal(err)
	}
	if n := committedTxID(t, ix) - before; n != 2 {
		t.Fatalf("AddLayer took %d transactions, want 2", n)
	}
	if n, err := ix.Len(); err != nil || n != len(refs) {
		t.Fatalf("Len = %d, %v; want %d", n, err, len(refs))
	}

	before = committedTxID(t, ix)
	if err := ix.Drop(blob); err != nil {
		t.Fatal(err)
	}
	if n := committedTxID(t, ix) - before; n != 2 {
		t.Fatalf("Drop took %d transactions, want 2", n)
	}
	if n, err := ix.Len(); err != nil || n != 0 {
		t.Fatalf("Len after Drop = %d, %v; want 0", n, err)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 0 {
		t.Fatalf("Blobs after Drop = %v, %v; want none", blobs, err)
	}
}

// TestChunkIndexDropBlobsSharesTransactionsAcrossBlobs: dropping many small
// blobs, as the first reconcile after the legacy import does, costs one
// transaction rather than one per blob, and still spares a hash that a newer
// blob re-indexed.
func TestChunkIndexDropBlobsSharesTransactionsAcrossBlobs(t *testing.T) {
	ix := newTestChunkIndex(t)
	shared := [32]byte{0xff}
	var stale []string
	for b := range 50 {
		blob := digest.FromString(fmt.Sprint("stale layer ", b)).String()
		stale = append(stale, blob)
		refs := []chunk.Ref{{Hash: [32]byte{byte(b), 1}, Len: 1}, {Hash: [32]byte{byte(b), 2}, Offset: 1, Len: 1}}
		if b == 0 {
			refs = append(refs, chunk.Ref{Hash: shared, Offset: 2, Len: 1})
		}
		if err := ix.AddLayer(blob, refs); err != nil {
			t.Fatal(err)
		}
	}
	newer := digest.FromString("newer layer").String()
	if err := ix.AddLayer(newer, []chunk.Ref{{Hash: shared, Offset: 9, Len: 1}}); err != nil {
		t.Fatal(err)
	}

	before := committedTxID(t, ix)
	dropped, err := ix.DropBlobs(stale)
	if err != nil || dropped != len(stale) {
		t.Fatalf("DropBlobs = %d, %v; want %d", dropped, err, len(stale))
	}
	if n := committedTxID(t, ix) - before; n != 1 {
		t.Fatalf("dropping %d blobs took %d transactions, want 1", len(stale), n)
	}
	if loc, ok := ix.Has(shared); !ok || loc.Blob != newer || loc.Offset != 9 {
		t.Fatalf("shared chunk = %+v, %v; want it still in the newer blob", loc, ok)
	}
	if n, err := ix.Len(); err != nil || n != 1 {
		t.Fatalf("Len = %d, %v; want only the newer blob's entry", n, err)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 1 || blobs[0] != newer {
		t.Fatalf("Blobs = %v, %v; want [%s]", blobs, err, newer)
	}
}

// TestChunkIndexDropListsABlobUntilItsLastEntryIsGone: a drop cut short
// between transactions leaves the blob listed with its remaining entries, so
// the next reconcile finds it and finishes.
func TestChunkIndexDropListsABlobUntilItsLastEntryIsGone(t *testing.T) {
	ix := newTestChunkIndex(t)
	blob := digest.FromString("layer").String()
	refs := make([]chunk.Ref, 10)
	for i := range refs {
		refs[i] = chunk.Ref{Hash: [32]byte{byte(i + 1)}, Offset: uint64(i), Len: 1}
	}
	if err := ix.AddLayer(blob, refs); err != nil {
		t.Fatal(err)
	}
	key, err := blobKey(blob)
	if err != nil {
		t.Fatal(err)
	}
	dropWithin := func(budget int) (done int) {
		t.Helper()
		if err := ix.update(func(tx *bolt.Tx) (err error) {
			done, err = dropBlobsTx(tx, [][32]byte{key}, budget)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return done
	}

	if done := dropWithin(4); done != 0 {
		t.Fatalf("finished %d blobs with 6 entries left, want 0", done)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 1 {
		t.Fatalf("Blobs = %v, %v; a blob with entries left must stay listed", blobs, err)
	}
	if n, err := ix.Len(); err != nil || n != 6 {
		t.Fatalf("Len = %d, %v; want the 6 entries not yet dropped", n, err)
	}
	if done := dropWithin(6); done != 1 {
		t.Fatalf("finished %d blobs with a budget covering the rest, want 1", done)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 0 {
		t.Fatalf("Blobs = %v, %v; want none", blobs, err)
	}
	if n, err := ix.Len(); err != nil || n != 0 {
		t.Fatalf("Len = %d, %v; want 0", n, err)
	}
}

func TestChunkIndexRejectsNonSHA256Blob(t *testing.T) {
	ix := newTestChunkIndex(t)
	if err := ix.AddLayer("sha512:"+hex.EncodeToString(make([]byte, 64)), []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err == nil {
		t.Fatal("expected an error for a non-sha256 blob digest")
	}
}

func TestOpenChunkIndexImportsAndRemovesLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "chunk-index.json")
	blob := digest.FromString("legacy layer").String()
	h := [32]byte{4}
	data, err := json.Marshal(map[string]chunkLoc{
		hex.EncodeToString(h[:]): {Blob: blob, Offset: 3, Len: 9},
		"not-hex":                {Blob: blob, Offset: 0, Len: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := OpenChunkIndex(filepath.Join(dir, "chunk-index.db"), legacy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if loc, ok := ix.Has(h); !ok || loc != (chunkLoc{Blob: blob, Offset: 3, Len: 9}) {
		t.Fatalf("migrated entry = %+v, %v", loc, ok)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy JSON still present: %v", err)
	}
}

// TestOpenChunkIndexImportsManyLegacyBlobsAsAddLayerWould replays a history of
// layers that share chunks. The legacy agent kept only the last layer to index
// each chunk. Importing its JSON must give the index that AddLayer gives for
// the same layers in the same order: the same entries, the same blobs, and
// drops that agree.
func TestOpenChunkIndexImportsManyLegacyBlobsAsAddLayerWould(t *testing.T) {
	// 45 layers of 400 unique chunks, each also holding 50 of 60 shared ones:
	// 18060 entries, so the import spans two transactions.
	const layers, unique, sharedPerLayer = 45, 400, 50
	shared := make([][32]byte, 60)
	for i := range shared {
		shared[i] = sha256.Sum256(fmt.Appendf(nil, "shared chunk %d", i))
	}
	want := newTestChunkIndex(t)
	want.db.NoSync = true // it only has to be right, not durable
	legacy := map[string]chunkLoc{}
	var blobs []string
	for l := range layers {
		blob := digest.FromString(fmt.Sprint("layer ", l)).String()
		blobs = append(blobs, blob)
		var refs []chunk.Ref
		for i := range unique {
			refs = append(refs, chunk.Ref{Hash: sha256.Sum256(fmt.Appendf(nil, "%d-%d", l, i))})
		}
		for j := range sharedPerLayer {
			refs = append(refs, chunk.Ref{Hash: shared[(7*l+j)%len(shared)]})
		}
		var offset uint64
		for i := range refs {
			refs[i].Offset, refs[i].Len = offset, 100+uint64(i)
			offset += refs[i].Len
			legacy[hex.EncodeToString(refs[i].Hash[:])] = chunkLoc{Blob: blob, Offset: refs[i].Offset, Len: refs[i].Len}
		}
		if err := want.AddLayer(blob, refs); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "chunk-index.json")
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.InfoLevel)
	got, err := OpenChunkIndex(filepath.Join(dir, "chunk-index.db"), legacyPath, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()

	hashes := make([][32]byte, 0, len(legacy))
	for key := range legacy {
		var h [32]byte
		if _, err := hex.Decode(h[:], []byte(key)); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	agree := func(when string) {
		t.Helper()
		gotLocs, gotFound, err := got.Lookup(hashes)
		if err != nil {
			t.Fatal(err)
		}
		wantLocs, wantFound, err := want.Lookup(hashes)
		if err != nil {
			t.Fatal(err)
		}
		for i := range hashes {
			if gotFound[i] != wantFound[i] || gotLocs[i] != wantLocs[i] {
				t.Fatalf("%s: chunk %x imported as %+v (%v), AddLayer gives %+v (%v)",
					when, hashes[i], gotLocs[i], gotFound[i], wantLocs[i], wantFound[i])
			}
		}
		gotBlobs, err := got.Blobs()
		if err != nil {
			t.Fatal(err)
		}
		wantBlobs, err := want.Blobs()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(gotBlobs, wantBlobs) {
			t.Fatalf("%s: imported blobs %v, AddLayer gives %v", when, gotBlobs, wantBlobs)
		}
	}
	agree("after import")
	if n, err := got.Len(); err != nil || n != len(legacy) {
		t.Fatalf("Len = %d, %v; want all %d legacy entries", n, err, len(legacy))
	}

	migrated := logs.FilterMessage("Migrated legacy chunk index").All()
	if len(migrated) != 1 {
		t.Fatalf("logged %d migrations, want 1", len(migrated))
	}
	fields := migrated[0].ContextMap()
	if fields["entries"] != int64(len(legacy)) || fields["blobs"] != int64(layers) {
		t.Fatalf("migration logged %v; want entries=%d blobs=%d", fields, len(legacy), layers)
	}
	if took, ok := fields["took"].(time.Duration); !ok || took <= 0 {
		t.Fatalf("migration logged took=%v; want the import's duration", fields["took"])
	}

	// The newest layer owns every shared chunk it holds; a middle one owns
	// only its unique chunks.
	for _, blob := range []string{blobs[layers-1], blobs[layers/2]} {
		if err := got.Drop(blob); err != nil {
			t.Fatal(err)
		}
		if err := want.Drop(blob); err != nil {
			t.Fatal(err)
		}
		agree("after dropping " + blob)
	}
}

// TestImportLegacyJSONTellsAFailedImportFromAnUnreadableFile: a write failure
// part way through the import is not an unreadable file, and the log says
// which it was. Both still remove the JSON, since the index is a cache.
func TestImportLegacyJSONTellsAFailedImportFromAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chunk-index.db")
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	// Read-only, every write fails, as it would on a full disk.
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	readOnly := &ChunkIndex{db: db}
	core, logs := observer.New(zap.InfoLevel)
	legacy := filepath.Join(dir, "chunk-index.json")
	h := [32]byte{4}
	data, err := json.Marshal(map[string]chunkLoc{hex.EncodeToString(h[:]): {Blob: digest.FromString("layer").String(), Len: 1}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, content, logged string
	}{
		{"write failure", string(data), "Legacy chunk index import failed"},
		{"unreadable file", "{not json", "Discarding unreadable legacy chunk index"},
	} {
		if err := os.WriteFile(legacy, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		readOnly.importLegacyJSON(legacy, zap.New(core))
		if entries := logs.TakeAll(); len(entries) != 1 || entries[0].Message != tc.logged {
			t.Fatalf("%s: logged %v; want only %q", tc.name, entries, tc.logged)
		}
		if _, err := os.Stat(legacy); !os.IsNotExist(err) {
			t.Fatalf("%s: legacy JSON still present: %v", tc.name, err)
		}
	}
}

// legacyChunkIndexEntries builds a legacy JSON index of entries 64 KiB chunks
// spread round-robin over blobs blobs.
func legacyChunkIndexEntries(entries, blobs int) map[string]chunkLoc {
	digests := make([]string, blobs)
	for b := range digests {
		digests[b] = digest.FromString(fmt.Sprint("blob ", b)).String()
	}
	m := make(map[string]chunkLoc, entries)
	for i := range entries {
		h := sha256.Sum256(fmt.Appendf(nil, "chunk %d", i))
		m[hex.EncodeToString(h[:])] = chunkLoc{Blob: digests[i%blobs], Offset: uint64(i/blobs) << 16, Len: 1 << 16}
	}
	return m
}

// BenchmarkOpenChunkIndexImportingLegacyJSON times the first start after the
// update from a JSON index of 145k entries, as on a long-lived device, spread
// over few or many blobs. It all runs before the agent serves any RPC, and
// `wendy device update` waits 20 s for the agent to come back.
func BenchmarkOpenChunkIndexImportingLegacyJSON(b *testing.B) {
	for _, blobs := range []int{20, 500, 2000} {
		b.Run(fmt.Sprint("blobs=", blobs), func(b *testing.B) {
			data, err := json.Marshal(legacyChunkIndexEntries(145_000, blobs))
			if err != nil {
				b.Fatal(err)
			}
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				legacy := filepath.Join(dir, "chunk-index.json")
				if err := os.WriteFile(legacy, data, 0o644); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				ix, err := OpenChunkIndex(filepath.Join(dir, "chunk-index.db"), legacy, zap.NewNop())
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if n, err := ix.Len(); err != nil || n != 145_000 {
					b.Fatalf("Len = %d, %v; want 145000", n, err)
				}
				if err := ix.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

func TestOpenChunkIndexDiscardsUnreadableLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "chunk-index.json")
	if err := os.WriteFile(legacy, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenChunkIndex(filepath.Join(dir, "chunk-index.db"), legacy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if n, _ := ix.Len(); n != 0 {
		t.Fatalf("Len = %d, want an empty index", n)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("unreadable legacy JSON still present: %v", err)
	}
}

func TestOpenChunkIndexReplacesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chunk-index.db")
	if err := os.WriteFile(path, []byte("this is not a bbolt file, but it is long enough to have meta pages"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatalf("a corrupt index must be replaced, got %v", err)
	}
	defer ix.Close()
	if err := ix.AddLayer(digest.FromString("x").String(), []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(path + ".corrupt-*")
	if len(aside) != 1 {
		t.Fatalf("corrupt file not moved aside: %v", aside)
	}
}

// bbolt page type flags (go.etcd.io/bbolt/internal/common).
const (
	boltBranchPageFlag = 0x01
	boltMetaPageFlag   = 0x04
)

// writeChunkIndexFile builds a closed index file with 4 KiB pages, as on a
// device, holding perLayer entries for each of layers blobs. Entry i of layer
// l has hash sha256("l-i").
func writeChunkIndexFile(t *testing.T, layers, perLayer int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chunk-index.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for l := range layers {
		refs := make([]chunk.Ref, perLayer)
		for i := range refs {
			refs[i] = chunk.Ref{Hash: sha256.Sum256(fmt.Appendf(nil, "%d-%d", l, i)), Offset: uint64(i), Len: 1}
		}
		if err := ix.AddLayer(digest.FromString(fmt.Sprint("layer ", l)).String(), refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// chunkIndexMeta reads the page size, and the root-bucket and freelist page
// ids, from the newer of an index file's two meta pages. A page starts with a
// 16-byte header; the meta that follows holds the page size at byte 24, the
// root bucket's page at 32, the freelist's page at 48 and the txid at 64.
// bbolt writes them in host byte order, little-endian wherever the agent runs.
func chunkIndexMeta(t *testing.T, path string) (pageSize int, root, freelist uint64) {
	t.Helper()
	f, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize = int(binary.LittleEndian.Uint32(f[24:28]))
	newest, newestTx := 0, uint64(0)
	for i := range 2 {
		if tx := binary.LittleEndian.Uint64(f[i*pageSize+64:]); tx >= newestTx {
			newest, newestTx = i, tx
		}
	}
	meta := f[newest*pageSize:]
	return pageSize, binary.LittleEndian.Uint64(meta[32:40]), binary.LittleEndian.Uint64(meta[48:56])
}

// setChunkIndexPageFlags overwrites one page's type flags, as a torn write
// would. The meta checksums cover only the meta pages, so bbolt cannot see it.
func setChunkIndexPageFlags(t *testing.T, path string, pageSize int, pgid uint64, flags uint16) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(binary.LittleEndian.AppendUint16(nil, flags), int64(pgid)*int64(pageSize)+8); err != nil {
		t.Fatal(err)
	}
}

// chunkIndexBucketPage returns the page holding the root of a bucket that is
// too big to live inline in its parent's page.
func chunkIndexBucketPage(t *testing.T, path string, bucket []byte) uint64 {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pgid uint64
	if err := db.View(func(tx *bolt.Tx) error {
		pgid = uint64(tx.Bucket(bucket).Root())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pgid == 0 {
		t.Fatalf("bucket %s is inline; the test index needs more entries", bucket)
	}
	return pgid
}

// corruptChunkIndexFiles lists the files OpenChunkIndex moved aside.
func corruptChunkIndexFiles(t *testing.T, path string) []string {
	t.Helper()
	aside, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	return aside
}

// TestOpenChunkIndexReplacesAFileWithACorruptPage damages a real index file the
// way a torn write or a lost tail would, where the meta checksums cannot see
// it. bbolt panics or faults on such pages rather than returning an error, so
// without recovery the agent would crash-loop on every start.
func TestOpenChunkIndexReplacesAFileWithACorruptPage(t *testing.T) {
	truncate := func(t *testing.T, path string, size int64) {
		t.Helper()
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{"freelist page", func(t *testing.T, path string) {
			pageSize, _, freelist := chunkIndexMeta(t, path)
			setChunkIndexPageFlags(t, path, pageSize, freelist, boltBranchPageFlag)
		}},
		{"root bucket page", func(t *testing.T, path string) {
			pageSize, root, _ := chunkIndexMeta(t, path)
			setChunkIndexPageFlags(t, path, pageSize, root, boltMetaPageFlag)
		}},
		{"tail lost", func(t *testing.T, path string) {
			// Cut the file at the start of the OS page holding its freelist
			// page. bbolt maps the file rounded up to a power of two, so
			// reading that page faults (SIGBUS) rather than panicking: a
			// fault recover alone cannot catch.
			pageSize, _, freelist := chunkIndexMeta(t, path)
			osPage := int64(os.Getpagesize())
			size := int64(freelist) * int64(pageSize) / osPage * osPage
			if size < 2*int64(pageSize) || size&(size-1) == 0 {
				t.Fatalf("cutting the file to %d bytes would not leave its freelist page mapped past the end; change the test index", size)
			}
			truncate(t, path, size)
		}},
		{"cut short before the second meta page", func(t *testing.T, path string) {
			pageSize, _, _ := chunkIndexMeta(t, path)
			truncate(t, path, int64(pageSize))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeChunkIndexFile(t, 2, 1000)
			tc.corrupt(t, path)

			ix, err := OpenChunkIndex(path, "", zap.NewNop())
			if err != nil {
				t.Fatalf("a corrupt index must be replaced, got %v", err)
			}
			defer ix.Close()
			if n, err := ix.Len(); err != nil || n != 0 {
				t.Fatalf("Len = %d, %v; want a new, empty index", n, err)
			}
			if err := ix.AddLayer(digest.FromString("layer after recovery").String(), []chunk.Ref{{Hash: [32]byte{7}, Len: 1}}); err != nil {
				t.Fatal(err)
			}
			if _, ok := ix.Has([32]byte{7}); !ok {
				t.Fatal("the replacement index does not record entries")
			}
			if aside := corruptChunkIndexFiles(t, path); len(aside) != 1 {
				t.Fatalf("corrupt file not moved aside: %v", aside)
			}
		})
	}
}

// TestChunkIndexDisablesItselfOnACorruptPage: a torn page inside the chunks
// bucket goes unnoticed at open, and bbolt panics on the first read of it. That
// read fails instead, the index behaves as the disabled one from then on, and
// its file is moved aside so the next start opens an empty one.
func TestChunkIndexDisablesItselfOnACorruptPage(t *testing.T) {
	path := writeChunkIndexFile(t, 2, 1000)
	pageSize, _, _ := chunkIndexMeta(t, path)
	setChunkIndexPageFlags(t, path, pageSize, chunkIndexBucketPage(t, path, bucketChunks), boltMetaPageFlag)

	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatalf("the damage is in a data page, so open should succeed: %v", err)
	}
	indexed := sha256.Sum256([]byte("0-0"))
	if _, found, err := ix.Lookup([][32]byte{indexed}); !errors.Is(err, errChunkIndexCorrupt) || found[0] {
		t.Fatalf("Lookup = found %v, err %v; want a corruption error and nothing found", found[0], err)
	}

	blob := digest.FromString("layer indexed after the failure").String()
	if err := ix.AddLayer(blob, []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatalf("AddLayer = %v; want the disabled index's silent no-op", err)
	}
	if _, found, err := ix.Lookup([][32]byte{indexed, {1}}); err != nil || found[0] || found[1] {
		t.Fatalf("Lookup = %v, %v; want nothing found and no error", found, err)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 0 {
		t.Fatalf("Blobs = %v, %v; want none", blobs, err)
	}
	if n, err := ix.Len(); err != nil || n != 0 {
		t.Fatalf("Len = %d, %v; want 0", n, err)
	}
	if err := ix.Drop(blob); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the corrupt file is still at %s: %v", path, err)
	}
	if aside := corruptChunkIndexFiles(t, path); len(aside) != 1 {
		t.Fatalf("corrupt file not moved aside: %v", aside)
	}
	next, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if n, err := next.Len(); err != nil || n != 0 {
		t.Fatalf("next start's index Len = %d, %v; want a new, empty index", n, err)
	}
}

// TestOpenChunkIndexKeepsAValidFileItCannotOpen: a failure that is not the
// file's fault (here EACCES; on a device ENOSPC or EIO) leaves the file in
// place, so only this start runs with the disabled index.
func TestOpenChunkIndexKeepsAValidFileItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, file permissions do not deny opening")
	}
	path := writeChunkIndexFile(t, 1, 10)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if ix, err := OpenChunkIndex(path, "", zap.NewNop()); err == nil {
		ix.Close()
		t.Fatal("opening an index the agent cannot write must fail")
	}
	if aside := corruptChunkIndexFiles(t, path); len(aside) != 0 {
		t.Fatalf("a valid index was moved aside: %v", aside)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if n, err := ix.Len(); err != nil || n != 10 {
		t.Fatalf("Len = %d, %v; want the 10 entries the file held", n, err)
	}
}

func TestOpenChunkIndexKeepsOnlyTheNewestCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk-index.db")
	older := path + ".corrupt-1"
	if err := os.WriteFile(older, []byte("moved aside by an earlier start"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("this is not a bbolt file"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenChunkIndex(path, "", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if aside := corruptChunkIndexFiles(t, path); len(aside) != 1 || aside[0] == older {
		t.Fatalf("corrupt files = %v; want only the one just moved aside", aside)
	}
}

func TestDisabledChunkIndexHoldsNothing(t *testing.T) {
	var ix ChunkIndex // what NewClient falls back to when the index file cannot be opened
	blob := digest.FromString("layer").String()
	if err := ix.AddLayer(blob, []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Has([32]byte{1}); ok {
		t.Fatal("a disabled index must report every chunk missing")
	}
	if err := ix.Drop(blob); err != nil {
		t.Fatal(err)
	}
	if blobs, err := ix.Blobs(); err != nil || len(blobs) != 0 {
		t.Fatalf("Blobs = %v, %v", blobs, err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
}
