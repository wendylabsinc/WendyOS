package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

const (
	// chunkStoreIdleAfter is how long the chunk store must see no chunk RPC,
	// with no assembly in flight, before periodic maintenance may sweep staged
	// chunks: a deploy touches the store at least once per chunk it sends.
	chunkStoreIdleAfter = 10 * time.Minute
	// chunkStoreMaintenanceInterval paces the idle-time maintenance pass.
	chunkStoreMaintenanceInterval = 15 * time.Minute
	// stagingRetention keeps the chunks of a cancelled or failed deploy long
	// enough for the user's next attempt to resume from them (WDY-3217).
	stagingRetention = 6 * time.Hour
	// cachePruneStagingIdleAfter is the quiet period an explicit cache prune
	// requires before it removes every staged chunk, whatever its age. A
	// deploy in progress is guarded by PrepareImage's in-flight marker
	// (chunkActivity.begin), held from its start until its last layer is
	// assembled however long the upload pauses. This window guards the chunks
	// that flow before PrepareImage starts: every chunk RPC touches the store.
	//
	// Deploys that never call PrepareImage (CLIs older than 2026-08-12, or the
	// current CLI after a non-security PrepareImage error falls back to
	// assembling in RunContainer) have only the per-chunk touch: a gap of over
	// a minute with no chunk RPC lets a concurrent prune remove their staged
	// chunks, RunContainer then fails on a missing chunk, and a rerun
	// re-uploads it.
	cachePruneStagingIdleAfter = time.Minute
	// retiredStagingSuffix marks a staging directory from an earlier agent run.
	retiredStagingSuffix = ".retired-"
)

// chunkActivity tracks whether a deploy may be relying on staged chunks.
type chunkActivity struct {
	lastNano atomic.Int64
	inFlight atomic.Int32
}

func (a *chunkActivity) touch() { a.lastNano.Store(time.Now().UnixNano()) }

// begin marks an assembly or image preparation in flight until the returned
// func runs.
func (a *chunkActivity) begin() func() {
	a.inFlight.Add(1)
	a.touch()
	return func() {
		a.touch()
		a.inFlight.Add(-1)
	}
}

// idleFor reports whether nothing is in flight and no chunk RPC has touched
// the store for at least d.
func (a *chunkActivity) idleFor(d time.Duration, now time.Time) bool {
	if a.inFlight.Load() > 0 {
		return false
	}
	last := a.lastNano.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= d
}

// sweep removes staged chunk files, and temp files a crash orphaned, last
// modified before cutoff.
func (s *staging) sweep(cutoff time.Time) (files int, bytes int64, err error) {
	return s.walk(cutoff, true)
}

// usage reports what sweep(cutoff) would remove.
func (s *staging) usage(cutoff time.Time) (files int, bytes int64, err error) {
	return s.walk(cutoff, false)
}

// walk reports, or with remove deletes, the staged files last modified before
// cutoff. Files it fails to delete come back as one error after the pass,
// with a count and the first failure: staging has no logger, and a line per
// file would repeat on every pass.
func (s *staging) walk(cutoff time.Time, remove bool) (files int, bytes int64, err error) {
	d, err := os.Open(s.dir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer d.Close()
	var failed int
	var firstFailure error
	for {
		entries, rerr := d.ReadDir(1024)
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if remove {
				if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !os.IsNotExist(err) {
					if failed == 0 {
						firstFailure = err
					}
					failed++
					continue
				}
			}
			files++
			bytes += info.Size()
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
	}
	if failed > 0 {
		err = errors.Join(err, fmt.Errorf("%d staged files could not be removed, the first: %w", failed, firstFailure))
	}
	return files, bytes, err
}

// retire renames the staging directory aside so this agent run starts with an
// empty one. No deploy in this process can be relying on a chunk staged by an
// earlier run, and renaming is instant, so leftovers of any size can be
// deleted in the background without racing new uploads.
func (s *staging) retire(now time.Time) error {
	err := os.Rename(s.dir, fmt.Sprintf("%s%s%d", s.dir, retiredStagingSuffix, now.UnixNano()))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// purgeRetired deletes every staging directory retire moved aside, including
// ones a crash left behind before their purge finished.
func (s *staging) purgeRetired() (files int, bytes int64, err error) {
	dirs, err := filepath.Glob(s.dir + retiredStagingSuffix + "*")
	if err != nil {
		return 0, 0, err
	}
	for _, dir := range dirs {
		n, b, werr := newStaging(dir).sweep(time.Now().Add(time.Hour))
		files, bytes = files+n, bytes+b
		if werr != nil {
			err = werr
			continue
		}
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = rerr
		}
	}
	return files, bytes, err
}

// StartChunkStoreMaintenance keeps the chunk store bounded (WDY-3212,
// WDY-3217). Call it before the agent serves RPCs.
//
// At start, before it returns, it retires the live staging directory, so this
// run starts with an empty one. Then, in the background and without waiting
// for the store to go idle, it purges the retired directories and reconciles
// the index once, dropping the entries of layer blobs containerd no longer
// holds.
//
// After that, every chunkStoreMaintenanceInterval, if no deploy has used the
// store for chunkStoreIdleAfter, it sweeps staged chunks older than
// stagingRetention and reconciles the index again.
func (c *Client) StartChunkStoreMaintenance(ctx context.Context) {
	if err := c.staging.retire(time.Now()); err != nil {
		c.logger.Warn("Retiring leftover chunk staging failed", zap.Error(err))
	}
	go c.runChunkStoreMaintenance(ctx, chunkStoreMaintenanceInterval)
}

func (c *Client) runChunkStoreMaintenance(ctx context.Context, interval time.Duration) {
	c.runMaintenancePass(func() {
		files, bytes, err := c.staging.purgeRetired()
		if err != nil {
			c.logger.Warn("Deleting leftover chunk staging failed", zap.Error(err))
		}
		dropped, err := c.reconcileChunkIndex(ctx)
		if err != nil {
			c.logger.Warn("Reconciling chunk index failed", zap.Error(err))
		}
		c.logChunkStoreMaintenance(files, bytes, dropped)
	})

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.runMaintenancePass(func() { c.maintainIdleChunkStore(ctx, now) })
		}
	}
}

// runMaintenancePass runs one maintenance pass and logs a panic instead of
// letting it kill the agent. The chunk store is a cache, and the next pass
// tries again.
func (c *Client) runMaintenancePass(pass func()) {
	defer func() {
		if p := recover(); p != nil {
			c.logger.Error("Chunk store maintenance panicked", zap.Any("panic", p), zap.Stack("stack"))
		}
	}()
	pass()
}

// maintainIdleChunkStore sweeps staged chunks older than stagingRetention and
// reconciles the index, unless a deploy has used the store recently.
func (c *Client) maintainIdleChunkStore(ctx context.Context, now time.Time) {
	files, bytes, idle, err := c.sweepIdleStaging(now)
	if !idle {
		return
	}
	if err != nil {
		c.logger.Warn("Sweeping chunk staging failed", zap.Error(err))
	}
	dropped, err := c.reconcileChunkIndex(ctx)
	if err != nil {
		c.logger.Warn("Reconciling chunk index failed", zap.Error(err))
	}
	c.logChunkStoreMaintenance(files, bytes, dropped)
}

// sweepIdleStaging sweeps staged chunks older than stagingRetention unless the
// store was used within chunkStoreIdleAfter. It takes chunkSweepMu before it
// checks idleness, so an in-flight query or stage finishes, and touches the
// store, first. The deferred unlock keeps a panicking sweep from leaving every
// later chunk RPC blocked on the lock.
func (c *Client) sweepIdleStaging(now time.Time) (files int, bytes int64, idle bool, err error) {
	c.chunkSweepMu.Lock()
	defer c.chunkSweepMu.Unlock()
	if !c.chunkActivity.idleFor(chunkStoreIdleAfter, now) {
		return 0, 0, false, nil
	}
	files, bytes, err = c.staging.sweep(now.Add(-stagingRetention))
	return files, bytes, true, err
}

func (c *Client) logChunkStoreMaintenance(files int, bytes int64, droppedBlobs int) {
	if files == 0 && droppedBlobs == 0 {
		return
	}
	c.logger.Info("Chunk store maintenance",
		zap.Int("staged_chunks_removed", files),
		zap.Int64("staged_bytes_removed", bytes),
		zap.Int("index_blobs_dropped", droppedBlobs))
}

// reconcileChunkIndex drops the entries of every indexed blob containerd no
// longer holds and returns how many blobs it dropped. A blob containerd could
// not answer for, as at startup before it serves, ends the pass: that blob and
// the ones after it wait for the next pass.
func (c *Client) reconcileChunkIndex(ctx context.Context) (int, error) {
	blobs, err := c.chunkIndex.Blobs()
	if err != nil {
		return 0, err
	}
	ctx = c.withNamespace(ctx)
	cs := c.client.ContentStore()
	var stale []string
	var checkErr error
	for _, blob := range blobs {
		if dgst, perr := digest.Parse(blob); perr == nil {
			_, ierr := cs.Info(ctx, dgst)
			if ierr == nil {
				continue
			}
			if !errdefs.IsNotFound(ierr) {
				checkErr = fmt.Errorf("checking indexed blob %s: %w", blob, ierr)
				break
			}
		}
		stale = append(stale, blob)
	}
	// In batches, not a transaction per blob: the first pass after the legacy
	// import can find hundreds of stale blobs.
	dropped, err := c.chunkIndex.DropBlobs(stale)
	return dropped, errors.Join(checkErr, err)
}

// pruneChunkStore extends a cache prune to the chunk store (WDY-3212,
// WDY-3217). It removes every staged chunk, or on a dry run counts them,
// unless the store is in use: a chunk RPC within cachePruneStagingIdleAfter,
// or an image preparation or layer assembly in flight. A real prune of an
// idle store then drops the index entries of every layer blob containerd no
// longer holds; PruneCache calls it after the forced GC, so that includes the
// blobs the prune just let containerd collect.
//
// While the store is in use, the real prune defers that reconcile to idle
// maintenance or the next prune: a deploy can be assembling a layer the GC
// just collected, and a reconcile that found the blob missing just before the
// assembly committed it would drop the entries the assembly then wrote.
//
// Failures are logged, not returned: the cache-root release before this step
// already happened and stands on its own. A Client built without a staging
// area or an index, as some tests build it, skips that part.
func (c *Client) pruneChunkStore(ctx context.Context, now time.Time, dryRun bool, result *services.CachePruneResult) {
	if c.staging != nil {
		files, bytes, inUse, err := c.pruneStaging(now, dryRun)
		if err != nil {
			c.logger.Warn("Pruning chunk staging failed", zap.Error(err))
		}
		result.StagedChunks, result.StagedBytes, result.StagingInUse = uint64(files), uint64(bytes), inUse
	}
	if dryRun || c.chunkIndex == nil {
		return
	}
	if !result.StagingInUse {
		dropped, err := c.reconcileChunkIndex(ctx)
		if err != nil {
			c.logger.Warn("Reconciling chunk index after cache prune failed", zap.Error(err))
		}
		result.ChunkIndexBlobsDropped = uint64(dropped)
	}
	c.logger.Info("Cache prune pruned the chunk store",
		zap.Uint64("staged_chunks_removed", result.StagedChunks),
		zap.Uint64("staged_bytes_removed", result.StagedBytes),
		zap.Bool("staging_in_use", result.StagingInUse),
		zap.Uint64("index_blobs_dropped", result.ChunkIndexBlobsDropped),
		zap.Bool("reconcile_deferred", result.StagingInUse))
}

// pruneStaging removes, or with dryRun counts, every staged chunk unless a
// deploy used the store within cachePruneStagingIdleAfter; inUse reports that
// it left staging alone. Like sweepIdleStaging, it checks idleness under
// chunkSweepMu, so an in-flight query or stage finishes, and touches the
// store, first, and it unlocks in a defer.
func (c *Client) pruneStaging(now time.Time, dryRun bool) (files int, bytes int64, inUse bool, err error) {
	c.chunkSweepMu.Lock()
	defer c.chunkSweepMu.Unlock()
	if !c.chunkActivity.idleFor(cachePruneStagingIdleAfter, now) {
		return 0, 0, true, nil
	}
	if dryRun {
		files, bytes, err = c.staging.usage(now)
	} else {
		files, bytes, err = c.staging.sweep(now)
	}
	return files, bytes, false, err
}
