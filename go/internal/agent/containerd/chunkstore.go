package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

// maxStagedChunkBytes bounds a single staged chunk. The CDC chunker emits
// chunks of at most chunk.MaxSize (64 KiB); this 4 MiB ceiling leaves ample
// headroom for legitimate clients while rejecting absurdly large payloads.
const maxStagedChunkBytes = 4 << 20

// defaultChunkStagingDir is where chunks received via WriteChunks are buffered
// until AssembleLayerFromChunks consumes them. It is deliberately disk-backed
// (not an in-memory map) so reassembling a multi-GiB layer — e.g. LLM weights
// on a Jetson AGX Thor — does not consume device RAM that belongs to the apps.
const defaultChunkStagingDir = "/var/lib/wendy/chunk-staging"

// staging persists chunk bytes received via StageChunk to disk, keyed by hash,
// until the next AssembleLayerFromChunks streams them into the content store.
// Disk — not the agent heap — is the staging budget, so reassembly's memory is
// bounded by the segment buffer pool every assembly shares (assemblyBuffers,
// 48 MiB for the whole agent), not by the uncompressed layer (×2 with the old
// reconstruct buffer). Distinct chunks map to distinct files, so concurrent
// deploys staging different content do not collide.
type staging struct {
	dir string
	mu  sync.Mutex
	// changed is closed and replaced whenever a new chunk becomes visible.
	// Closing broadcasts to every concurrent image preparation waiter, unlike
	// sending on a shared channel (which would wake only one waiter).
	changed chan struct{}
}

func newStaging(dir string) *staging {
	return &staging{dir: dir, changed: make(chan struct{})}
}

// changes returns the current broadcast generation. Callers subscribe before
// checking availability so a write racing the check either becomes visible to
// that check or closes the returned channel; no wakeup can be lost.
func (s *staging) changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *staging) notifyChanged() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// path returns the on-disk location for a chunk hash.
func (s *staging) path(h [32]byte) string {
	return filepath.Join(s.dir, hex.EncodeToString(h[:]))
}

// has reports whether the chunk is already staged on disk.
func (s *staging) has(h [32]byte) bool {
	_, err := os.Stat(s.path(h))
	return err == nil
}

// retain reports whether the chunk is staged, as has does, and keeps a chunk
// it reports present clear of the sweep. The read may come much later than
// the report: a build queued behind a long one reads its context only when it
// runs, and the store can sit idle meanwhile. So a file older than
// stagingRetention/2 gets its mtime moved to now, which keeps it at least that
// long; a newer file costs nothing beyond has's stat. The refresh is best
// effort: if it fails, a sweep can still take the chunk, and its read fails
// closed as before.
func (s *staging) retain(h [32]byte, now time.Time) bool {
	fi, err := os.Stat(s.path(h))
	if err != nil {
		return false
	}
	if fi.ModTime().Before(now.Add(-stagingRetention / 2)) {
		_ = os.Chtimes(s.path(h), now, now)
	}
	return true
}

// read returns the staged chunk bytes, or an os.IsNotExist error if absent.
func (s *staging) read(h [32]byte) ([]byte, error) {
	return os.ReadFile(s.path(h))
}

// statLen returns the staged chunk's size without reading it, false if absent.
func (s *staging) statLen(h [32]byte) (int64, bool) {
	fi, err := os.Stat(s.path(h))
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

// write stores data for hash h atomically (temp file + rename), creating the
// staging directory on demand. It is idempotent: a chunk already on disk is
// left untouched. Files are written 0600 — readable only by the agent user.
func (s *staging) write(h [32]byte, data []byte) error {
	if s.has(h) {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path(h)); err != nil {
		os.Remove(tmpName)
		return err
	}
	s.notifyChanged()
	return nil
}

// remove deletes a staged chunk, ignoring a missing file.
func (s *staging) remove(h [32]byte) {
	_ = os.Remove(s.path(h))
}

// chunkSource resolves a chunk hash to its raw bytes, returning (nil, nil) when
// the chunk is available from no source so the caller can report it as missing.
type chunkSource func(h [32]byte) ([]byte, error)

// chunkStream is an io.Reader that yields the chunks named by order in sequence,
// holding at most one chunk in memory at a time and verifying each chunk's
// SHA-256 as it is served. OpenChunkStream returns it to callers that want the
// bytes themselves; layer assembly reads segments instead (readSegments).
type chunkStream struct {
	order [][32]byte
	src   chunkSource
	idx   int
	cur   *bytes.Reader
}

func (s *chunkStream) Read(p []byte) (int, error) {
	for {
		if s.cur != nil {
			n, err := s.cur.Read(p)
			if err == io.EOF {
				s.cur = nil
				continue
			}
			return n, err
		}
		if s.idx >= len(s.order) {
			return 0, io.EOF
		}
		h := s.order[s.idx]
		s.idx++
		b, err := s.src(h)
		if err != nil {
			return 0, fmt.Errorf("chunk %d: %w", s.idx-1, err)
		}
		if b == nil {
			return 0, fmt.Errorf("chunk %d (%x) unavailable", s.idx-1, h)
		}
		if sha256.Sum256(b) != h {
			return 0, fmt.Errorf("chunk %d hash mismatch", s.idx-1)
		}
		s.cur = bytes.NewReader(b)
	}
}

func (c *Client) MissingChunks(ctx context.Context, hashes [][32]byte) ([][32]byte, error) {
	// Maintenance never sweeps staged chunks while a query holds this, and
	// staging.retain keeps each staged chunk this reports present clear of the
	// sweep afterwards, whichever call reads it later.
	c.chunkSweepMu.RLock()
	defer c.chunkSweepMu.RUnlock()
	c.chunkActivity.touch()

	// The chunk index outlives containerd's content GC. Validate each backing
	// blob once before reporting its chunks as present; otherwise a stale index
	// entry makes the CLI skip the upload and assembly fails much later when it
	// tries to read the missing blob. This is especially painful for multi-GB
	// layers because nearly the entire layer may be streamed before the stale
	// chunk is encountered.
	nsCtx := c.withNamespace(ctx)
	now := time.Now()
	candidates := make([][32]byte, 0, len(hashes))
	for _, h := range hashes {
		if !c.staging.retain(h, now) {
			candidates = append(candidates, h)
		}
	}
	// The index is a cache: a failure reading it must never fail a deploy.
	// Treat a lookup error as if the index held none of the candidates, so
	// every candidate not already staged is simply reported missing (and
	// re-sent) rather than aborting the whole call.
	locs, found, err := c.chunkIndex.Lookup(candidates)
	if err != nil {
		c.logger.Warn("Chunk index lookup failed; treating candidates as not indexed", zap.Error(err))
	}

	var cs content.Store
	blobSizes := make(map[string]uint64)
	invalidBlobs := make(map[string]struct{})
	for i, loc := range locs {
		if !found[i] {
			continue
		}
		if _, checked := blobSizes[loc.Blob]; checked {
			continue
		}
		if _, invalid := invalidBlobs[loc.Blob]; invalid {
			continue
		}
		dgst, err := digest.Parse(loc.Blob)
		if err != nil {
			invalidBlobs[loc.Blob] = struct{}{}
			continue
		}
		if cs == nil {
			cs = c.client.ContentStore()
		}
		info, err := cs.Info(nsCtx, dgst)
		if err != nil {
			if errdefs.IsNotFound(err) {
				invalidBlobs[loc.Blob] = struct{}{}
				continue
			}
			return nil, fmt.Errorf("checking indexed chunk blob %s: %w", loc.Blob, err)
		}
		if info.Size < 0 {
			invalidBlobs[loc.Blob] = struct{}{}
			continue
		}
		blobSizes[loc.Blob] = uint64(info.Size)
	}

	// A malformed range means the persisted index cannot safely describe that
	// blob. Drop all of its entries so every affected chunk is requested again.
	for i, loc := range locs {
		if !found[i] {
			continue
		}
		size, valid := blobSizes[loc.Blob]
		if !valid || loc.Offset > size || loc.Len > size-loc.Offset {
			invalidBlobs[loc.Blob] = struct{}{}
		}
	}
	// In batches, not a transaction per blob: the first query after the legacy
	// import can find hundreds of stale blobs, and this runs under the sweep
	// lock's read side. A failed drop costs nothing here, since those chunks
	// are reported missing below; the next query or reconcile retries it.
	if _, err := c.chunkIndex.DropBlobs(slices.Collect(maps.Keys(invalidBlobs))); err != nil {
		c.logger.Warn("Dropping stale chunk-index entries failed", zap.Int("blobs", len(invalidBlobs)), zap.Error(err))
	}

	var missing [][32]byte
	for i, h := range candidates {
		if !found[i] {
			missing = append(missing, h)
			continue
		}
		if _, invalid := invalidBlobs[locs[i].Blob]; invalid {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

// PresentLayers reports which of the given uncompressed layer digests (diff IDs)
// already exist in the content store, mapping each present diff ID to its blob
// size. Layers the device does not have are simply absent from the result. The
// CLI uses this to skip decompressing and chunking layers it can reuse as-is.
// Malformed digests are skipped rather than failing the whole query.
func (c *Client) PresentLayers(ctx context.Context, diffIDs []string) (map[string]int64, error) {
	nsCtx := c.withNamespace(ctx)
	cs := c.client.ContentStore()
	present := make(map[string]int64, len(diffIDs))
	for _, d := range diffIDs {
		dgst, err := digest.Parse(d)
		if err != nil {
			continue
		}
		info, err := cs.Info(nsCtx, dgst)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		present[d] = info.Size
	}
	return present, nil
}

func (c *Client) StageChunk(_ context.Context, h [32]byte, data []byte) error {
	// Maintenance never sweeps staged chunks while a stage holds this: see
	// MissingChunks above.
	c.chunkSweepMu.RLock()
	defer c.chunkSweepMu.RUnlock()
	c.chunkActivity.touch()

	if len(data) > maxStagedChunkBytes {
		return status.Errorf(codes.ResourceExhausted, "chunk too large: %d > %d bytes", len(data), maxStagedChunkBytes)
	}
	if sha256.Sum256(data) != h {
		return fmt.Errorf("staged chunk hash mismatch")
	}
	if err := c.staging.write(h, data); err != nil {
		return fmt.Errorf("staging chunk to disk: %w", err)
	}
	return nil
}

// readIndexedChunk reads a chunk's bytes from the uncompressed blob it was
// indexed into. Returns (nil,nil) when the blob is gone so the caller can treat
// it as a miss and prune the stale entry.
func (c *Client) readIndexedChunk(ctx context.Context, loc chunkLoc) ([]byte, error) {
	cs := c.client.ContentStore()
	dgst, err := digest.Parse(loc.Blob)
	if err != nil {
		return nil, err
	}
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		if errdefs.IsNotFound(err) {
			if derr := c.chunkIndex.Drop(loc.Blob); derr != nil {
				c.logger.Warn("Dropping stale chunk-index entries failed", zap.String("blob", loc.Blob), zap.Error(derr))
			}
			return nil, nil
		}
		return nil, err
	}
	defer ra.Close()
	b := make([]byte, loc.Len)
	if _, err := ra.ReadAt(b, int64(loc.Offset)); err != nil {
		return nil, err
	}
	return b, nil
}

// OpenChunkStream returns a reader over the chunks named by hashes, in order,
// verifying each chunk's SHA-256 as it is served and holding at most one chunk
// in memory.
//
// AssembleLayerFromChunks is the wrong tool for a caller that wants the bytes
// themselves: it writes a layer blob into the content store, which is right for
// an image layer and wrong for anything else — a remote build context, say.
// This exposes the same chunk resolution without that side effect.
//
// The stream fails closed: a chunk that is in neither the staging area nor the
// index ends the read with an error rather than contributing zero bytes, and a
// chunk whose contents do not hash to the name it was asked for is rejected. A
// caller reassembling something it will then execute — a build context — gets
// either every requested byte or an error, never a silently short prefix.
func (c *Client) OpenChunkStream(ctx context.Context, hashes [][32]byte) io.Reader {
	c.chunkActivity.touch()
	nsCtx := c.withNamespace(ctx)
	src := func(h [32]byte) ([]byte, error) {
		if b, err := c.staging.read(h); err == nil {
			return b, nil
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if loc, ok := c.chunkIndex.Has(h); ok {
			return c.readIndexedChunk(nsCtx, loc)
		}
		// SECURITY: a nil chunk is the "not held here" sentinel, not an empty
		// one — chunkStream.Read turns it into "chunk N (%x) unavailable" rather
		// than splicing zero bytes into the stream. Returning an empty []byte
		// here instead would be the silent-truncation bug.
		return nil, nil
	}
	return &chunkStream{order: hashes, src: src}
}

func (c *Client) AssembleLayerFromChunks(ctx context.Context, diffID string, hashes [][32]byte) error {
	defer c.chunkActivity.begin()()
	nsCtx := c.withNamespace(ctx)

	// Fast path: if the (uncompressed) layer blob already exists in the content
	// store, it was reassembled and indexed on a previous deploy. Skip the
	// expensive reconstruct + index update entirely — for an unchanged layer
	// this avoids reading the full layer on every deploy, which dominates
	// redeploy latency for large base images.
	if dgst, err := digest.Parse(diffID); err == nil {
		if _, err := c.client.ContentStore().Info(nsCtx, dgst); err == nil {
			return nil
		}
	}

	start := time.Now()
	segs, refs, total, err := c.planAssembly(hashes)
	if err != nil {
		return err
	}
	stats, err := c.writeAssembledLayer(nsCtx, diffID, total, segs)
	if err != nil {
		return err
	}
	// Nothing proved that an unverified manifest describes the committed
	// blob: a concurrent assembly of the same layer committed it first, or
	// the prefix this call resumed does not match. Indexing it would send
	// later assemblies to ranges that fail their hashes.
	if stats.verified {
		c.indexAssembledLayer(diffID, refs, hashes)
	}
	// WDY-3213's acceptance is measured on devices from this line.
	c.logger.Info("Assembled layer",
		zap.String("diff_id", diffID),
		zap.Int64("size", total),
		zap.Duration("took", time.Since(start)),
		zap.Int("segments", stats.segments),
		zap.Int64("staged_bytes", stats.stagedBytes),
		zap.Int64("indexed_bytes", stats.indexedBytes),
		zap.Int64("resumed_bytes", stats.resumedBytes),
		zap.Int("blobs", stats.blobs),
		zap.Bool("verified", stats.verified))
	return nil
}

// indexAssembledLayer indexes a verified layer's blob from its manifest, then
// releases the staged chunks the index now holds.
//
// Staged chunks go only once the index holds them. Until then a concurrent
// assembly of another layer may rely on them: its waitForChunks, or its CLI's
// MissingChunks, already counted them present. When this returns early, or
// is never called, retire and the sweep reclaim them.
func (c *Client) indexAssembledLayer(diffID string, refs []chunk.Ref, hashes [][32]byte) {
	// Commit verified the blob digest and the segment reader verified each
	// chunk's hash as it read it, so these ranges are exact; re-reading and
	// re-chunking the blob to rediscover them cost ~1 s per 430 MB on an Orin
	// Nano (WDY-3214). A resumed prefix, which the reader skips, is read back
	// from the blob and checked, so the index never records a range this call
	// did not verify.
	if err := c.chunkIndex.AddLayer(diffID, refs); err != nil {
		c.logger.Warn("failed to index reassembled layer", zap.String("diff_id", diffID), zap.Error(err))
		return
	}
	if c.chunkIndex.disabled() {
		return // nothing was recorded
	}

	// Release the staged chunks now embedded in the blob and indexed.
	for _, h := range hashes {
		c.staging.remove(h)
	}
}
