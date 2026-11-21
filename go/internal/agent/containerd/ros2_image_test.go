package containerd

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"go.uber.org/zap"
)

type ros2PreparedTestImage struct{ containerd.Image }

func TestROS2ImagePreparationSurvivesRequestCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var preparation ros2ImagePreparation
		defer preparation.close()
		ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), "wendy-test"), time.Second)
		defer cancel()
		release := make(chan struct{})
		calls := 0
		var setupCtx context.Context
		want := &ros2PreparedTestImage{}
		prepare := func(ctx context.Context) (containerd.Image, error) {
			calls++
			setupCtx = ctx
			<-release
			return want, ctx.Err()
		}
		first := make(chan error, 1)
		go func() { _, err := preparation.wait(ctx, prepare); first <- err }()
		synctest.Wait()
		time.Sleep(time.Second)
		if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request error = %v", err)
		}
		if setupCtx.Err() != nil {
			t.Fatalf("request canceled image extraction: %v", setupCtx.Err())
		}
		if ns, _ := namespaces.Namespace(setupCtx); ns != "wendy-test" {
			t.Fatalf("lost containerd namespace: %q", ns)
		}
		second := make(chan containerd.Image, 1)
		go func() {
			image, err := preparation.wait(context.Background(), prepare)
			if err != nil {
				t.Errorf("retry: %v", err)
			}
			second <- image
		}()
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("retry started %d preparations", calls)
		}
		close(release)
		if image := <-second; image != want {
			t.Fatalf("retry did not receive prepared image: %v", image)
		}
	})
}

func TestROS2ImagePreparationRetriesFailure(t *testing.T) {
	var preparation ros2ImagePreparation
	defer preparation.close()
	wantErr := errors.New("registry unavailable")
	_, err := preparation.wait(context.Background(), func(context.Context) (containerd.Image, error) { return nil, wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("first attempt = %v", err)
	}
	want := &ros2PreparedTestImage{}
	image, err := preparation.wait(context.Background(), func(context.Context) (containerd.Image, error) { return want, nil })
	if err != nil || image != want {
		t.Fatalf("retry = %v, %v", image, err)
	}
}

func TestROS2ImagePreparationConcurrentWaitersCancelIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var preparation ros2ImagePreparation
		defer preparation.close()
		release := make(chan struct{})
		calls := 0
		want := &ros2PreparedTestImage{}
		prepare := func(ctx context.Context) (containerd.Image, error) {
			calls++
			select {
			case <-release:
				return want, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled := make(chan error, 1)
		go func() { _, err := preparation.wait(ctx, prepare); canceled <- err }()
		synctest.Wait()
		const waiters = 8
		results := make(chan containerd.Image, waiters)
		for range waiters {
			go func() {
				image, err := preparation.wait(context.Background(), prepare)
				if err != nil {
					t.Errorf("concurrent waiter: %v", err)
				}
				results <- image
			}()
		}
		synctest.Wait()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter: %v", err)
		}
		close(release)
		for range waiters {
			if image := <-results; image != want {
				t.Fatalf("waiter lost the shared image: %v", image)
			}
		}
		if calls != 1 {
			t.Fatalf("concurrent callers started %d preparations, want 1", calls)
		}
	})
}

func TestROS2ClientCloseCancelsImagePreparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newROS2ContainerTestClient(t, nil, nil)
		result := make(chan error, 1)
		go func() {
			_, err := c.ros2ImagePreparation.wait(context.Background(), func(ctx context.Context) (containerd.Image, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			result <- err
		}()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("client close left preparation running: %v", err)
		}
	})
}

func TestROS2ImagePreparationTimeoutAndClose(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "timeout"
		if shutdown {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var preparation ros2ImagePreparation
				defer preparation.close()
				result := make(chan error, 1)
				go func() {
					_, err := preparation.wait(context.Background(), func(ctx context.Context) (containerd.Image, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					})
					result <- err
				}()
				synctest.Wait()
				wantErr := context.DeadlineExceeded
				if shutdown {
					preparation.close()
					wantErr = context.Canceled
				} else {
					time.Sleep(ros2ImagePreparationTimeout)
				}
				if err := <-result; !errors.Is(err, wantErr) {
					t.Fatalf("preparation ended with %v, want %v", err, wantErr)
				}
			})
		})
	}
}

func TestROS2ImagePreparationDoesNotStartAfterCancellationOrClose(t *testing.T) {
	var preparation ros2ImagePreparation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prepare := func(context.Context) (containerd.Image, error) {
		t.Error("started image preparation")
		return nil, nil
	}
	if _, err := preparation.wait(ctx, prepare); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
	preparation.close()
	if _, err := preparation.wait(context.Background(), prepare); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed preparation: %v", err)
	}
}

type ros2BlockingImageStore struct {
	images.Store
	release chan struct{}
	calls   int
	err     error
}

func (s *ros2BlockingImageStore) Get(ctx context.Context, _ string) (images.Image, error) {
	s.calls++
	select {
	case <-s.release:
		return images.Image{}, s.err
	case <-ctx.Done():
		return images.Image{}, ctx.Err()
	}
}

func TestROS2StandalonePreparationReleasesContainerLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &ros2BlockingImageStore{release: make(chan struct{}), err: errors.New("image store unavailable")}
		client, err := containerd.New("", containerd.WithServices(
			containerd.WithContainerStore(ros2TestContainerStore{}),
			containerd.WithImageStore(store),
		))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		c := &Client{client: client, namespace: "wendy", logger: zap.NewNop()}
		defer c.ros2ImagePreparation.close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := make(chan error, 1)
		go func() {
			_, err := c.ensureStandaloneROS2Sidecar(ctx, ros2HostSidecarName, labelKeyROS2HostSidecar, 0, nil)
			first <- err
		}()
		synctest.Wait()
		if !c.mu.TryLock() {
			t.Fatal("image preparation holds the container lock")
		}
		c.mu.Unlock()
		cancel()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled host request: %v", err)
		}
		second := make(chan error, 1)
		go func() {
			_, err := c.ensureStandaloneROS2Sidecar(context.Background(), ros2SystemSidecarName, labelKeyROS2SystemSidecar, 0, nil)
			second <- err
		}()
		synctest.Wait()
		if store.calls != 1 {
			t.Fatalf("system inspector did not share host preparation: %d calls", store.calls)
		}
		close(store.release)
		if err := <-second; !errors.Is(err, store.err) {
			t.Fatalf("system request: %v", err)
		}
	})
}
