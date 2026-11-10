package backend

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

func TestGalleryConcurrentReadsShareDial(t *testing.T) {
	b := New()
	defer b.galleryReset(true)
	var dials atomic.Int32
	started, proceed := make(chan struct{}), make(chan struct{})
	target := &grpcclient.AgentConnection{}
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		dials.Add(1)
		close(started)
		<-proceed
		return target, nil, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, release, err := b.galleryAcquire(context.Background(), "ser9")
			if err != nil {
				t.Error(err)
				return
			}
			if c != target {
				t.Error("different connection")
			}
			release(nil)
		}()
	}
	<-started
	close(proceed)
	wg.Wait()
	if dials.Load() != 1 {
		t.Fatalf("dialed %d times", dials.Load())
	}
	c, release, err := b.galleryAcquire(context.Background(), "ser9")
	if err != nil || c != target {
		t.Fatalf("reuse: %v", err)
	}
	release(nil)
	if dials.Load() != 1 {
		t.Fatal("sequential request redialed")
	}
}
func TestGalleryScopeResetCancelsDialAndBlocksReuse(t *testing.T) {
	b := New()
	started := make(chan struct{})
	b.connectTarget = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, liteConnection, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { _, _, err := b.galleryAcquire(context.Background(), "ser9"); done <- err }()
	<-started
	b.galleryReset(true)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reset accepted stale dial")
		}
	case <-time.After(time.Second):
		t.Fatal("dial not cancelled")
	}
	if _, _, err := b.galleryAcquire(context.Background(), "ser9"); err == nil {
		t.Fatal("inactive pool allowed request")
	}
}
func TestGalleryFailureRedialsAndKeepsDevicesSeparate(t *testing.T) {
	b := New()
	defer b.galleryReset(true)
	dials := 0
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		dials++
		return &grpcclient.AgentConnection{}, nil, nil
	}
	first, release, err := b.galleryAcquire(context.Background(), "ser9")
	if err != nil {
		t.Fatal(err)
	}
	release(errors.New("disconnected"))
	second, release, err := b.galleryAcquire(context.Background(), "ser9")
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	third, release, err := b.galleryAcquire(context.Background(), "andrea")
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	if first == second || second == third || dials != 3 {
		t.Fatal("failed connection or different device was reused")
	}
	b.galleryReset(true)
	b.gallery.mu.Lock()
	b.gallery.blocked = false
	b.gallery.mu.Unlock()
	fourth, release, err := b.galleryAcquire(context.Background(), "ser9")
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	if fourth == second || dials != 4 {
		t.Fatal("old account connection reused")
	}
}

func TestGalleryCapacityWaitsAndHonorsCancellation(t *testing.T) {
	b := New()
	defer b.galleryReset(true)
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		return &grpcclient.AgentConnection{}, nil, nil
	}
	var releases []func(error)
	for _, key := range []string{"a", "b", "c", "d"} {
		_, r, e := b.galleryAcquire(context.Background(), key)
		if e != nil {
			t.Fatal(e)
		}
		releases = append(releases, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := b.galleryAcquire(ctx, "e"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	releases[0](nil)
	_, release, err := b.galleryAcquire(context.Background(), "e")
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	for _, r := range releases[1:] {
		r(nil)
	}
}

type galleryReuseFixture struct{ *visualFixtureClient }

func (c *galleryReuseFixture) Episodes(context.Context, *pb.DataEpisodesRequest, ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	return &pb.DataEpisodesResponse{}, nil
}
func TestGalleryIndexPreviewPlaybackShareConnection(t *testing.T) {
	b := New()
	defer b.galleryReset(true)
	dials := 0
	client := &galleryReuseFixture{&visualFixtureClient{manifest: data.Manifest{ID: "episode", State: "complete", CanonicalClock: "CLOCK_BOOTTIME", BootID: "boot"}}}
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		dials++
		return &grpcclient.AgentConnection{DataService: client}, nil, nil
	}
	if _, err := b.VisualEpisodesAt("ser9"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.VisualPreviewAt("ser9", "episode"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.VisualEpisodeAt("ser9", "episode"); err != nil {
		t.Fatal(err)
	}
	if dials != 1 {
		t.Fatalf("expected one dial across list, preview and playback, got %d", dials)
	}
}
