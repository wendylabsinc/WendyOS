package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// stageAged stages data and backdates its file by age.
func stageAged(t *testing.T, s *staging, data []byte, age time.Duration) [32]byte {
	t.Helper()
	h := sha256.Sum256(data)
	if err := s.write(h, data); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(s.path(h), old, old); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestChunkActivityIdleFor(t *testing.T) {
	var a chunkActivity
	now := time.Now()
	if !a.idleFor(time.Minute, now) {
		t.Fatal("a store that never saw a chunk RPC is idle")
	}
	a.touch()
	if a.idleFor(time.Minute, time.Now()) {
		t.Fatal("a store touched just now is not idle")
	}
	if !a.idleFor(time.Minute, time.Now().Add(2*time.Minute)) {
		t.Fatal("a store untouched for longer than d is idle")
	}
	end := a.begin()
	if a.idleFor(time.Minute, time.Now().Add(time.Hour)) {
		t.Fatal("a store with an assembly in flight is never idle")
	}
	end()
	if !a.idleFor(time.Minute, time.Now().Add(2*time.Minute)) {
		t.Fatal("the store is idle again once the assembly ends")
	}
}

func TestStagingSweepRemovesOnlyFilesOlderThanCutoff(t *testing.T) {
	s := newStaging(t.TempDir())
	old := stageAged(t, s, []byte("abandoned chunk"), 7*time.Hour)
	fresh := stageAged(t, s, []byte("chunk of a live deploy"), time.Minute)
	orphan := filepath.Join(s.dir, "stage-123")
	if err := os.WriteFile(orphan, []byte("temp file a crash left"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-7 * time.Hour)
	if err := os.Chtimes(orphan, aged, aged); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-stagingRetention)
	if files, bytes, err := s.usage(cutoff); err != nil || files != 2 || bytes == 0 {
		t.Fatalf("usage = %d files, %d bytes, %v; want the 2 aged files", files, bytes, err)
	}
	files, _, err := s.sweep(cutoff)
	if err != nil || files != 2 {
		t.Fatalf("sweep removed %d files, %v; want 2", files, err)
	}
	if s.has(old) || !s.has(fresh) {
		t.Fatalf("after sweep: old present=%v, fresh present=%v", s.has(old), s.has(fresh))
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned temp file survived the sweep")
	}
}

// TestStagingSweepReportsFilesItCouldNotRemove: a sweep that cannot delete
// files says so once per pass, with a count and the first error, rather than
// dropping the failures or logging a line per file.
func TestStagingSweepReportsFilesItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, directory permissions do not deny deletes")
	}
	s := newStaging(t.TempDir())
	stageAged(t, s, []byte("abandoned chunk one"), 7*time.Hour)
	stageAged(t, s, []byte("abandoned chunk two"), 7*time.Hour)
	if err := os.Chmod(s.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.dir, 0o700) })

	files, _, err := s.sweep(time.Now().Add(-stagingRetention))
	if files != 0 {
		t.Fatalf("sweep reported %d files removed from a read-only directory", files)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "2 staged files could not be removed") || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("sweep error = %v; want one error counting both failures and wrapping the first", err)
	}
}

func TestStagingRetireStartsEmptyAndPurgeDeletesLeftovers(t *testing.T) {
	s := newStaging(filepath.Join(t.TempDir(), "staging"))
	leftover := stageAged(t, s, []byte("chunk from the previous agent run"), time.Hour)

	if err := s.retire(time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.has(leftover) {
		t.Fatal("retired chunk is still visible to this run")
	}
	data := []byte("chunk staged after startup")
	if err := s.write(sha256.Sum256(data), data); err != nil {
		t.Fatalf("staging after retire: %v", err)
	}

	files, _, err := s.purgeRetired()
	if err != nil || files != 1 {
		t.Fatalf("purgeRetired removed %d files, %v; want 1", files, err)
	}
	if left, _ := filepath.Glob(s.dir + retiredStagingSuffix + "*"); len(left) != 0 {
		t.Fatalf("retired dirs left behind: %v", left)
	}
	if !s.has(sha256.Sum256(data)) {
		t.Fatal("purge removed a chunk staged by this run")
	}
}

// recordingLeases is a leases.Manager that counts the leases created and the
// synchronous deletes. On each synchronous delete it runs collect, if set, as
// containerd runs its garbage collector before such a delete returns.
type recordingLeases struct {
	leases.Manager
	created, syncDeletes int
	collect              func()
}

func (l *recordingLeases) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	var lease leases.Lease
	for _, opt := range opts {
		if err := opt(&lease); err != nil {
			return leases.Lease{}, err
		}
	}
	l.created++
	return lease, nil
}

func (l *recordingLeases) Delete(ctx context.Context, _ leases.Lease, opts ...leases.DeleteOpt) error {
	var do leases.DeleteOptions
	for _, opt := range opts {
		if err := opt(ctx, &do); err != nil {
			return err
		}
	}
	if do.Synchronous {
		l.syncDeletes++
		if l.collect != nil {
			l.collect()
		}
	}
	return nil
}

// newMaintenanceClient builds a Client over a fake content store (holding
// only present) and a lease recorder, with its own index and staging dir.
func newMaintenanceClient(t *testing.T, present ...digest.Digest) (*Client, *recordingLeases) {
	t.Helper()
	blobs := map[digest.Digest]content.Info{}
	for _, d := range present {
		blobs[d] = content.Info{Digest: d, Size: 1 << 20}
	}
	ls := &recordingLeases{}
	client, err := containerdclient.New("",
		containerdclient.WithDefaultNamespace("default"),
		containerdclient.WithServices(
			containerdclient.WithContentStore(&chunkAvailabilityContentStore{blobs: blobs}),
			containerdclient.WithLeasesService(ls),
		),
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
	}, ls
}

func TestReconcileChunkIndexDropsBlobsContainerdNoLongerHolds(t *testing.T) {
	kept := digest.FromString("kept layer")
	c, _ := newMaintenanceClient(t, kept)
	if err := c.chunkIndex.AddLayer(kept.String(), []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	const collected = 3
	for i := range collected {
		blob := digest.FromString(fmt.Sprint("collected layer ", i)).String()
		if err := c.chunkIndex.AddLayer(blob, []chunk.Ref{{Hash: [32]byte{2, byte(i)}, Len: 1}}); err != nil {
			t.Fatal(err)
		}
	}

	before := committedTxID(t, c.chunkIndex)
	dropped, err := c.reconcileChunkIndex(context.Background())
	if err != nil || dropped != collected {
		t.Fatalf("reconcile dropped %d, %v; want %d", dropped, err, collected)
	}
	if n := committedTxID(t, c.chunkIndex) - before; n != 1 {
		t.Fatalf("dropping %d collected blobs took %d transactions, want 1", collected, n)
	}
	for i := range collected {
		if _, ok := c.chunkIndex.Has([32]byte{2, byte(i)}); ok {
			t.Fatalf("entry of collected blob %d survived", i)
		}
	}
	if _, ok := c.chunkIndex.Has([32]byte{1}); !ok {
		t.Fatal("entry of a present blob was dropped")
	}
}

// TestReconcileChunkIndexKeepsEntriesContainerdCannotAnswerFor: the startup
// reconcile can run before containerd's content service answers. Unavailable
// is not NotFound, so the pass fails and the blob's entries stay.
func TestReconcileChunkIndexKeepsEntriesContainerdCannotAnswerFor(t *testing.T) {
	blob := digest.FromString("layer containerd could not be asked about")
	cs := &chunkAvailabilityContentStore{
		errs: map[digest.Digest]error{blob: fmt.Errorf("connection refused: %w", errdefs.ErrUnavailable)},
	}
	c := newChunkAvailabilityClient(t, cs, newTestChunkIndex(t), filepath.Join(t.TempDir(), "staging"))
	if err := c.chunkIndex.AddLayer(blob.String(), []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatal(err)
	}

	dropped, err := c.reconcileChunkIndex(context.Background())
	if !errdefs.IsUnavailable(err) || dropped != 0 {
		t.Fatalf("reconcile = %d, %v; want nothing dropped and the Unavailable error", dropped, err)
	}
	if _, ok := c.chunkIndex.Has([32]byte{1}); !ok {
		t.Fatal("the entry of a blob containerd could not answer for was dropped")
	}
	if blobs, err := c.chunkIndex.Blobs(); err != nil || len(blobs) != 1 {
		t.Fatalf("Blobs = %v, %v; want the blob still listed", blobs, err)
	}
}

// panickingContentStore stands in for any bug a maintenance pass could hit.
type panickingContentStore struct{ content.Store }

func (panickingContentStore) Info(context.Context, digest.Digest) (content.Info, error) {
	panic("content store bug")
}

// TestChunkStoreMaintenanceSurvivesAPanickingPass: nothing above the
// maintenance goroutine recovers a panic, so one escaping it would kill the
// agent.
func TestChunkStoreMaintenanceSurvivesAPanickingPass(t *testing.T) {
	c := newChunkAvailabilityClient(t, panickingContentStore{}, newTestChunkIndex(t), filepath.Join(t.TempDir(), "staging"))
	core, logs := observer.New(zap.ErrorLevel)
	c.logger = zap.New(core)
	if err := c.chunkIndex.AddLayer(digest.FromString("layer").String(), []chunk.Ref{{Hash: [32]byte{1}, Len: 1}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // return right after the startup pass
	c.runChunkStoreMaintenance(ctx, time.Hour)
	if n := logs.FilterMessage("Chunk store maintenance panicked").Len(); n != 1 {
		t.Fatalf("logged %d maintenance panics, want 1", n)
	}
}

// TestMissingChunksKeepsAReportedStagedChunkClearOfTheSweep: a chunk staged
// long ago and reported present may be read much later. A build queued behind
// a long one reads its context only when it runs, while the store sits idle.
// An idle sweep in between must not take the chunk.
func TestMissingChunksKeepsAReportedStagedChunkClearOfTheSweep(t *testing.T) {
	c, _ := newMaintenanceClient(t)
	data := []byte("build-context chunk staged by a build seven hours ago")
	old := stageAged(t, c.staging, data, 7*time.Hour)
	recent := stageAged(t, c.staging, []byte("chunk staged an hour ago"), time.Hour)
	recentBefore, err := os.Stat(c.staging.path(recent))
	if err != nil {
		t.Fatal(err)
	}

	missing, err := c.MissingChunks(context.Background(), [][32]byte{old, recent})
	if err != nil || len(missing) != 0 {
		t.Fatalf("MissingChunks = %x, %v; want both staged chunks reported present", missing, err)
	}
	// Only files a sweep could reach soon are refreshed.
	if recentAfter, err := os.Stat(c.staging.path(recent)); err != nil || !recentAfter.ModTime().Equal(recentBefore.ModTime()) {
		t.Fatalf("a chunk staged an hour ago was touched: %v", err)
	}

	c.maintainIdleChunkStore(context.Background(), time.Now().Add(chunkStoreIdleAfter+time.Minute))
	if !c.staging.has(old) {
		t.Fatal("the idle sweep took a chunk MissingChunks had just reported present")
	}
	got, err := io.ReadAll(c.OpenChunkStream(context.Background(), [][32]byte{old}))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reading the reported chunk after the sweep = %q, %v", got, err)
	}
}

func TestMaintainIdleChunkStoreSkipsWhileADeployIsActive(t *testing.T) {
	c, _ := newMaintenanceClient(t)
	h := stageAged(t, c.staging, []byte("chunk of an abandoned deploy"), 7*time.Hour)

	c.chunkActivity.touch()
	c.maintainIdleChunkStore(context.Background(), time.Now())
	if !c.staging.has(h) {
		t.Fatal("maintenance swept staging while a deploy was active")
	}

	c.maintainIdleChunkStore(context.Background(), time.Now().Add(chunkStoreIdleAfter+time.Minute))
	if c.staging.has(h) {
		t.Fatal("idle maintenance kept a chunk older than stagingRetention")
	}
}

func TestPruneChunkStoreRemovesStagingAndReconcilesTheIndex(t *testing.T) {
	kept := digest.FromString("layer containerd still holds")
	collected := digest.FromString("layer the prune let containerd collect")
	c, _ := newMaintenanceClient(t, kept)
	for i, blob := range []digest.Digest{kept, collected} {
		if err := c.chunkIndex.AddLayer(blob.String(), []chunk.Ref{{Hash: [32]byte{byte(i + 1)}, Len: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("chunk of a cancelled deploy")
	h := stageAged(t, c.staging, data, 5*time.Minute)

	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), false, &result)
	if result.StagedChunks != 1 || result.StagedBytes != uint64(len(data)) || result.StagingInUse || c.staging.has(h) {
		t.Fatalf("staging not pruned: %+v, staged chunk present %v", result, c.staging.has(h))
	}
	if result.ChunkIndexBlobsDropped != 1 {
		t.Fatalf("ChunkIndexBlobsDropped = %d, want 1", result.ChunkIndexBlobsDropped)
	}
	if _, ok := c.chunkIndex.Has([32]byte{2}); ok {
		t.Fatal("the entry of a blob containerd no longer holds survived the prune")
	}
	if _, ok := c.chunkIndex.Has([32]byte{1}); !ok {
		t.Fatal("the prune dropped the entry of a blob containerd still holds")
	}
}

func TestPruneChunkStoreDryRunCountsStagingAndChangesNothing(t *testing.T) {
	collected := digest.FromString("layer containerd no longer holds")
	c, _ := newMaintenanceClient(t)
	if err := c.chunkIndex.AddLayer(collected.String(), []chunk.Ref{{Hash: [32]byte{3}, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	data := []byte("chunk of a cancelled deploy")
	h := stageAged(t, c.staging, data, 5*time.Minute)

	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), true, &result)
	if result.StagedChunks != 1 || result.StagedBytes != uint64(len(data)) || !c.staging.has(h) {
		t.Fatalf("dry run: %+v, staged chunk present %v; want it counted and kept", result, c.staging.has(h))
	}
	if _, ok := c.chunkIndex.Has([32]byte{3}); !ok || result.ChunkIndexBlobsDropped != 0 {
		t.Fatalf("a dry run changed the index: entry present %v, dropped %d", ok, result.ChunkIndexBlobsDropped)
	}
}

// TestPruneChunkStoreLeavesStagingOfAnActiveDeploy: a chunk QueryChunks
// reported present must still be staged when the deploy assembles its layer,
// however young the chunk and however explicit the prune.
func TestPruneChunkStoreLeavesStagingOfAnActiveDeploy(t *testing.T) {
	c, _ := newMaintenanceClient(t)
	h := stageAged(t, c.staging, []byte("chunk a deploy is about to use"), time.Hour)

	c.chunkActivity.touch()
	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), false, &result)
	if !result.StagingInUse || result.StagedChunks != 0 || !c.staging.has(h) {
		t.Fatalf("a deploy's staging was pruned seconds after its last chunk RPC: %+v", result)
	}

	end := c.chunkActivity.begin()
	result = services.CachePruneResult{}
	c.pruneChunkStore(context.Background(), time.Now().Add(time.Hour), false, &result)
	end()
	if !result.StagingInUse || !c.staging.has(h) {
		t.Fatalf("staging was pruned while an assembly was in flight: %+v", result)
	}

	result = services.CachePruneResult{}
	c.pruneChunkStore(context.Background(), time.Now().Add(cachePruneStagingIdleAfter+time.Second), false, &result)
	if result.StagingInUse || result.StagedChunks != 1 || c.staging.has(h) {
		t.Fatalf("staging a quiet store holds was not pruned: %+v", result)
	}
}

// TestPruneChunkStoreLeavesStagingWhilePrepareImageWaits: a slow upload can
// go well over a minute between chunk RPCs, so what keeps a prune off the
// chunks a deploy already staged is PrepareImage's in-flight marker, held from
// its start until its last layer is assembled, not the per-chunk touch.
func TestPruneChunkStoreLeavesStagingWhilePrepareImageWaits(t *testing.T) {
	c, _ := newChunkStorePruneClient(t)
	h := stageAged(t, c.staging, []byte("chunk QueryChunks reported present"), time.Hour)
	uploading := sha256.Sum256([]byte("chunk the CLI is still uploading"))
	layer := digest.FromString("layer waiting for its chunks").String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.PrepareImage(ctx, "app:latest", []*agentpb.RunContainerLayerHeader{{
			DiffId: layer, Digest: layer, ChunkHashes: [][]byte{uploading[:]},
		}}, nil)
	}()
	for deadline := time.Now().Add(5 * time.Second); c.chunkActivity.inFlight.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}

	// An hour on, the last chunk RPC is long past: only the in-flight marker
	// can hold the prune off.
	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now().Add(time.Hour), false, &result)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareImage = %v, want it cancelled while waiting for its chunk", err)
	}
	if !result.StagingInUse || !c.staging.has(h) {
		t.Fatalf("a prune took staging while PrepareImage was waiting for chunks: %+v, staged chunk present %v", result, c.staging.has(h))
	}
}

// TestPruneChunkStoreDefersTheReconcileWhileADeployUsesTheStore: a deploy
// assembling a layer the prune's GC just collected commits the blob and
// indexes it again, and a reconcile that found the blob missing just before
// would drop those fresh entries. So a prune that finds the store in use
// leaves the index to idle maintenance or the next prune.
func TestPruneChunkStoreDefersTheReconcileWhileADeployUsesTheStore(t *testing.T) {
	c, _ := newMaintenanceClient(t)
	core, logs := observer.New(zap.InfoLevel)
	c.logger = zap.New(core)
	collected := digest.FromString("layer the prune let containerd collect")
	if err := c.chunkIndex.AddLayer(collected.String(), []chunk.Ref{{Hash: [32]byte{5}, Len: 1}}); err != nil {
		t.Fatal(err)
	}

	c.chunkActivity.touch()
	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), false, &result)
	if !result.StagingInUse || result.ChunkIndexBlobsDropped != 0 {
		t.Fatalf("result = %+v; want staging in use and no index blob dropped", result)
	}
	if _, ok := c.chunkIndex.Has([32]byte{5}); !ok {
		t.Fatal("the prune reconciled the index while a deploy was using the store")
	}
	deferred := logs.FilterMessage("Cache prune pruned the chunk store").FilterField(zap.Bool("reconcile_deferred", true))
	if deferred.Len() != 1 {
		t.Fatalf("the deferred reconcile was not logged: %v", logs.All())
	}
}

// TestPruneChunkStoreKeepsGoingWhenStagingCannotBeRemoved: the cache-root
// release before the chunk-store step already happened, so a staging failure
// is logged, not returned, and the index is still reconciled.
func TestPruneChunkStoreKeepsGoingWhenStagingCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, directory permissions do not deny deletes")
	}
	c, _ := newMaintenanceClient(t)
	core, logs := observer.New(zap.WarnLevel)
	c.logger = zap.New(core)
	if err := c.chunkIndex.AddLayer(digest.FromString("collected layer").String(), []chunk.Ref{{Hash: [32]byte{4}, Len: 1}}); err != nil {
		t.Fatal(err)
	}
	h := stageAged(t, c.staging, []byte("chunk in a read-only staging dir"), 5*time.Minute)
	if err := os.Chmod(c.staging.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(c.staging.dir, 0o700) })

	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), false, &result)
	if result.StagedChunks != 0 || !c.staging.has(h) {
		t.Fatalf("reported %d staged chunks removed from a read-only dir", result.StagedChunks)
	}
	if logs.FilterMessage("Pruning chunk staging failed").Len() != 1 {
		t.Fatalf("staging failure not logged once: %v", logs.All())
	}
	if result.ChunkIndexBlobsDropped != 1 {
		t.Fatalf("ChunkIndexBlobsDropped = %d, want 1: a staging failure must not skip the reconcile", result.ChunkIndexBlobsDropped)
	}
}

// TestPruneChunkStoreSkipsWhatABareClientLacks: PruneCache runs on Clients
// built without a staging area or an index (cache_prune_test.go builds them),
// and must not dereference either.
func TestPruneChunkStoreSkipsWhatABareClientLacks(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	var result services.CachePruneResult
	c.pruneChunkStore(context.Background(), time.Now(), false, &result)
	if result != (services.CachePruneResult{}) {
		t.Fatalf("result = %+v, want zero", result)
	}
}
