package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"
)

// maxConcurrentLayerPush bounds how many layers are decompressed, chunked, and
// streamed at once. It overlaps the CPU-bound work of one layer with another's
// QueryChunks/WriteChunks network round-trips. Each in-flight layer spills its
// uncompressed tar to a temp file (not RAM), so the cap mainly bounds transient
// chunking buffers rather than whole layers. Chunking within a single layer is
// already parallelized across cores (chunk.ChunkReaderAt), so this need not
// equal the core count.
const maxConcurrentLayerPush = 4

// maxChunksPerWriteStream bounds one client-streaming WriteChunks RPC. Closing
// each small batch gets an application-level acknowledgement, limits how much
// progress can be reported before the receiver confirms it, and starts a fresh
// HTTP/2 stream at negligible overhead (at most 4 MiB per batch).
const maxChunksPerWriteStream = 64

// chunkTransferProgressWriter is implemented by interactive build output
// adapters that can display chunk-upload bytes directly. Keeping this as an
// optional extension of io.Writer preserves quiet and non-interactive callers.
type chunkTransferProgressWriter interface {
	ReportChunkTransfer(current, total int64, rate float64)
}

type chunkIndexProgressWriter interface {
	ReportChunkIndex(current, total int64, rate float64, done bool)
}

type imagePreparationProgressWriter interface {
	ReportImagePreparation()
}

type chunkTransferProgress struct {
	mu       sync.Mutex
	reporter chunkTransferProgressWriter
	current  int64
	total    int64
	started  time.Time
	last     time.Time
}

type chunkIndexProgress struct {
	mu       sync.Mutex
	reporter chunkIndexProgressWriter
	current  int64
	total    int64
	unknown  int
	started  time.Time
	last     time.Time
}

func newChunkIndexProgress(output io.Writer) *chunkIndexProgress {
	reporter, _ := output.(chunkIndexProgressWriter)
	return &chunkIndexProgress{reporter: reporter}
}

func (p *chunkIndexProgress) addProcessed(n int64) {
	if p == nil || p.reporter == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.started.IsZero() {
		p.started = now
	}
	p.current += n
	p.reportLocked(now, false)
}

// startLayer registers a layer before indexing begins. Cache-hit manifests know
// their uncompressed size up front; cache misses do not, so the aggregate total
// remains undisclosed until every unknown-size layer has finished. This avoids
// displaying impossible counters such as 5.0GB/1.3GB while parallel layers are
// still discovering their raw sizes.
func (p *chunkIndexProgress) startLayer(knownSize int64) {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.IsZero() {
		p.started = time.Now()
	}
	if knownSize > 0 {
		p.total += knownSize
	} else {
		p.unknown++
	}
	p.reportLocked(time.Now(), true)
}

// finishLayer replaces the size reserved by startLayer with the actual raw
// size. For an initially unknown layer this is the point at which its bytes can
// safely contribute to the displayed denominator.
func (p *chunkIndexProgress) finishLayer(knownSize, actualSize int64) {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if knownSize > 0 {
		p.total += actualSize - knownSize
	} else {
		if p.unknown > 0 {
			p.unknown--
		}
		p.total += actualSize
	}
	p.reportLocked(time.Now(), true)
}

func (p *chunkIndexProgress) abortLayer(knownSize int64) {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if knownSize > 0 {
		p.total -= knownSize
	} else if p.unknown > 0 {
		p.unknown--
	}
}

func (p *chunkIndexProgress) reportLocked(now time.Time, force bool) {
	const interval = 100 * time.Millisecond
	if !force && !p.last.IsZero() && now.Sub(p.last) < interval {
		return
	}
	p.last = now
	rate := float64(0)
	if elapsed := now.Sub(p.started).Seconds(); elapsed > 0 {
		rate = float64(p.current) / elapsed
	}
	total := p.total
	if p.unknown > 0 || p.current > total {
		total = 0
	}
	p.reporter.ReportChunkIndex(p.current, total, rate, false)
}

func (p *chunkIndexProgress) finish() {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	rate := float64(0)
	if elapsed := now.Sub(p.started).Seconds(); elapsed > 0 {
		rate = float64(p.current) / elapsed
	}
	total := p.total
	if p.unknown > 0 || p.current > total {
		total = 0
	}
	p.reporter.ReportChunkIndex(p.current, total, rate, true)
}

func newChunkTransferProgress(output io.Writer) *chunkTransferProgress {
	reporter, _ := output.(chunkTransferProgressWriter)
	return &chunkTransferProgress{reporter: reporter}
}

func (p *chunkTransferProgress) addTotal(n int64) {
	if p == nil || p.reporter == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.IsZero() {
		p.started = time.Now()
	}
	p.total += n
	p.reportLocked(time.Now(), true)
}

func (p *chunkTransferProgress) addSent(n int64) {
	if p == nil || p.reporter == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.started.IsZero() {
		p.started = now
	}
	p.current += n
	p.reportLocked(now, p.current >= p.total)
}

func (p *chunkTransferProgress) reportLocked(now time.Time, force bool) {
	const interval = 100 * time.Millisecond
	if !force && !p.last.IsZero() && now.Sub(p.last) < interval {
		return
	}
	p.last = now
	rate := float64(0)
	if elapsed := now.Sub(p.started).Seconds(); elapsed > 0 {
		rate = float64(p.current) / elapsed
	}
	p.reporter.ReportChunkTransfer(p.current, p.total, rate)
}

// pushLayersByChunks implements chunk-diff layer push for a set of OCI layers,
// processing up to maxConcurrentLayerPush layers concurrently. For each layer it:
//  1. Resolves the chunk manifest (DiffID, size, ordered chunk hashes) from the
//     on-disk manifest cache when the layer's compressed digest is known,
//     otherwise decompresses (to a temp file) and CDC-chunks the raw tar and
//     caches the result.
//  2. Queries the device for which chunk hashes are missing.
//  3. Streams only the missing chunk bytes via WriteChunks — decompressing the
//     layer at this point if the cache hit let us skip it earlier.
//  4. Produces one RunContainerLayerHeader per layer (COMPRESSION_NONE, carrying
//     the full ordered chunk manifest), in the original layer order.
//
// The common case — an unchanged layer whose chunks the device already has —
// resolves from cache and finds nothing missing, so the layer is never
// decompressed or re-chunked. A layer the device already holds in full (reported
// by the QueryLayers pre-check) is skipped entirely: it is never decompressed,
// chunked, or transferred.
func pushLayersByChunks(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer) ([]*agentpb.RunContainerLayerHeader, error) {
	return pushLayersByChunksWithPrepare(ctx, cs, layers, nil)
}

// imagePrepareFunc starts an authenticated device-side preparation using the
// complete ordered layer manifests. It is invoked after manifest resolution
// but before any missing chunk upload, and is expected to block until the
// preparation is complete (or the context is cancelled).
type imagePrepareFunc func(context.Context, []*agentpb.RunContainerLayerHeader) error

// blocksChunkPrepareFallback marks integrity and identity failures that must
// not be retried through a transport with a different trust boundary.
func blocksChunkPrepareFallback(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.DataLoss,
		codes.PermissionDenied, codes.Unauthenticated:
		return true
	default:
		return false
	}
}

// pushLayersByChunksWithPrepare resolves every layer manifest first, then runs
// prepare concurrently with missing-chunk upload. The short resolution barrier
// is necessary because the agent must authenticate the image config (which
// binds the ordered diff IDs) and know each layer's chunk set before it can
// safely create persistent snapshots.
func pushLayersByChunksWithPrepare(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc) ([]*agentpb.RunContainerLayerHeader, error) {
	return pushLayersByChunksWithPrepareOutput(ctx, cs, layers, prepare, nil)
}

// pushLayersByChunksWithPrepareOutput is the writer-aware form used while a
// live build renderer owns the terminal. Status goes through output instead of
// printing over Bubble Tea's frame. A nil output preserves the ordinary CLI
// behavior for callers without a live renderer.
func pushLayersByChunksWithPrepareOutput(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc, output io.Writer) ([]*agentpb.RunContainerLayerHeader, error) {
	return pushLayersByChunksWithPrepareMode(ctx, cs, layers, prepare, output, false, nil)
}

// pushLayersByChunksWithStrictPrepareOutput is for callers, such as Compose,
// that will create a container by image name after this function returns. They
// cannot use the single-container path's lenient "finish during RunContainer"
// behavior: PrepareImage must have registered the named image first.
func pushLayersByChunksWithStrictPrepareOutput(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc, output io.Writer) ([]*agentpb.RunContainerLayerHeader, error) {
	return pushLayersByChunksWithPrepareMode(ctx, cs, layers, prepare, output, true, nil)
}

// pushLayersByChunksWithPrepareMode is the full-parameter core. prog, when
// non-nil, is the single-service live-progress aggregator (WDY-2431/2433):
// it receives layer counts, per-layer chunk plans, and sent-byte updates so
// the interactive bar / plain heartbeat can render the push. Compose callers
// pass nil prog and get their progress through output's optional
// chunk*ProgressWriter interfaces instead; both sinks are nil-safe.
func pushLayersByChunksWithPrepareMode(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc, output io.Writer, strictPrepare bool, prog *chunkPushProgress) ([]*agentpb.RunContainerLayerHeader, error) {
	return pushLayersByChunksWithPrepareModeAndCache(ctx, cs, layers, prepare, output, strictPrepare, prog, loadManifestCache)
}

type cachedManifestResult struct {
	manifest *cachedManifest
	found    bool
}

// pushLayersByChunksWithPrepareModeAndCache is split from the public call path
// so tests can control cache-read timing without touching the process-global
// cache directory. Cache reads are metadata-only: a miss never decompresses or
// chunks a layer until QueryLayers has confirmed that the full layer is absent.
func pushLayersByChunksWithPrepareModeAndCache(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc, output io.Writer, strictPrepare bool, prog *chunkPushProgress, loadCache func(string) (*cachedManifest, bool)) ([]*agentpb.RunContainerLayerHeader, error) {
	headers := make([]*agentpb.RunContainerLayerHeader, len(layers))

	// Start both remote preflight queries and local manifest-cache reads together.
	// QueryChunks is the authoritative capability check: an old agent's
	// Unimplemented error still bubbles up and triggers the registry fallback.
	// QueryLayers remains optional and fails closed to chunking every layer.
	// Only cache metadata is touched here; expensive decompression stays behind
	// the layer-presence result below.
	var (
		present   map[string]int64
		preloaded = make([]cachedManifestResult, len(layers))
	)
	preflightCtx, cancelPreflight := context.WithCancel(ctx)
	defer cancelPreflight()
	capabilityDone := make(chan error, 1)
	go func() {
		_, err := cs.QueryChunks(preflightCtx, &agentpb.QueryChunksRequest{})
		capabilityDone <- err
	}()
	layersDone := make(chan map[string]int64, 1)
	go func() {
		layersDone <- queryPresentLayers(preflightCtx, cs, layers, output)
	}()
	cacheDone := make(chan struct{})
	go func() {
		defer close(cacheDone)
		var cacheGroup errgroup.Group
		cacheGroup.SetLimit(maxConcurrentLayerPush)
		for i, l := range layers {
			i, digest := i, l.Digest
			cacheGroup.Go(func() error {
				if preflightCtx.Err() != nil {
					return nil
				}
				preloaded[i].manifest, preloaded[i].found = loadCache(digest)
				return nil
			})
		}
		_ = cacheGroup.Wait()
	}()
	if err := <-capabilityDone; err != nil {
		cancelPreflight()
		<-cacheDone
		return nil, err
	}
	<-cacheDone
	present = <-layersDone

	// Build the present-layer headers up front and collect the indices that still
	// need a chunk-diff push. A present-layer header carries no chunk hashes, so
	// the agent skips reassembly and reuses the blob already in its content store.
	// We trust that blob for the rest of the deploy: unlike the always-send-chunks
	// path (where QueryChunks would re-report bytes the device lost mid-deploy),
	// nothing re-sends a skipped layer. The window is safe in practice — the blob
	// stays referenced by the app's current image until this deploy replaces it,
	// so containerd GC will not reclaim it underneath us.
	var toPush []int
	skipped := 0
	for i, l := range layers {
		if l.DiffID != "" {
			if size, ok := present[l.DiffID]; ok {
				headers[i] = &agentpb.RunContainerLayerHeader{
					Digest:      l.DiffID,
					DiffId:      l.DiffID,
					Size:        size,
					Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE,
				}
				skipped++
				continue
			}
		}
		toPush = append(toPush, i)
	}
	// Interactive mode already renders this same reused-layer count live, via
	// the progress bar's ticker (chunkPushSnapshot.Line(), fed by
	// SetLayerCounts below) and again in its post-exit Summary() — printing it
	// here too would race cliLogln's direct write to os.Stderr against the
	// live Bubble Tea program rendering to that same fd, garbling the bar on
	// every deploy with layer reuse (WDY-2432/2433 final-review fix wave,
	// finding 3). Callers with their own output writer (compose's live build
	// renderer) still get the line, routed through that writer; the plain
	// non-interactive path keeps it as its only feedback until the
	// end-of-push Summary() line.
	if skipped > 0 && (output != nil || !buildProgressInteractive()) {
		chunkPushLogln(output, "Reusing %s layer(s) already on device; chunking %s.",
			tui.Value(fmt.Sprintf("%d", skipped)), tui.Value(fmt.Sprintf("%d", len(toPush))))
	}
	prog.SetLayerCounts(len(layers), skipped)
	limit := maxConcurrentLayerPush
	if n := runtime.GOMAXPROCS(0); n < limit {
		limit = n
	}
	if limit > len(toPush) {
		limit = len(toPush)
	}
	if limit < 1 {
		limit = 1
	}

	resolved := make([]*resolvedChunkLayer, len(layers))
	indexProgress := newChunkIndexProgress(output)
	transferProgress := newChunkTransferProgress(output)
	defer func() {
		for _, r := range resolved {
			if r != nil {
				r.close()
			}
		}
	}()
	var resolveGroup errgroup.Group
	resolveGroup.SetLimit(limit)
	for _, idx := range toPush {
		idx, l := idx, layers[idx]
		resolveGroup.Go(func() error {
			r, err := resolveChunkLayerWithCache(l, indexProgress, preloaded[idx])
			if err != nil {
				return err
			}
			resolved[idx] = r
			headers[idx] = r.header // distinct index per goroutine — preserves layer order
			return nil
		})
	}
	if err := resolveGroup.Wait(); err != nil {
		return nil, err
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	uploadCtx, cancelUpload := context.WithCancel(ctx)
	defer cancelUpload()
	var prepareDone chan error
	if prepare != nil {
		prepareDone = make(chan error, 1)
		go func() {
			err := prepare(prepareCtx, headers)
			prepareDone <- err
			// Compose cannot complete without strict preparation. Stop sending
			// chunks as soon as an old or unhealthy agent rejects PrepareImage;
			// otherwise we upload the entire delta before discovering the error
			// and then upload the full image again through the registry fallback.
			if strictPrepare && err != nil {
				cancelUpload()
			}
		}()
	}

	uploadGroup, uploadGroupCtx := errgroup.WithContext(uploadCtx)
	uploadGroup.SetLimit(limit)
	for _, idx := range toPush {
		r := resolved[idx]
		uploadGroup.Go(func() error {
			return r.upload(uploadGroupCtx, cs, indexProgress, transferProgress, prog)
		})
	}
	uploadErr := uploadGroup.Wait()
	indexProgress.finish()
	if uploadErr != nil {
		cancelPrepare()
		if prepareDone != nil {
			if prepareErr := <-prepareDone; strictPrepare && prepareErr != nil {
				return nil, prepareErr
			}
		}
		return nil, uploadErr
	}

	if prepareDone != nil {
		var prepareErr error
		select {
		case prepareErr = <-prepareDone:
			// Preparation already finished while chunks were uploading.
		default:
			if reporter, ok := output.(imagePreparationProgressWriter); ok {
				reporter.ReportImagePreparation()
			}
			prepareErr = <-prepareDone
		}
		if err := prepareErr; err != nil {
			if strictPrepare {
				return nil, err
			}
			switch status.Code(err) {
			case codes.InvalidArgument, codes.FailedPrecondition, codes.DataLoss, codes.PermissionDenied, codes.Unauthenticated:
				// Security failures must not silently fall through to the registry
				// path, which has a different trust boundary.
				return nil, err
			case codes.Unimplemented:
				// Older agents use the existing RunContainer assembly/unpack path.
			default:
				// Preparation is an optimization. RunContainer repeats all work
				// idempotently and remains the authoritative error path. Skip the
				// notice when the interactive progress bar owns the terminal and
				// no output writer is routing around it (same garbling hazard as
				// the reused-layer line above).
				if output != nil || !buildProgressInteractive() {
					chunkPushLogln(output, "Image prewarming unavailable (%v); finishing during start.", err)
				}
			}
		}
	}
	return headers, nil
}

// queryPresentLayers asks the device which of these layers it already has, keyed
// by diff ID, returning each present diff ID's uncompressed size. Layers with no
// known diff ID are not queried. The pre-check is a pure optimization: an agent
// too old to implement QueryLayers (Unimplemented), or any query error, yields a
// nil map so the caller chunks every layer as before.
func queryPresentLayers(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, output io.Writer) map[string]int64 {
	diffIDs := make([]string, 0, len(layers))
	for _, l := range layers {
		if l.DiffID != "" {
			diffIDs = append(diffIDs, l.DiffID)
		}
	}
	if len(diffIDs) == 0 {
		return nil
	}
	resp, err := cs.QueryLayers(ctx, &agentpb.QueryLayersRequest{DiffIds: diffIDs})
	if err != nil {
		if ctx.Err() == nil && !isUnimplementedRPCError(err) {
			// The agent supports chunk-diff (the probe succeeded) but the layer
			// pre-check failed for another reason; chunk everything rather than
			// abort the deploy over a missed optimization.
			chunkPushLogln(output, "Layer pre-check unavailable (%v); chunking all layers.", err)
		}
		return nil
	}
	out := make(map[string]int64, len(resp.GetPresent()))
	for _, p := range resp.GetPresent() {
		out[p.GetDiffId()] = p.GetSize()
	}
	return out
}

func chunkPushLogln(output io.Writer, format string, args ...any) {
	if output == nil {
		cliLogln(format, args...)
		return
	}
	fmt.Fprintln(output, cliStyle.Render(fmt.Sprintf(format, args...)))
}

// pushLayerByChunks runs the chunk-diff push for a single layer and returns its
// reassembly header. The uncompressed tar is spilled to a temp file rather than
// held in RAM; missing chunk bytes are read back from it on demand.
type resolvedChunkLayer struct {
	l      localLayer
	header *agentpb.RunContainerLayerHeader
	dl     *decompressedLayer
	refs   []chunk.Ref
}

func (r *resolvedChunkLayer) close() {
	if r.dl != nil {
		r.dl.Close()
		r.dl = nil
	}
}

// ensureDecompressed materializes and chunks a cache-hit layer only when the
// device reports missing chunk bytes. Cache misses already did this during
// manifest resolution and retain the temporary file through upload.
func (r *resolvedChunkLayer) ensureDecompressed(progress *chunkIndexProgress) error {
	if r.dl != nil {
		return nil
	}
	knownSize := r.header.GetSize()
	progress.startLayer(knownSize)
	finished := false
	defer func() {
		if !finished {
			progress.abortLayer(knownSize)
		}
	}()
	var lastCompleted int64
	d, refs, err := decompressAndChunkLayerToTemp(r.l, func(completed int64) {
		progress.addProcessed(completed - lastCompleted)
		lastCompleted = completed
	})
	if err != nil {
		return err
	}
	progress.finishLayer(knownSize, d.size)
	finished = true
	// A cache entry is accepted only for the current chunk algorithm version,
	// so deterministic re-chunking must reproduce its identity and shape.
	if got := d.diffID; got != r.header.GetDiffId() {
		d.Close()
		return fmt.Errorf("cached layer diff ID changed: got %s, want %s", got, r.header.GetDiffId())
	}
	if len(refs) != len(r.header.GetChunkHashes()) {
		d.Close()
		return fmt.Errorf("cached layer chunk count changed: got %d, want %d", len(refs), len(r.header.GetChunkHashes()))
	}
	r.dl, r.refs = d, refs
	return nil
}

func resolveChunkLayer(l localLayer, progress *chunkIndexProgress) (*resolvedChunkLayer, error) {
	m, ok := loadManifestCache(l.Digest)
	return resolveChunkLayerWithCache(l, progress, cachedManifestResult{manifest: m, found: ok})
}

func resolveChunkLayerWithCache(l localLayer, progress *chunkIndexProgress, cached cachedManifestResult) (*resolvedChunkLayer, error) {
	var (
		diffID        string
		size          int64
		orderedHashes [][]byte           // ordered raw 32-byte hashes, for the manifest + QueryChunks
		dl            *decompressedLayer // file-backed tar; populated only when we must produce chunk bytes
		refs          []chunk.Ref        // chunk offsets into dl; populated alongside dl
	)

	// decompressAndChunk streams the layer to a temp file and chunks it in the
	// same pass, filling dl/refs/diffID/size. Both entry points run the
	// identical region+FastCDC algorithm, so these hashes match the device's.
	decompressAndChunk := func() error {
		progress.startLayer(0)
		finished := false
		defer func() {
			if !finished {
				progress.abortLayer(0)
			}
		}()
		var lastCompleted int64
		d, r, err := decompressAndChunkLayerToTemp(l, func(completed int64) {
			progress.addProcessed(completed - lastCompleted)
			lastCompleted = completed
		})
		if err != nil {
			return err
		}
		dl = d
		progress.finishLayer(0, d.size)
		finished = true
		refs, diffID, size = r, d.diffID, d.size
		return nil
	}

	if cached.found {
		diffID, size, orderedHashes = cached.manifest.DiffID, cached.manifest.Size, cached.manifest.Hashes
	} else {
		if err := decompressAndChunk(); err != nil {
			return nil, err
		}
		orderedHashes = make([][]byte, len(refs))
		for i, rf := range refs {
			h := rf.Hash // copy to avoid aliasing the loop variable
			orderedHashes[i] = h[:]
		}
		saveManifestCache(l.Digest, &cachedManifest{DiffID: diffID, Size: size, Hashes: orderedHashes})
	}

	return &resolvedChunkLayer{
		l: l,
		header: &agentpb.RunContainerLayerHeader{
			Digest:      diffID,
			DiffId:      diffID,
			Size:        size,
			Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE,
			ChunkHashes: orderedHashes,
		},
		dl:   dl,
		refs: refs,
	}, nil
}

func (r *resolvedChunkLayer) upload(ctx context.Context, cs agentpb.WendyContainerServiceClient, indexProgress *chunkIndexProgress, transferProgress *chunkTransferProgress, prog *chunkPushProgress) error {
	orderedHashes := r.header.GetChunkHashes()
	qresp, err := cs.QueryChunks(ctx, &agentpb.QueryChunksRequest{ChunkHashes: orderedHashes})
	if err != nil {
		return fmt.Errorf("querying missing chunks for layer %s: %w", r.header.GetDiffId(), err)
	}
	unique := make(map[[32]byte]bool, len(orderedHashes))
	for _, hb := range orderedHashes {
		var h [32]byte
		copy(h[:], hb)
		unique[h] = true
	}
	missing := make(map[[32]byte]bool, len(qresp.GetMissingHashes()))
	for _, hb := range qresp.GetMissingHashes() {
		var h [32]byte
		copy(h[:], hb)
		missing[h] = true
	}

	if len(missing) > 0 {
		// The device needs some chunks, so we must produce their bytes. If a
		// cache hit let us skip decompression above, do it now. Re-chunking here
		// reproduces the exact hashes in `missing` only because chunking is
		// deterministic and loadManifestCache rejects manifests from a different
		// AlgoVersion — so the cached hashes always match what ChunkReaderAt emits.
		if err := r.ensureDecompressed(indexProgress); err != nil {
			return err
		}
		var missingBytes int64
		planned := make(map[[32]byte]bool, len(missing))
		for _, ref := range r.refs {
			if !missing[ref.Hash] || planned[ref.Hash] {
				continue
			}
			planned[ref.Hash] = true
			missingBytes += int64(ref.Len)
		}
		transferProgress.addTotal(missingBytes)
		prog.LayerPlanned(len(unique), len(missing), missingBytes)
		var wc grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]
		chunksInStream := 0
		for chunkIndex, ref := range r.refs {
			if !missing[ref.Hash] {
				continue
			}
			if wc == nil {
				// Hardware reproduction showed that particular raw chunk payloads can
				// stall a USB-NCM link while the compressed registry path succeeds.
				// Give the fast path the same property; gRPC decompresses the message
				// before the agent hashes and stages the original bytes.
				wc, err = cs.WriteChunks(ctx, grpc.UseCompressor(grpcgzip.Name))
				if err != nil {
					return fmt.Errorf("opening chunk upload for layer %s: %w", r.header.GetDiffId(), err)
				}
			}
			buf := make([]byte, ref.Len) // ref.Len <= chunk.MaxSize (64 KiB)
			if _, err := r.dl.f.ReadAt(buf, int64(ref.Offset)); err != nil {
				return fmt.Errorf("reading chunk %d/%d for layer %s: %w", chunkIndex+1, len(r.refs), r.header.GetDiffId(), err)
			}
			hb := ref.Hash // copy
			if err := wc.Send(&agentpb.WriteChunksRequest{
				Hash: hb[:],
				Data: buf,
			}); err != nil {
				// grpc-go reports io.EOF from Send when the server has already
				// closed a client-streaming RPC. CloseAndRecv carries the actual
				// terminal status (for example ResourceExhausted or InvalidArgument);
				// without this read the CLI hides the actionable agent error behind
				// a bare EOF and incorrectly treats it as a transport drop.
				if errors.Is(err, io.EOF) {
					if _, terminalErr := wc.CloseAndRecv(); terminalErr != nil {
						err = terminalErr
					}
				}
				return fmt.Errorf("sending chunk %d/%d for layer %s: %w", chunkIndex+1, len(r.refs), r.header.GetDiffId(), err)
			}
			// The ordered manifest may reference identical content more than
			// once. A single staged copy satisfies every occurrence.
			delete(missing, ref.Hash)
			prog.ChunkSent(len(buf))
			transferProgress.addSent(int64(len(buf)))
			chunksInStream++
			if chunksInStream == maxChunksPerWriteStream {
				if _, err := wc.CloseAndRecv(); err != nil {
					return fmt.Errorf("closing chunk upload batch after chunk %d/%d for layer %s: %w", chunkIndex+1, len(r.refs), r.header.GetDiffId(), err)
				}
				wc = nil
				chunksInStream = 0
			}
		}
		if wc != nil {
			_, err = wc.CloseAndRecv()
		}
		if err != nil {
			return fmt.Errorf("closing chunk upload for layer %s: %w", r.header.GetDiffId(), err)
		}
	} else {
		prog.LayerPlanned(len(unique), 0, 0)
	}
	return nil
}

func pushLayerByChunks(ctx context.Context, cs agentpb.WendyContainerServiceClient, l localLayer) (*agentpb.RunContainerLayerHeader, error) {
	r, err := resolveChunkLayer(l, nil)
	if err != nil {
		return nil, err
	}
	defer r.close()
	if err := r.upload(ctx, cs, nil, nil, nil); err != nil {
		return nil, err
	}
	return r.header, nil
}
