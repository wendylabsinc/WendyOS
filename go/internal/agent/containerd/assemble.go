package containerd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
)

const (
	// maxSegmentBytes bounds a segment: one read of a layer's bytes and one
	// write to the content store. planAssembly caps every chunk at
	// maxStagedChunkBytes, below it, so a single chunk always fits and runs
	// coalesce only up to it. It matches the containerd proxy writer's own
	// 8 MiB message split, so a segment costs one Write round trip rather than
	// the eight 1 MiB round trips of content.WriteBlob's copy loop.
	maxSegmentBytes = 8 << 20
	// assemblyReadAhead is how many verified segments may wait for the writer,
	// so reading and hashing the next segments overlaps the current write.
	assemblyReadAhead = 4
	// assemblyBufferCount is how many segment buffers exist in the agent:
	// assemblyReadAhead waiting, one being read and one being written, so a
	// lone assembly keeps its whole pipeline.
	assemblyBufferCount = assemblyReadAhead + 2
	// assemblyBuffersIdle is how long the pool keeps its buffers once none
	// is in use: long enough to span the layers of a deploy and the deploys
	// of a working session, short enough that an idle agent does not hold
	// 48 MiB it last used hours ago.
	assemblyBuffersIdle = 30 * time.Second
)

// assemblyBuffers is the segment buffer pool every assembly shares. Compose
// prepares up to four services at once, and a build delivery prepares images
// too; with buffers of their own, four assemblies took the agent's heap from
// 14 to 121 MiB, on devices with 1 GB of RAM. Shared, the agent holds at most
// assemblyBufferCount × maxSegmentBytes (48 MiB) of them at any concurrency,
// and none once they have sat unused for assemblyBuffersIdle.
var assemblyBuffers = newBufferPool(assemblyBufferCount, maxSegmentBytes, assemblyBuffersIdle)

// bufferPool recycles at most n buffers of one size, each allocated on first
// use, and frees them all once none has been in use for its idle time.
//
// It cannot deadlock. Only readers take buffers: readSegments' goroutine, and
// verifyPrefix, which holds no other buffer while it waits. Writers never do.
// A reader gives back a buffer it could not deliver (a failed read, its
// context ending first, or a panic), and the consumer gives back every buffer
// delivered to it: after writing it, or, when it stops early, by cancelling
// the reader and draining the channel. So every buffer out of the pool is held
// by a goroutine that returns it without waiting on the pool, and a reader
// waiting for one waits only on writes already under way, its own
// assembly's or another's. Nor does an assembly hold a buffer while it waits
// for another's per-ref ingest lock: its reader starts only once OpenWriter
// has returned.
type bufferPool struct {
	// slots holds the buffers not in use; nil stands for one not made yet,
	// or freed after the pool sat idle.
	slots chan []byte
	size  int
	idle  time.Duration

	mu   sync.Mutex
	trim *time.Timer // frees the buffers once the pool has sat idle

	// live counts buffers that exist, outstanding those out of the pool,
	// and peak the most ever out at once, for tests.
	live, outstanding, peak atomic.Int64
}

func newBufferPool(n, size int, idle time.Duration) *bufferPool {
	p := &bufferPool{slots: make(chan []byte, n), size: size, idle: idle}
	for range n {
		p.slots <- nil
	}
	return p
}

// acquire returns a buffer of the pool's size, waiting while every buffer is
// out, or ctx's error if ctx ends first.
func (p *bufferPool) acquire(ctx context.Context) ([]byte, error) {
	var b []byte
	select {
	case b = <-p.slots:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if b == nil {
		b = make([]byte, p.size)
		p.live.Add(1)
	}
	out := p.outstanding.Add(1)
	for {
		peak := p.peak.Load()
		if out <= peak || p.peak.CompareAndSwap(peak, out) {
			break
		}
	}
	return b, nil
}

// release returns a buffer acquire handed out. It never blocks: the pool has
// a slot for every buffer it made. The last buffer back starts the idle
// countdown.
func (p *bufferPool) release(b []byte) {
	// Count it back before another goroutine can take it, so the counters
	// never show more out than exist.
	idle := p.outstanding.Add(-1) == 0
	p.slots <- b[:cap(b)]
	if !idle {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trim == nil {
		p.trim = time.AfterFunc(p.idle, p.freeIdle)
	} else {
		p.trim.Reset(p.idle)
	}
}

// freeIdle frees every buffer in the pool, unless one is out again: that use
// restarts the countdown when it ends. A buffer acquired while this runs
// comes from a slot not yet visited, or is made anew.
func (p *bufferPool) freeIdle() {
	for range cap(p.slots) {
		if p.outstanding.Load() != 0 {
			return
		}
		select {
		case b := <-p.slots:
			if b != nil {
				p.live.Add(-1)
			}
			p.slots <- nil
		default:
			return
		}
	}
}

// assemblySegment is a run of a layer's chunks read into one buffer and written
// with one Write: consecutive chunks that sit back to back in the same indexed
// blob, read with one ReadAt, or consecutive staged chunks (blob == ""), read
// with one file read each.
type assemblySegment struct {
	blob   string
	offset uint64 // start of the run within blob
	size   uint64
	chunks []assemblyChunk
}

type assemblyChunk struct {
	hash [32]byte
	len  uint64
}

// planAssembly resolves where every chunk of a layer lives and groups the
// chunks into segments. The reused chunks of a rebuilt dependency layer are
// almost always contiguous in their previous blob, so ~85% reuse of a 330 MB
// layer becomes a few dozen large reads instead of two containerd RPCs per
// 64 KiB chunk (WDY-3213). It also returns each chunk's range in the blob being
// assembled, for the index, and that blob's size.
//
// Every planned chunk is 1 to maxStagedChunkBytes long, which bounds each
// segment's size and keeps the sums below from wrapping. A length outside
// that range means a corrupt staged file or index entry; see plausibleChunkLen.
func (c *Client) planAssembly(hashes [][32]byte) ([]assemblySegment, []chunk.Ref, int64, error) {
	stagedLen := make([]int64, len(hashes))
	var unstaged [][32]byte
	for i, h := range hashes {
		n, ok := c.staging.statLen(h)
		if ok && !plausibleChunkLen(uint64(n)) {
			// No chunk can be this: StageChunk bounds its size, and a layer
			// has no empty chunks. The staging area writes without fsync, so
			// a power cut can leave a chunk's file empty. MissingChunks would
			// report it present and staging.write would never replace it, so
			// remove it: the CLI then sends the chunk again.
			c.staging.remove(h)
			ok = false
		}
		if ok {
			stagedLen[i] = n
		} else {
			stagedLen[i] = -1
			unstaged = append(unstaged, h)
		}
	}
	locs, found, err := c.chunkIndex.Lookup(unstaged)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("reading chunk index: %w", err)
	}

	var (
		segs  []assemblySegment
		refs  = make([]chunk.Ref, len(hashes))
		total int64
		next  int // position in unstaged, locs and found
	)
	for i, h := range hashes {
		var (
			n    uint64
			blob string
			off  uint64
		)
		if stagedLen[i] >= 0 {
			n = uint64(stagedLen[i])
		} else {
			if !found[next] {
				return nil, nil, 0, fmt.Errorf("chunk %d (%x) unavailable", i, h)
			}
			loc := locs[next]
			next++
			if !plausibleChunkLen(loc.Len) {
				// A corrupt entry. MissingChunks checks only that a range
				// fits its blob, so it would keep reporting the chunk
				// present; forget the blob, so the CLI sends it again.
				c.dropIndexedBlob(loc.Blob)
				return nil, nil, 0, fmt.Errorf("chunk %d (%x) unavailable", i, h)
			}
			n, blob, off = loc.Len, loc.Blob, loc.Offset
		}
		last := len(segs) - 1
		if last >= 0 && segs[last].blob == blob && segs[last].size+n <= maxSegmentBytes &&
			(blob == "" || segs[last].offset+segs[last].size == off) {
			segs[last].chunks = append(segs[last].chunks, assemblyChunk{hash: h, len: n})
			segs[last].size += n
		} else {
			segs = append(segs, assemblySegment{blob: blob, offset: off, size: n, chunks: []assemblyChunk{{hash: h, len: n}}})
		}
		refs[i] = chunk.Ref{Hash: h, Offset: uint64(total), Len: n}
		total += int64(n)
	}
	return segs, refs, total, nil
}

// plausibleChunkLen reports whether n can be a chunk's length: neither empty
// nor larger than StageChunk accepts.
func plausibleChunkLen(n uint64) bool {
	return n > 0 && n <= maxStagedChunkBytes
}

// dropIndexedBlob forgets every index entry for blob, so the next
// MissingChunks reports its chunks missing and the CLI sends them again.
func (c *Client) dropIndexedBlob(blob string) {
	if err := c.chunkIndex.Drop(blob); err != nil {
		c.logger.Warn("Dropping stale chunk-index entries failed", zap.String("blob", blob), zap.Error(err))
	}
}

// segmentData is one segment's verified bytes, or the error that ended reading.
// data lives in buf, a buffer from assemblyBuffers that the consumer must
// release once it is done with data; an error carries no buffer.
type segmentData struct {
	data []byte
	buf  []byte
	err  error
}

// release gives s's buffer back to the pool.
func (s segmentData) release() {
	if s.buf != nil {
		assemblyBuffers.release(s.buf)
	}
}

// readSegments reads segs in order on its own goroutine, verifies every chunk's
// SHA-256, and delivers each segment's bytes while staying up to
// assemblyReadAhead segments ahead of the consumer. The first skip bytes of the
// layer are not delivered: a resumed ingest already holds them. The channel
// closes after the last segment, after an error, or when ctx ends.
//
// Each segment is read into a buffer from assemblyBuffers; the consumer
// releases every segment it receives (see segmentData), and one that stops
// early cancels ctx and drains the channel, releasing the rest.
//
// A non-nil blobs receives, before the channel closes, how many source blobs
// the reader opened.
func (c *Client) readSegments(ctx context.Context, segs []assemblySegment, skip int64, blobs *int) <-chan segmentData {
	out := make(chan segmentData, assemblyReadAhead)
	go func() {
		defer close(out)
		var held []byte // acquired and not yet delivered
		defer func() {
			if held != nil {
				assemblyBuffers.release(held)
			}
			// No gRPC handler's recovery covers this goroutine, so a panic
			// here would take the agent down. Fail the assembly instead.
			if p := recover(); p != nil {
				select {
				case out <- segmentData{err: fmt.Errorf("reading layer segments: panic: %v", p)}:
				case <-ctx.Done():
				}
			}
		}()
		r := segmentReader{c: c, ctx: ctx, readers: map[string]content.ReaderAt{}}
		defer r.close()
		if blobs != nil {
			defer func() { *blobs = len(r.readers) }()
		}
		var pos int64
		for _, seg := range segs {
			if ctx.Err() != nil {
				return // cancelled: read nothing more
			}
			start := pos
			pos += int64(seg.size)
			if pos <= skip {
				continue
			}
			buf, err := assemblyBuffers.acquire(ctx)
			if err != nil {
				return // ctx ended
			}
			held = buf
			data := buf[:seg.size]
			if err := r.read(seg, data); err != nil {
				assemblyBuffers.release(buf) // before waiting to report the error
				held = nil
				select {
				case out <- segmentData{err: err}:
				case <-ctx.Done():
				}
				return
			}
			if skip > start {
				data = data[skip-start:]
			}
			select {
			case out <- segmentData{data: data, buf: buf}:
				held = nil
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// segmentReader reads segments, holding one ReaderAt per source blob for the
// whole assembly instead of opening one per chunk.
type segmentReader struct {
	c       *Client
	ctx     context.Context
	readers map[string]content.ReaderAt
}

func (r *segmentReader) close() {
	for _, ra := range r.readers {
		ra.Close()
	}
}

// read fills data, seg.size bytes long, with seg's bytes, and fails unless
// every chunk in it matches its hash.
//
// A chunk that fails its hash fails the assembly, and would fail every later
// one the same way: staging.write never replaces a staged file, and an index
// entry stays until its blob goes. So the failing copy goes too, and the next
// MissingChunks reports the chunk missing, so the CLI sends it again.
func (r *segmentReader) read(seg assemblySegment, data []byte) error {
	if seg.blob != "" {
		if err := r.readBlob(seg.blob, seg.offset, data); err != nil {
			return err
		}
		return r.checkIndexed(seg.blob, seg.chunks, data)
	}
	var off uint64
	for _, ch := range seg.chunks {
		if err := r.readStaged(ch, data[off:off+ch.len]); err != nil {
			return err
		}
		off += ch.len
	}
	return nil
}

// readStaged fills dst with a staged chunk and checks its hash, removing a
// staged file that fails it. When another layer's assembly has consumed the
// file since planning, the chunk is read from the blob that assembly indexed
// it into — the fallback the per-chunk reader always had.
func (r *segmentReader) readStaged(ch assemblyChunk, dst []byte) error {
	err := r.c.staging.readInto(ch.hash, dst)
	if err == nil {
		if sha256.Sum256(dst) != ch.hash {
			r.c.staging.remove(ch.hash)
			return fmt.Errorf("staged chunk %x hash mismatch", ch.hash)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	loc, ok := r.c.chunkIndex.Has(ch.hash)
	if !ok || loc.Len != ch.len {
		return fmt.Errorf("chunk %x unavailable", ch.hash)
	}
	if err := r.readBlob(loc.Blob, loc.Offset, dst); err != nil {
		return err
	}
	return r.checkIndexed(loc.Blob, []assemblyChunk{ch}, dst)
}

// checkIndexed fails unless data, read from blob, holds chunks in order,
// dropping the blob's index entries when a range fails its hash.
func (r *segmentReader) checkIndexed(blob string, chunks []assemblyChunk, data []byte) error {
	var off uint64
	for _, ch := range chunks {
		if sha256.Sum256(data[off:off+ch.len]) != ch.hash {
			r.c.dropIndexedBlob(blob)
			return fmt.Errorf("indexed chunk %x hash mismatch in %s", ch.hash, blob)
		}
		off += ch.len
	}
	return nil
}

// readBlob fills dst from blob at off.
func (r *segmentReader) readBlob(blob string, off uint64, dst []byte) error {
	ra, ok := r.readers[blob]
	if !ok {
		dgst, err := digest.Parse(blob)
		if err != nil {
			return err
		}
		ra, err = r.c.client.ContentStore().ReaderAt(r.ctx, ocispec.Descriptor{Digest: dgst})
		if err != nil {
			if errdefs.IsNotFound(err) {
				// The index outlived the blob. Forget it, so the next
				// QueryChunks asks the CLI for these chunks again.
				r.c.dropIndexedBlob(blob)
			}
			return fmt.Errorf("opening indexed blob %s: %w", blob, err)
		}
		r.readers[blob] = ra
	}
	if err := readFullAt(ra, dst, int64(off)); err != nil {
		return fmt.Errorf("reading %d bytes at %d of %s: %w", len(dst), off, blob, err)
	}
	return nil
}

// readFullAt fills dst from ra at off.
func readFullAt(ra content.ReaderAt, dst []byte, off int64) error {
	n, err := ra.ReadAt(dst, off)
	if n == len(dst) {
		return nil // a full read may still report io.EOF at the blob's end
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// readInto fills dst with the staged chunk h. A chunk that is not staged
// returns an error for which os.IsNotExist is true.
func (s *staging) readInto(h [32]byte, dst []byte) error {
	f, err := os.Open(s.path(h))
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.ReadFull(f, dst); err != nil {
		return fmt.Errorf("reading staged chunk %x: %w", h, err)
	}
	return nil
}

// assemblyStats describes one assembly, for its log line.
type assemblyStats struct {
	verified     bool  // see writeAssembledLayer
	segments     int   // segments planned
	stagedBytes  int64 // bytes planned from staged chunk files
	indexedBytes int64 // bytes planned from indexed blobs
	resumedBytes int64 // bytes the resumed ingest already held
	blobs        int   // source blobs the reader opened
}

// writeAssembledLayer writes segs into the content store as diffID, one Write
// per verified segment instead of content.WriteBlob's 1 MiB copy loop. Like
// content.Copy, it resumes a partial ingest by skipping the bytes the ingest
// already holds, so concurrent assemblies of the same layer still serialize on
// containerd's per-ref lock rather than clobbering each other.
//
// stats.verified reports that every chunk of the manifest was hash-checked
// against the bytes now committed as diffID: the reader checked each chunk it
// wrote, and checkResumedPrefix read back and checked the prefix the reader
// skipped. Only then may the manifest's ranges be indexed. It is false, with a
// nil error, whenever this call committed nothing it checked:
//   - OpenWriter or Commit reports the blob already exists. A concurrent
//     assembly committed it first, and over the proxy the content server
//     answers a Commit with AlreadyExists before it hashes a byte of this
//     ingest, so the outcome says nothing about this manifest.
//   - The resumed prefix read back from the blob does not match the manifest,
//     or could not be read.
//
// The blob itself is valid either way: its digest was verified when it was
// committed. Only the manifest is unproven.
//
// Once ctx ends, any failure is reported as ctx's error, so a cancelled
// deploy reads as cancelled rather than as the broken write it caused.
func (c *Client) writeAssembledLayer(ctx context.Context, diffID string, size int64, segs []assemblySegment) (assemblyStats, error) {
	stats := assemblyStats{segments: len(segs)}
	for _, seg := range segs {
		if seg.blob == "" {
			stats.stagedBytes += int64(seg.size)
		} else {
			stats.indexedBytes += int64(seg.size)
		}
	}
	fail := func(what string, err error) (assemblyStats, error) {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return assemblyStats{}, fmt.Errorf("%s layer %s: %w", what, diffID, err)
	}

	dgst, err := digest.Parse(diffID)
	if err != nil {
		return assemblyStats{}, fmt.Errorf("parsing digest %q: %w", diffID, err)
	}
	w, err := content.OpenWriter(ctx, c.client.ContentStore(),
		content.WithRef(diffID),
		content.WithDescriptor(ocispec.Descriptor{Digest: dgst, Size: size}))
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			c.logger.Debug("Layer already exists in content store", zap.String("digest", diffID))
			return stats, nil
		}
		return fail("opening", err)
	}
	defer w.Close()
	st, err := w.Status()
	if err != nil {
		return fail("checking the ingest of", err)
	}
	stats.resumedBytes = st.Offset

	readCtx, cancel := context.WithCancel(ctx)
	var blobs int // the reader's to set, and this call's to read once the channel closes
	segments := c.readSegments(readCtx, segs, st.Offset, &blobs)
	defer func() {
		// However this returns, stop the reader and give back the buffers
		// it still has queued. After the last segment the channel is
		// already closed, and this finds nothing.
		cancel()
		for seg := range segments {
			seg.release()
		}
	}()
	for seg := range segments {
		if seg.err != nil {
			return fail("reassembling", seg.err)
		}
		_, err := w.Write(seg.data)
		seg.release()
		if err != nil {
			return fail("writing", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail("writing", err)
	}
	stats.blobs = blobs
	labels := map[string]string{
		labelKeyGCRoot:     gcTimestamp(),
		labelKeyWendyLayer: "true",
	}
	if err := w.Commit(ctx, size, dgst, content.WithLabels(labels)); err != nil {
		if errdefs.IsAlreadyExists(err) {
			c.logger.Debug("Layer already exists in content store", zap.String("digest", diffID))
			return stats, nil
		}
		return fail("committing", err)
	}
	c.logger.Info("Wrote layer to content store", zap.String("digest", diffID), zap.Int64("size", size))
	stats.verified = st.Offset == 0 || c.checkResumedPrefix(ctx, dgst, segs, st.Offset)
	return stats, nil
}

// errPrefixMismatch marks a resumed prefix that does not hold the manifest's
// chunks.
var errPrefixMismatch = errors.New("resumed prefix does not match the manifest")

// checkResumedPrefix reports whether the first prefix bytes of the committed
// blob dgst hold the chunks segs names there. The reader skipped those bytes:
// an earlier attempt wrote them, or, under containerd's shared-content policy,
// the writer already held a blob another namespace committed. Commit vouched
// for them as part of the blob, not as this manifest's chunks. So every chunk
// that starts in the prefix is read back and hash-checked, in pieces of at most
// maxSegmentBytes. A mismatch or a failed read is logged and reported false:
// the blob is valid, but the manifest must not be indexed.
func (c *Client) checkResumedPrefix(ctx context.Context, dgst digest.Digest, segs []assemblySegment, prefix int64) bool {
	err := c.verifyPrefix(ctx, dgst, segs, prefix)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errPrefixMismatch):
		c.logger.Warn("Resumed layer prefix does not match its chunk manifest; not indexing it",
			zap.String("digest", dgst.String()), zap.Int64("resumed_bytes", prefix), zap.Error(err))
	default:
		c.logger.Warn("Could not read back a resumed layer prefix; not indexing it",
			zap.String("digest", dgst.String()), zap.Int64("resumed_bytes", prefix), zap.Error(err))
	}
	return false
}

// verifyPrefix checks the chunks of segs that start in the first prefix bytes
// of the blob dgst against their hashes; see checkResumedPrefix. planAssembly
// bounds every chunk to maxStagedChunkBytes, so a chunk always fits a piece.
func (c *Client) verifyPrefix(ctx context.Context, dgst digest.Digest, segs []assemblySegment, prefix int64) error {
	ra, err := c.client.ContentStore().ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return fmt.Errorf("opening committed layer: %w", err)
	}
	defer ra.Close()
	buf, err := assemblyBuffers.acquire(ctx)
	if err != nil {
		return err
	}
	defer assemblyBuffers.release(buf)

	var (
		start   int64 // offset of the pending piece in the blob
		pending []assemblyChunk
		size    uint64 // bytes pending
	)
	check := func() error {
		piece := buf[:size]
		if err := readFullAt(ra, piece, start); err != nil {
			return fmt.Errorf("reading %d bytes at %d of the committed layer: %w", size, start, err)
		}
		var off uint64
		for _, ch := range pending {
			if sha256.Sum256(piece[off:off+ch.len]) != ch.hash {
				return fmt.Errorf("%w: chunk %x at %d", errPrefixMismatch, ch.hash, start+int64(off))
			}
			off += ch.len
		}
		start += int64(size)
		pending, size = pending[:0], 0
		return nil
	}
	pos := int64(0) // offset of the next chunk in the blob
chunks:
	for _, seg := range segs {
		for _, ch := range seg.chunks {
			if pos >= prefix {
				break chunks
			}
			if size+ch.len > maxSegmentBytes {
				if err := check(); err != nil {
					return err
				}
			}
			pending = append(pending, ch)
			size += ch.len
			pos += int64(ch.len)
		}
	}
	if size == 0 {
		return nil
	}
	return check()
}
