package networkmanager

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"
)

func found(name string) (string, error) { return name, nil }

func TestReloadSerializesBothManagersAndCancelsQueuedCaller(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var calls []string
	r := newReloader(found, func(ctx context.Context, path string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, path)
		n := len(calls)
		mu.Unlock()
		if n == 2 { // Hold nmcli: the gate must cover the complete manager pair.
			close(entered)
			<-release
		}
		return nil, nil
	})
	first := make(chan error, 1)
	go func() { first <- r.reload(context.Background()) }()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.reload(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	second := make(chan error, 1)
	go func() { second <- r.reload(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("concurrent reload passed held transaction: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if want := []string{"networkctl", "nmcli", "networkctl", "nmcli"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v; want %v", calls, want)
	}
}

func TestReloadRetriesOnlyNetworkdAlreadyReloading(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		wantCalls    int
	}{
		{"networkctl", "Failed to issue io.systemd.service.Reload() varlink call: io.systemd.Network.AlreadyReloading\n", 3},
		{"networkctl", "Access denied", 1},
		{"networkctl", "io.systemd.Network.AlreadyReloadingOther", 1},
		{"nmcli", "io.systemd.Network.AlreadyReloading", 1},
	} {
		t.Run(tc.name+tc.output, func(t *testing.T) {
			calls := 0
			failure := errors.New("exit status 1")
			r := newReloader(func(name string) (string, error) {
				if name != tc.name {
					return "", exec.ErrNotFound
				}
				return name, nil
			}, func(context.Context, string, ...string) ([]byte, error) {
				calls++
				if calls < 3 {
					return []byte(tc.output), failure
				}
				return nil, nil
			})
			r.retryDelay = time.Millisecond
			err := r.reload(context.Background())
			if calls != tc.wantCalls {
				t.Fatalf("calls %d; want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls == 3 {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("lost error: %v", err)
			}
		})
	}
}

func TestReloadBusyIsBoundedAndRetainsFailure(t *testing.T) {
	failure := errors.New("exit status 1")
	r := newReloader(found, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("io.systemd.Network.AlreadyReloading"), failure
	})
	r.retryDelay = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := r.reload(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, failure) {
		t.Fatalf("lost deadline/failure: %v", err)
	}
	// A canceled attempt releases the gate.
	r.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	if err := r.reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReloadQueuedDeadlineDoesNotRunCommand(t *testing.T) {
	r := newReloader(found, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ran command while gate held")
		return nil, nil
	})
	r.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.reload(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	<-r.gate
}

func TestReloadLookupFailureAndMissingManager(t *testing.T) {
	failure := errors.New("permission denied")
	r := newReloader(func(string) (string, error) { return "", failure }, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unexpected command")
		return nil, nil
	})
	if err := r.reload(context.Background()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	r.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if err := r.reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}
