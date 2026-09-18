package containerd

import (
	"context"
	"testing"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	digest "github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

type fakeCacheContentStore struct {
	infos   []content.Info
	updates []content.Info
}

func (f *fakeCacheContentStore) Walk(_ context.Context, fn content.WalkFunc, _ ...string) error {
	for _, info := range f.infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeCacheContentStore) Update(_ context.Context, info content.Info, _ ...string) (content.Info, error) {
	f.updates = append(f.updates, info)
	return info, nil
}

type fakeCacheSnapshotter struct {
	infos   []snapshots.Info
	usage   map[string]snapshots.Usage
	updates []snapshots.Info
}

func (f *fakeCacheSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, _ ...string) error {
	for _, info := range f.infos {
		if err := fn(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeCacheSnapshotter) Update(_ context.Context, info snapshots.Info, _ ...string) (snapshots.Info, error) {
	f.updates = append(f.updates, info)
	return info, nil
}

func (f *fakeCacheSnapshotter) Usage(_ context.Context, key string) (snapshots.Usage, error) {
	return f.usage[key], nil
}

func TestPruneCacheRootsDryRunSelectsOnlyOldWendyCache(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-10 * time.Minute).Format(time.RFC3339)
	legacySnapshot := digest.FromString("legacy-snapshot").String()

	cs := &fakeCacheContentStore{infos: []content.Info{
		{Digest: digest.FromString("old"), Size: 100, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: old}},
		{Digest: digest.FromString("recent"), Size: 200, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: recent}},
		{Digest: digest.FromString("foreign"), Size: 300, Labels: map[string]string{labelKeyGCRoot: old}},
	}}
	sn := &fakeCacheSnapshotter{
		infos: []snapshots.Info{
			{Name: "wendy", Kind: snapshots.KindCommitted, Labels: map[string]string{labelKeyWendySnapshot: "true", labelKeyGCRoot: old}},
			{Name: legacySnapshot, Kind: snapshots.KindCommitted, Labels: map[string]string{labelKeyGCRoot: old}},
			{Name: "recent", Kind: snapshots.KindCommitted, Labels: map[string]string{labelKeyWendySnapshot: "true", labelKeyGCRoot: recent}},
			{Name: "foreign", Kind: snapshots.KindCommitted, Labels: map[string]string{labelKeyGCRoot: old}},
			{Name: "active", Kind: snapshots.KindActive, Labels: map[string]string{labelKeyWendySnapshot: "true", labelKeyGCRoot: old}},
		},
		usage: map[string]snapshots.Usage{"wendy": {Size: 400}, legacySnapshot: {Size: 500}},
	}

	got, err := pruneCacheRoots(context.Background(), cs, sn, now.Add(-time.Hour), true)
	if err != nil {
		t.Fatalf("pruneCacheRoots: %v", err)
	}
	if got.ContentBlobs != 1 || got.ContentBytes != 100 || got.Snapshots != 2 || got.SnapshotBytes != 900 {
		t.Fatalf("result = %+v", got)
	}
	if len(cs.updates) != 0 || len(sn.updates) != 0 {
		t.Fatalf("dry run mutated stores: content=%d snapshots=%d", len(cs.updates), len(sn.updates))
	}
}

func TestPruneCacheRootsRemovesOnlyGCRootLabel(t *testing.T) {
	old := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	cs := &fakeCacheContentStore{infos: []content.Info{{
		Digest: digest.FromString("layer"), Size: 12,
		Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: old, "keep": "content"},
	}}}
	sn := &fakeCacheSnapshotter{
		infos: []snapshots.Info{{
			Name: "snapshot", Kind: snapshots.KindCommitted,
			Labels: map[string]string{labelKeyWendySnapshot: "true", labelKeyGCRoot: old, "keep": "snapshot"},
		}},
		usage: map[string]snapshots.Usage{"snapshot": {Size: 34}},
	}

	_, err := pruneCacheRoots(context.Background(), cs, sn, time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatalf("pruneCacheRoots: %v", err)
	}
	if len(cs.updates) != 1 || cs.updates[0].Labels["keep"] != "content" {
		t.Fatalf("content update = %+v", cs.updates)
	}
	if _, ok := cs.updates[0].Labels[labelKeyGCRoot]; ok {
		t.Fatal("content GC root was not removed")
	}
	if len(sn.updates) != 1 || sn.updates[0].Labels["keep"] != "snapshot" {
		t.Fatalf("snapshot update = %+v", sn.updates)
	}
	if _, ok := sn.updates[0].Labels[labelKeyGCRoot]; ok {
		t.Fatal("snapshot GC root was not removed")
	}
}

func TestCacheRootOlderThanRejectsInvalidTimestamp(t *testing.T) {
	if cacheRootOlderThan("not-a-time", time.Now()) {
		t.Fatal("invalid timestamp must fail safe")
	}
}

func TestPruneCutoffDefaultsToGracePeriod(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	cutoff, effective := pruneCutoff(now, nil)

	if effective != cachePruneGracePeriod {
		t.Fatalf("effective = %v, want %v", effective, cachePruneGracePeriod)
	}
	want := now.Add(-cachePruneGracePeriod)
	if !cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", cutoff, want)
	}
}

func TestPruneCutoffZeroMinAgeSelectsEverything(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	zero := time.Duration(0)

	cutoff, effective := pruneCutoff(now, &zero)

	if effective != 0 {
		t.Fatalf("effective = %v, want 0", effective)
	}
	if !cutoff.Equal(now) {
		t.Fatalf("cutoff = %v, want %v", cutoff, now)
	}

	recent := now.Add(-10 * time.Minute).Format(time.RFC3339)
	cs := &fakeCacheContentStore{infos: []content.Info{
		{Digest: digest.FromString("recent"), Size: 10, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: recent}},
	}}
	sn := &fakeCacheSnapshotter{}

	got, err := pruneCacheRoots(context.Background(), cs, sn, cutoff, true)
	if err != nil {
		t.Fatalf("pruneCacheRoots: %v", err)
	}
	if got.ContentBlobs != 1 {
		t.Fatalf("ContentBlobs = %d, want 1 (a minutes-old entry must be selected at min age 0)", got.ContentBlobs)
	}

	negative := -time.Hour
	negCutoff, negEffective := pruneCutoff(now, &negative)
	if negEffective != 0 {
		t.Fatalf("negative min age effective = %v, want 0", negEffective)
	}
	if !negCutoff.Equal(now) {
		t.Fatalf("negative min age cutoff = %v, want %v", negCutoff, now)
	}
}

// fakeLeaseGCer records the lease it created and how it was asked to delete
// it, mirroring the subset of leases.Manager forceContainerdGC uses.
type fakeLeaseGCer struct {
	created    leases.Lease
	deletedID  string
	deleteOpts []leases.DeleteOpt
	createErr  error
	deleteErr  error
}

func (f *fakeLeaseGCer) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	if f.createErr != nil {
		return leases.Lease{}, f.createErr
	}
	var l leases.Lease
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return leases.Lease{}, err
		}
	}
	f.created = l
	return l, nil
}

func (f *fakeLeaseGCer) Delete(_ context.Context, l leases.Lease, opts ...leases.DeleteOpt) error {
	f.deletedID = l.ID
	f.deleteOpts = opts
	return f.deleteErr
}

func TestForceContainerdGCDeletesThrowawayLeaseSynchronously(t *testing.T) {
	fake := &fakeLeaseGCer{}

	if err := forceContainerdGC(context.Background(), fake); err != nil {
		t.Fatalf("forceContainerdGC: %v", err)
	}

	if fake.created.ID == "" {
		t.Fatal("no lease was created (WithRandomID did not run)")
	}
	if fake.deletedID != fake.created.ID {
		t.Fatalf("deleted lease ID = %q, want the created lease ID %q", fake.deletedID, fake.created.ID)
	}

	var opts leases.DeleteOptions
	for _, opt := range fake.deleteOpts {
		if err := opt(context.Background(), &opts); err != nil {
			t.Fatalf("applying delete opt: %v", err)
		}
	}
	if !opts.Synchronous {
		t.Fatal("lease was not deleted with SynchronousDelete")
	}
}

func TestReclaimedBetweenClampsNegative(t *testing.T) {
	if got := reclaimedBetween(100, 150, true); got == nil || *got != 50 {
		t.Fatalf("increase: got %v, want 50", got)
	}
	if got := reclaimedBetween(150, 100, true); got == nil || *got != 0 {
		t.Fatalf("decrease: got %v, want 0 (clamped)", got)
	}
	if got := reclaimedBetween(100, 150, false); got != nil {
		t.Fatalf("!ok: got %v, want nil", got)
	}
}

// fullContentStoreAdapter satisfies content.Store (which pruneCacheRoots's
// narrower cacheContentStore only needs Walk/Update from) by embedding the
// interface and delegating just those two methods to a fakeCacheContentStore,
// so the fake can drive a real *containerd.Client via client.WithContentStore.
type fullContentStoreAdapter struct {
	content.Store
	fake *fakeCacheContentStore
}

func (a *fullContentStoreAdapter) Walk(ctx context.Context, fn content.WalkFunc, filters ...string) error {
	return a.fake.Walk(ctx, fn, filters...)
}

func (a *fullContentStoreAdapter) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	return a.fake.Update(ctx, info, fieldpaths...)
}

// fullSnapshotterAdapter mirrors fullContentStoreAdapter for snapshots.Snapshotter.
type fullSnapshotterAdapter struct {
	snapshots.Snapshotter
	fake *fakeCacheSnapshotter
}

func (a *fullSnapshotterAdapter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return a.fake.Walk(ctx, fn, filters...)
}

func (a *fullSnapshotterAdapter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	return a.fake.Update(ctx, info, fieldpaths...)
}

func (a *fullSnapshotterAdapter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	return a.fake.Usage(ctx, key)
}

// newPruneCacheTestClient wires cs/sn into a real *containerd.Client with no
// gRPC dial (address ""), so (*Client).PruneCache exercises its actual
// c.client.ContentStore()/SnapshotService() calls against the fakes.
func newPruneCacheTestClient(t *testing.T, cs *fakeCacheContentStore, sn *fakeCacheSnapshotter) *Client {
	t.Helper()
	cd, err := containerdclient.New("",
		containerdclient.WithDefaultNamespace("default"),
		containerdclient.WithServices(
			containerdclient.WithContentStore(&fullContentStoreAdapter{fake: cs}),
			containerdclient.WithSnapshotters(map[string]snapshots.Snapshotter{"native": &fullSnapshotterAdapter{fake: sn}}),
		),
	)
	if err != nil {
		t.Fatalf("containerdclient.New: %v", err)
	}
	t.Cleanup(func() { _ = cd.Close() })
	return &Client{client: cd, logger: zap.NewNop(), snapshotter: "native"}
}

func TestClientPruneCacheDryRunLeavesReclaimedBytesNil(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	cs := &fakeCacheContentStore{infos: []content.Info{
		{Digest: digest.FromString("old"), Size: 100, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: old}},
	}}
	sn := &fakeCacheSnapshotter{}
	c := newPruneCacheTestClient(t, cs, sn)

	forceGCCalled := false
	c.forceGC = func(context.Context) error { forceGCCalled = true; return nil }
	// A dry run must never force GC or measure free space at all (not even
	// the "before" measurement) — both are wasted work when nothing will be
	// released, and the reclaimed-bytes computation is skipped entirely.
	c.freeBytes = func(string) (uint64, bool) { t.Fatal("freeBytes must not be consulted on a dry run"); return 0, false }

	zero := time.Duration(0)
	result, err := c.PruneCache(context.Background(), services.CachePruneOptions{DryRun: true, MinAge: &zero})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if result.ReclaimedBytes != nil {
		t.Fatalf("ReclaimedBytes = %v, want nil on a dry run", *result.ReclaimedBytes)
	}
	if result.MinimumAgeSeconds != 0 {
		t.Fatalf("MinimumAgeSeconds = %d, want 0", result.MinimumAgeSeconds)
	}
	if result.ContentBlobs != 1 {
		t.Fatalf("ContentBlobs = %d, want 1", result.ContentBlobs)
	}
	if forceGCCalled {
		t.Fatal("forceGC must not run on a dry run")
	}
	if len(cs.updates) != 0 {
		t.Fatal("dry run must not mutate the content store")
	}
}

func TestClientPruneCacheRealRunSetsReclaimedBytes(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	cs := &fakeCacheContentStore{infos: []content.Info{
		{Digest: digest.FromString("old"), Size: 100, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: old}},
	}}
	sn := &fakeCacheSnapshotter{}
	c := newPruneCacheTestClient(t, cs, sn)

	forceGCCalled := false
	c.forceGC = func(context.Context) error { forceGCCalled = true; return nil }
	calls := 0
	c.freeBytes = func(path string) (uint64, bool) {
		calls++
		if calls == 1 {
			return 1000, true
		}
		return 1400, true
	}

	// The entry is 2h old; ask for everything older than 1h so it is eligible
	// (the default 24h grace period would otherwise skip it).
	minAge := time.Hour
	result, err := c.PruneCache(context.Background(), services.CachePruneOptions{MinAge: &minAge})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if !forceGCCalled {
		t.Fatal("forceGC must run on a real (non-dry-run) prune")
	}
	if result.ReclaimedBytes == nil {
		t.Fatal("ReclaimedBytes = nil, want a measured value on a real run")
	}
	if *result.ReclaimedBytes != 400 {
		t.Fatalf("ReclaimedBytes = %d, want 400", *result.ReclaimedBytes)
	}
	if result.MinimumAgeSeconds != uint64(minAge/time.Second) {
		t.Fatalf("MinimumAgeSeconds = %d, want %d (echoes the effective age)", result.MinimumAgeSeconds, uint64(minAge/time.Second))
	}
	if len(cs.updates) != 1 {
		t.Fatalf("real run must release the pin: updates = %d, want 1", len(cs.updates))
	}
}

func TestClientPruneCacheReportsNilReclaimedBytesWhenGCFails(t *testing.T) {
	cs := &fakeCacheContentStore{}
	sn := &fakeCacheSnapshotter{}
	c := newPruneCacheTestClient(t, cs, sn)

	c.forceGC = func(context.Context) error { return context.DeadlineExceeded }
	c.freeBytes = func(string) (uint64, bool) { return 1000, true }

	result, err := c.PruneCache(context.Background(), services.CachePruneOptions{})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if result.ReclaimedBytes != nil {
		t.Fatalf("ReclaimedBytes = %v, want nil when forcing GC failed", *result.ReclaimedBytes)
	}
}

// TestClientPruneCacheDoesNotHoldLockDuringGC guards against a deploy or stop
// (CreateContainerWithProgress, StopContainer/stopOne, DeleteContainer — all
// of which also take c.mu) stalling for the whole forced-GC sweep. The forceGC
// seam runs synchronously in PruneCache's own goroutine, so if c.mu were still
// held at that point TryLock would report it busy without needing any actual
// concurrency.
func TestClientPruneCacheDoesNotHoldLockDuringGC(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	cs := &fakeCacheContentStore{infos: []content.Info{
		{Digest: digest.FromString("old"), Size: 100, Labels: map[string]string{labelKeyWendyLayer: "true", labelKeyGCRoot: old}},
	}}
	sn := &fakeCacheSnapshotter{}
	c := newPruneCacheTestClient(t, cs, sn)

	c.freeBytes = func(string) (uint64, bool) { return 1000, true }

	lockWasFree := false
	c.forceGC = func(context.Context) error {
		lockWasFree = c.mu.TryLock()
		if lockWasFree {
			c.mu.Unlock()
		}
		return nil
	}

	minAge := time.Hour
	if _, err := c.PruneCache(context.Background(), services.CachePruneOptions{MinAge: &minAge}); err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if !lockWasFree {
		t.Fatal("c.mu was still held while forceGC ran; the GC pass must run unlocked")
	}
}
