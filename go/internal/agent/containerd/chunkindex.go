package containerd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	digest "github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

const (
	// defaultChunkIndexPath is the persistent chunk index.
	defaultChunkIndexPath = "/var/lib/wendy/chunk-index.db"
	// legacyChunkIndexPath is the JSON index agents before WDY-3212 rewrote in
	// full on every new layer. It is imported once, then removed.
	legacyChunkIndexPath = "/var/lib/wendy/chunk-index.json"
	// chunkIndexTxEntries bounds one write transaction, so indexing a
	// multi-GB layer never holds a whole huge insert's dirty pages in memory.
	chunkIndexTxEntries = 16384
	// chunkLocSize is an encoded chunkLoc: blob digest (32) | offset (8) | length (8).
	chunkLocSize = 48
)

var (
	// bucketChunks maps a chunk hash to the blob range holding its bytes.
	bucketChunks = []byte("chunks")
	// bucketBlobChunks has one empty-valued key per indexed chunk, blob digest
	// (32 bytes) followed by chunk hash (32 bytes), so dropping a blob costs
	// O(its chunks) instead of a scan of the whole index.
	bucketBlobChunks = []byte("blob-chunks")
	// bucketBlobs is the set of indexed blob digests, for reconciliation.
	bucketBlobs = []byte("blobs")

	// errChunkIndexCorrupt marks a failure caused by the index file's
	// contents: bbolt panicked on a corrupt page, or reading a page the file
	// no longer holds faulted.
	errChunkIndexCorrupt = errors.New("chunk index corrupt")
)

type chunkLoc struct {
	Blob   string `json:"blob"`
	Offset uint64 `json:"offset"`
	Len    uint64 `json:"len"`
}

// ChunkIndex maps content-defined chunk hashes to byte ranges inside
// uncompressed Wendy layer blobs already in the containerd content store.
//
// It is a cache of what the content store holds: losing it costs re-sent
// chunks, never correctness. Updates touch only the entries they change, so
// indexing a layer costs O(chunks in that layer) rather than a rewrite of every
// chunk ever indexed, which grew every deploy's cost with device history
// (WDY-3212).
//
// The zero ChunkIndex is a disabled index: it holds nothing and records
// nothing, so every chunk is reported missing and re-sent. An index whose file
// turns out to be corrupt while the agent runs behaves the same from then on.
type ChunkIndex struct {
	db     *bolt.DB
	path   string
	logger *zap.Logger
	// broken is set once a transaction hit a corrupt page; see recoverCorrupt.
	broken atomic.Bool
}

// OpenChunkIndex opens the index at path, creating it if needed. A legacy JSON
// index at legacyPath (empty to skip) is imported once and then removed. A
// corrupt file is moved aside and replaced by an empty index. Any other
// failure is returned and the file left alone, so the caller runs this start
// with the disabled index and a later start can still use the file.
func OpenChunkIndex(path, legacyPath string, logger *zap.Logger) (*ChunkIndex, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := openChunkIndexDB(path)
	if err != nil {
		if !isCorruptChunkIndex(err) {
			// A lock another process holds, a full or read-only disk, an
			// I/O or memory error: none of them says the file is bad, and
			// moving it aside would discard a good index (or pull it out
			// from under the process holding the lock).
			return nil, fmt.Errorf("opening chunk index %s: %w", path, err)
		}
		aside, merr := moveChunkIndexAside(path)
		if merr != nil {
			return nil, fmt.Errorf("opening chunk index %s: %w (moving it aside: %v)", path, err, merr)
		}
		logger.Warn("Chunk index corrupt; starting an empty one",
			zap.String("path", path), zap.String("moved_to", aside), zap.Error(err))
		if db, err = openChunkIndexDB(path); err != nil {
			return nil, fmt.Errorf("creating chunk index %s: %w", path, err)
		}
	}
	ix := &ChunkIndex{db: db, path: path, logger: logger}
	if legacyPath != "" {
		ix.importLegacyJSON(legacyPath, logger)
	}
	return ix, nil
}

// openChunkIndexDB opens the store at path and creates its buckets. bbolt
// panics, rather than returning an error, on many kinds of page corruption,
// and reading a page the file no longer holds faults. Both come back as
// errChunkIndexCorrupt instead of killing the agent, which would otherwise
// crash-loop on every start. A handle whose open panicked is leaked rather
// than closed, since bbolt may have died holding its locks; it pins only the
// old inode, so the replacement file does not conflict with it.
func openChunkIndexDB(path string) (db *bolt.DB, err error) {
	defer func(panicOnFault bool) {
		debug.SetPanicOnFault(panicOnFault)
		if p := recover(); p != nil {
			db, err = nil, fmt.Errorf("%w: %v", errChunkIndexCorrupt, p)
		}
	}(debug.SetPanicOnFault(true))
	db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketChunks, bucketBlobChunks, bucketBlobs} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// isCorruptChunkIndex reports whether opening the index failed because of the
// file's contents, the only failure that moving the file aside can fix.
func isCorruptChunkIndex(err error) bool {
	return errors.Is(err, errChunkIndexCorrupt) ||
		errors.Is(err, berrors.ErrInvalid) ||
		errors.Is(err, berrors.ErrChecksum) ||
		errors.Is(err, berrors.ErrVersionMismatch) ||
		// A file cut short before its second meta page holds no entries: a
		// full disk or a power cut interrupted its creation. bbolt reports
		// it with an untyped error.
		strings.HasPrefix(err.Error(), "file size too small")
}

// moveChunkIndexAside renames a corrupt index file out of the way, keeping it
// for diagnosis, and deletes any older one, so a disk that keeps corrupting
// the index does not fill up with copies of it.
func moveChunkIndexAside(path string) (string, error) {
	aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".corrupt-"
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if name := e.Name(); strings.HasPrefix(name, prefix) && name != filepath.Base(aside) {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
	return aside, nil
}

// Close releases the index file.
func (ix *ChunkIndex) Close() error {
	if ix.db == nil {
		return nil
	}
	return ix.db.Close()
}

// disabled reports whether the index holds and records nothing: the zero
// index, or one whose file proved corrupt.
func (ix *ChunkIndex) disabled() bool {
	return ix.db == nil || ix.broken.Load()
}

// view and update run fn in a read or write transaction, and do nothing on a
// disabled index. Every access to the file goes through one of them, so a
// corrupt page anywhere disables the index instead of panicking out of it.
func (ix *ChunkIndex) view(fn func(*bolt.Tx) error) (err error) {
	if ix.disabled() {
		return nil
	}
	defer ix.recoverCorrupt(debug.SetPanicOnFault(true), &err)
	return ix.db.View(fn)
}

func (ix *ChunkIndex) update(fn func(*bolt.Tx) error) (err error) {
	if ix.disabled() {
		return nil
	}
	defer ix.recoverCorrupt(debug.SetPanicOnFault(true), &err)
	return ix.db.Update(fn)
}

// recoverCorrupt, deferred with the goroutine's previous SetPanicOnFault
// setting, turns a panic out of a transaction, or a fault on a page the file
// no longer holds, into an error. bbolt rolls the transaction back first. The
// first such failure disables the index for the rest of this run and moves its
// file aside, so the next start opens an empty one. A panic left to escape
// would fail every chunk deploy from then on, and kill the agent when the
// maintenance goroutine hit it.
func (ix *ChunkIndex) recoverCorrupt(panicOnFault bool, err *error) {
	debug.SetPanicOnFault(panicOnFault)
	p := recover()
	if p == nil {
		return
	}
	*err = fmt.Errorf("%w: %v", errChunkIndexCorrupt, p)
	if ix.broken.Swap(true) {
		return // an earlier failure already disabled the index and moved its file
	}
	aside, merr := moveChunkIndexAside(ix.path)
	logger := ix.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Error("Chunk index corrupt; disabled until the agent restarts",
		zap.String("path", ix.path), zap.String("moved_to", aside), zap.NamedError("move_error", merr),
		zap.Any("panic", p), zap.Stack("stack"))
}

// Has reports where a chunk's bytes live.
func (ix *ChunkIndex) Has(h [32]byte) (chunkLoc, bool) {
	locs, found, err := ix.Lookup([][32]byte{h})
	if err != nil || !found[0] {
		return chunkLoc{}, false
	}
	return locs[0], true
}

// Lookup resolves many hashes in one read transaction. found[i] reports whether
// hashes[i] is indexed, and locs[i] is its location when it is. A lookup that
// fails reports nothing found.
func (ix *ChunkIndex) Lookup(hashes [][32]byte) (locs []chunkLoc, found []bool, err error) {
	locs = make([]chunkLoc, len(hashes))
	found = make([]bool, len(hashes))
	err = ix.view(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketChunks)
		for i, h := range hashes {
			if v := b.Get(h[:]); len(v) == chunkLocSize {
				locs[i], found[i] = decodeChunkLoc(v), true
			}
		}
		return nil
	})
	if err != nil {
		// A transaction that panicked part way may have filled some slots
		// from a file now known to be corrupt.
		clear(locs)
		clear(found)
	}
	return locs, found, err
}

// AddLayer records refs as byte ranges of blobDigest. A hash already indexed in
// another blob is re-pointed here: the newest blob is the one most likely to
// outlive a prune.
func (ix *ChunkIndex) AddLayer(blobDigest string, refs []chunk.Ref) error {
	if ix.disabled() {
		return nil
	}
	blob, err := blobKey(blobDigest)
	if err != nil {
		return err
	}
	for start := 0; ; start += chunkIndexTxEntries {
		end := min(start+chunkIndexTxEntries, len(refs))
		if err := ix.update(func(tx *bolt.Tx) error {
			if err := tx.Bucket(bucketBlobs).Put(blob[:], nil); err != nil {
				return err
			}
			chunks, pairs := tx.Bucket(bucketChunks), tx.Bucket(bucketBlobChunks)
			for _, r := range refs[start:end] {
				if err := chunks.Put(r.Hash[:], encodeChunkLoc(blob, r.Offset, r.Len)); err != nil {
					return err
				}
				if err := pairs.Put(blobChunkKey(blob, r.Hash), nil); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if end == len(refs) {
			return nil
		}
	}
}

// Drop removes every entry recorded for blobDigest; see DropBlobs.
func (ix *ChunkIndex) Drop(blobDigest string) error {
	_, err := ix.DropBlobs([]string{blobDigest})
	return err
}

// DropBlobs removes every entry recorded for the given blobs and returns how
// many of the blobs it finished. A hash a later blob re-indexed keeps pointing
// at that later blob. A transaction deletes at most chunkIndexTxEntries
// entries, across blobs: one multi-GB layer never makes a huge transaction,
// and hundreds of small stale blobs share a few instead of one each. A blob's
// key goes last, in the transaction that deletes its final entries, so a drop
// cut short leaves the blob listed and the next reconcile finishes it.
func (ix *ChunkIndex) DropBlobs(blobDigests []string) (dropped int, err error) {
	blobs := make([][32]byte, 0, len(blobDigests))
	for _, d := range blobDigests {
		if blob, err := blobKey(d); err == nil {
			blobs = append(blobs, blob) // a digest AddLayer rejects was never indexed
		}
	}
	// Sorted, the blobs of one transaction read one run of the blob-chunks
	// bucket.
	slices.SortFunc(blobs, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	blobs = slices.Compact(blobs)
	for len(blobs) > 0 && !ix.disabled() {
		var done int
		if err := ix.update(func(tx *bolt.Tx) (err error) {
			done, err = dropBlobsTx(tx, blobs, chunkIndexTxEntries)
			return err
		}); err != nil {
			return dropped, err
		}
		dropped += done
		blobs = blobs[done:]
	}
	return dropped, nil
}

// dropBlobsTx deletes up to budget of the blobs' entries, taking the blobs in
// order, and the key of each blob left with none. It returns how many blobs it
// finished; the next call resumes with the first unfinished one.
func dropBlobsTx(tx *bolt.Tx, blobs [][32]byte, budget int) (done int, err error) {
	chunks, pairs, listed := tx.Bucket(bucketChunks), tx.Bucket(bucketBlobChunks), tx.Bucket(bucketBlobs)
	for _, blob := range blobs {
		if budget == 0 {
			break
		}
		// Collect one key past the budget, to learn whether the blob has
		// entries left over for the next transaction. Deleting while a cursor
		// walks the bucket is unsafe, hence the copies.
		var keys [][]byte
		c := pairs.Cursor()
		for k, _ := c.Seek(blob[:]); k != nil && bytes.HasPrefix(k, blob[:]) && len(keys) <= budget; k, _ = c.Next() {
			keys = append(keys, bytes.Clone(k))
		}
		more := len(keys) > budget
		if more {
			keys = keys[:budget]
		}
		for _, k := range keys {
			hash := k[len(blob):]
			if v := chunks.Get(hash); len(v) == chunkLocSize && bytes.Equal(v[:len(blob)], blob[:]) {
				if err := chunks.Delete(hash); err != nil {
					return done, err
				}
			}
			if err := pairs.Delete(k); err != nil {
				return done, err
			}
		}
		if more {
			break // the blob stays listed until its last entry goes
		}
		if err := listed.Delete(blob[:]); err != nil {
			return done, err
		}
		budget -= len(keys)
		done++
	}
	return done, nil
}

// Blobs lists every indexed blob digest.
func (ix *ChunkIndex) Blobs() ([]string, error) {
	var out []string
	if err := ix.view(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlobs).ForEach(func(k, _ []byte) error {
			out = append(out, "sha256:"+hex.EncodeToString(k))
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Len is the number of indexed chunk hashes.
func (ix *ChunkIndex) Len() (int, error) {
	var n int
	if err := ix.view(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketChunks).Stats().KeyN
		return nil
	}); err != nil {
		return 0, err
	}
	return n, nil
}

// importLegacyJSON moves the entries of a pre-WDY-3212 JSON index into the
// store and deletes the JSON. Nothing is retried, since the index is a cache:
// an unreadable file is discarded, and a failed import keeps what it wrote.
func (ix *ChunkIndex) importLegacyJSON(path string, logger *zap.Logger) {
	start := time.Now()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	var entries map[string]chunkLoc
	if err == nil && len(data) > 0 {
		err = json.Unmarshal(data, &entries)
	}
	if err != nil {
		logger.Warn("Discarding unreadable legacy chunk index", zap.String("path", path), zap.Error(err))
	} else if imported, blobs, err := ix.importEntries(entries); err != nil {
		logger.Warn("Legacy chunk index import failed", zap.String("path", path),
			zap.Int("entries_imported", imported), zap.Error(err))
	} else {
		logger.Info("Migrated legacy chunk index", zap.String("path", path),
			zap.Int("entries", imported), zap.Int("blobs", blobs), zap.Duration("took", time.Since(start)))
	}
	for _, p := range []string{path, path + ".tmp"} {
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			logger.Warn("Could not remove legacy chunk index", zap.String("path", p), zap.Error(rerr))
		}
	}
}

// importEntries writes the legacy entries as the records AddLayer writes, in
// transactions of at most chunkIndexTxEntries entries across blobs, sorted by
// chunk hash so each transaction fills one run of the chunks bucket. One
// transaction per blob made the migration grow with the number of blobs:
// 23.6 s for 2000, all before the agent serves an RPC. It returns how many
// entries, and how many distinct blobs, the legacy index held that this one
// can.
func (ix *ChunkIndex) importEntries(entries map[string]chunkLoc) (imported, blobs int, err error) {
	type entry struct {
		hash, blob     [32]byte
		offset, length uint64
	}
	type parsedBlob struct {
		key   [32]byte
		valid bool
	}
	parsed := make(map[string]parsedBlob)
	flat := make([]entry, 0, len(entries))
	for key, loc := range entries {
		raw, err := hex.DecodeString(key)
		if err != nil || len(raw) != 32 {
			continue
		}
		b, seen := parsed[loc.Blob]
		if !seen {
			k, err := blobKey(loc.Blob)
			b = parsedBlob{key: k, valid: err == nil}
			parsed[loc.Blob] = b
			if b.valid {
				blobs++
			}
		}
		if !b.valid {
			continue // not a digest this index can hold; those chunks are re-sent when needed
		}
		e := entry{blob: b.key, offset: loc.Offset, length: loc.Len}
		copy(e.hash[:], raw)
		flat = append(flat, e)
	}
	slices.SortFunc(flat, func(a, b entry) int { return bytes.Compare(a.hash[:], b.hash[:]) })
	for start := 0; start < len(flat); start += chunkIndexTxEntries {
		batch := flat[start:min(start+chunkIndexTxEntries, len(flat))]
		if err := ix.update(func(tx *bolt.Tx) error {
			listed, chunks, pairs := tx.Bucket(bucketBlobs), tx.Bucket(bucketChunks), tx.Bucket(bucketBlobChunks)
			for _, e := range batch {
				if err := listed.Put(e.blob[:], nil); err != nil {
					return err
				}
				if err := chunks.Put(e.hash[:], encodeChunkLoc(e.blob, e.offset, e.length)); err != nil {
					return err
				}
				if err := pairs.Put(blobChunkKey(e.blob, e.hash), nil); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return imported, blobs, err
		}
		imported += len(batch)
	}
	return imported, blobs, nil
}

// blobKey is the raw 32-byte sha256 of a "sha256:<hex>" blob digest.
func blobKey(blob string) ([32]byte, error) {
	var k [32]byte
	d, err := digest.Parse(blob)
	if err != nil {
		return k, err
	}
	if d.Algorithm() != digest.SHA256 {
		return k, fmt.Errorf("chunk index holds sha256 blobs only, got %s", d.Algorithm())
	}
	_, err = hex.Decode(k[:], []byte(d.Encoded()))
	return k, err
}

func blobChunkKey(blob, hash [32]byte) []byte {
	k := make([]byte, 0, 64)
	k = append(k, blob[:]...)
	return append(k, hash[:]...)
}

func encodeChunkLoc(blob [32]byte, offset, length uint64) []byte {
	v := make([]byte, chunkLocSize)
	copy(v, blob[:])
	binary.BigEndian.PutUint64(v[32:40], offset)
	binary.BigEndian.PutUint64(v[40:48], length)
	return v
}

func decodeChunkLoc(v []byte) chunkLoc {
	return chunkLoc{
		Blob:   "sha256:" + hex.EncodeToString(v[:32]),
		Offset: binary.BigEndian.Uint64(v[32:40]),
		Len:    binary.BigEndian.Uint64(v[40:48]),
	}
}
