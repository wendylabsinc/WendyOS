package containerd

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	digest "github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

// cachePruneGracePeriod keeps cache roots that may belong to an in-flight
// deployment. A layer push spans multiple RPCs, so there is no single lease
// covering the entire transfer until AssembleImage creates the final image
// reference. A day safely covers even unusually large/slow edge deployments.
//
// `--all` (min age 0) bypasses this protection entirely: WriteLayer writes
// layers with a gc.root label and no lease (AssembleLayerFromChunks, in
// chunkstore.go, funnels into the same WriteLayer, so the chunk path is
// covered by this statement too), so releasing every pin regardless of age
// must not run while a deploy to this device is in progress.
const cachePruneGracePeriod = 24 * time.Hour

type cacheContentStore interface {
	Walk(context.Context, content.WalkFunc, ...string) error
	Update(context.Context, content.Info, ...string) (content.Info, error)
}

type cacheSnapshotter interface {
	Walk(context.Context, snapshots.WalkFunc, ...string) error
	Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error)
	Usage(context.Context, string) (snapshots.Usage, error)
}

type contentCacheCandidate struct {
	info content.Info
}

type snapshotCacheCandidate struct {
	info  snapshots.Info
	usage snapshots.Usage
}

// PruneCache releases Wendy's explicit GC roots from cache entries old enough
// not to belong to an in-flight deployment. Containerd's own reference graph
// remains authoritative, preserving current images and active containers while
// its normal GC reclaims entries that are now unreachable.
//
// Unless this is a dry run, PruneCache also forces a synchronous containerd GC
// pass after releasing pins (so callers get a real, measured reclaim rather
// than waiting on containerd's background GC) and reports the free-space
// delta on the container-storage filesystem as ReclaimedBytes.
//
// c.mu is held only around the label walk/update (pruneCacheRoots) and, on a
// real (non-dry) run, the "before" free-space measurement taken immediately
// after acquiring the lock — not around the forced GC pass or the "after"
// measurement. CreateContainerWithProgress, StopContainer/stopOne and
// DeleteContainer also take c.mu, and a synchronous GC sweep over the whole
// content store has no bound on how long it can run; holding the lock across
// it would stall an unrelated deploy or stop for the duration of the sweep.
// The "before" statfs is a microsecond syscall with no GC under the lock, so
// it is safe to take there; neither it nor the lease create/delete touches
// state c.mu protects, so releasing the lock before the GC pass is safe. The
// locked section is a closure with a deferred Unlock so a panic inside it
// (e.g. a Walk callback or sn.Usage) cannot leave c.mu held forever — the
// agent's gRPC interceptor recovers handler panics and keeps serving, so an
// un-deferred Unlock skipped by a panic would hang every later
// create/stop/delete.
func (c *Client) PruneCache(ctx context.Context, opts services.CachePruneOptions) (services.CachePruneResult, error) {
	ctx = c.withNamespace(ctx)
	cutoff, effective := pruneCutoff(time.Now(), opts.MinAge)

	// freeBytes/forceGC are set by NewClient; a bare *Client built directly
	// by a test has neither, so fall back to the real implementations lazily
	// rather than nil-deref below.
	if c.freeBytes == nil {
		c.freeBytes = filesystemFreeBytes
	}
	if c.forceGC == nil {
		c.forceGC = func(ctx context.Context) error {
			return forceContainerdGC(ctx, c.client.LeasesService())
		}
	}

	// Dry runs never force GC or measure free space; skip "before" too so a
	// dry run never touches the seam.
	var before uint64
	var okBefore bool
	result, err := func() (services.CachePruneResult, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !opts.DryRun {
			before, okBefore = c.freeBytes(containerdRootDir)
		}
		return pruneCacheRoots(
			ctx,
			c.client.ContentStore(),
			c.client.SnapshotService(c.snapshotter),
			cutoff,
			opts.DryRun,
		)
	}()

	result.MinimumAgeSeconds = uint64(effective / time.Second)
	if err != nil || opts.DryRun {
		return result, err
	}

	var gcErr error
	if gcErr = c.forceGC(ctx); gcErr != nil {
		c.logger.Warn("prune cache: forcing a synchronous containerd GC pass failed; pins were already released and containerd's background GC will still reclaim them", zap.Error(gcErr))
	}
	after, okAfter := c.freeBytes(containerdRootDir)
	result.ReclaimedBytes = reclaimedBetween(before, after, okBefore && okAfter && gcErr == nil)

	return result, nil
}

// pruneCutoff computes the cutoff time and effective minimum age for a
// PruneCache call. A nil minAge uses the agent's default grace period. Only
// an explicit 0 means "release every pin regardless of age"; a negative
// minAge is not a valid request for that and fails safe to the default grace
// period instead of releasing everything.
func pruneCutoff(now time.Time, minAge *time.Duration) (cutoff time.Time, effective time.Duration) {
	switch {
	case minAge == nil:
		effective = cachePruneGracePeriod
	case *minAge < 0:
		effective = cachePruneGracePeriod
	default:
		effective = *minAge
	}
	return now.Add(-effective), effective
}

// leaseGCer is the subset of leases.Manager forceContainerdGC needs to force
// a synchronous GC pass via an ephemeral lease.
type leaseGCer interface {
	Create(context.Context, ...leases.Opt) (leases.Lease, error)
	Delete(context.Context, leases.Lease, ...leases.DeleteOpt) error
}

// forceContainerdGC forces containerd to run a synchronous garbage collection
// pass. It creates a randomly-named, short-lived, throwaway lease and
// immediately deletes it with leases.SynchronousDelete, which makes the
// containerd server run gc.ScheduleAndWait before the delete returns.
func forceContainerdGC(ctx context.Context, lm leaseGCer) error {
	l, err := lm.Create(ctx, leases.WithRandomID(), leases.WithExpiration(time.Minute))
	if err != nil {
		return fmt.Errorf("creating throwaway GC lease: %w", err)
	}
	if err := lm.Delete(ctx, l, leases.SynchronousDelete); err != nil {
		return fmt.Errorf("deleting throwaway GC lease: %w", err)
	}
	return nil
}

// reclaimedBetween reports the free-space delta between two filesystem
// measurements, or nil when either measurement was unavailable (ok is
// false). A delta that went negative (free space shrank, e.g. concurrent
// writes during the GC pass) clamps to 0 rather than reporting a fabricated
// negative reclaim.
func reclaimedBetween(before, after uint64, ok bool) *uint64 {
	if !ok {
		return nil
	}
	var delta uint64
	if after > before {
		delta = after - before
	}
	return &delta
}

func pruneCacheRoots(ctx context.Context, cs cacheContentStore, sn cacheSnapshotter, cutoff time.Time, dryRun bool) (services.CachePruneResult, error) {
	var (
		result             services.CachePruneResult
		contentCandidates  []contentCacheCandidate
		snapshotCandidates []snapshotCacheCandidate
	)

	if err := cs.Walk(ctx, func(info content.Info) error {
		if info.Labels[labelKeyWendyLayer] != "true" || !cacheRootOlderThan(info.Labels[labelKeyGCRoot], cutoff) {
			return nil
		}
		contentCandidates = append(contentCandidates, contentCacheCandidate{info: info})
		result.ContentBlobs++
		if info.Size > 0 {
			result.ContentBytes += uint64(info.Size)
		}
		return nil
	}); err != nil {
		return services.CachePruneResult{}, fmt.Errorf("listing cached content: %w", err)
	}

	if err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if info.Kind != snapshots.KindCommitted || !isWendyCacheSnapshot(info) || !cacheRootOlderThan(info.Labels[labelKeyGCRoot], cutoff) {
			return nil
		}
		usage, _ := sn.Usage(ctx, info.Name) // best-effort accounting must not block cleanup
		snapshotCandidates = append(snapshotCandidates, snapshotCacheCandidate{info: info, usage: usage})
		result.Snapshots++
		if usage.Size > 0 {
			result.SnapshotBytes += uint64(usage.Size)
		}
		return nil
	}); err != nil {
		return services.CachePruneResult{}, fmt.Errorf("listing cached snapshots: %w", err)
	}

	if dryRun {
		return result, nil
	}

	for _, candidate := range contentCandidates {
		candidate.info.Labels = labelsWithout(candidate.info.Labels, labelKeyGCRoot)
		if _, err := cs.Update(ctx, candidate.info, "labels"); err != nil {
			return result, fmt.Errorf("releasing cache root from content %s: %w", candidate.info.Digest, err)
		}
	}
	for _, candidate := range snapshotCandidates {
		candidate.info.Labels = labelsWithout(candidate.info.Labels, labelKeyGCRoot)
		if _, err := sn.Update(ctx, candidate.info, "labels"); err != nil {
			return result, fmt.Errorf("releasing cache root from snapshot %s: %w", candidate.info.Name, err)
		}
	}

	return result, nil
}

func cacheRootOlderThan(value string, cutoff time.Time) bool {
	if value == "" {
		return false
	}
	created, err := time.Parse(time.RFC3339, value)
	return err == nil && !created.After(cutoff)
}

func isWendyCacheSnapshot(info snapshots.Info) bool {
	if info.Labels[labelKeyWendySnapshot] == "true" {
		return true
	}
	// Agents predating labelKeyWendySnapshot used the OCI chain ID as the
	// snapshot name and set the same GC root. Recognize that exact legacy shape
	// so upgrades can clean the cache already accumulated on devices.
	d, err := digest.Parse(info.Name)
	return err == nil && d.Algorithm() == digest.SHA256
}

func labelsWithout(labels map[string]string, key string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != key {
			out[k] = v
		}
	}
	return out
}
