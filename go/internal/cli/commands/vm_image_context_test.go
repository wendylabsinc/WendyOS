package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type vmImageTestTransport func(*http.Request) (*http.Response, error)

func (f vmImageTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Model a response that sends a little data, then waits until its request is
// cancelled. release also lets a failing test unblock an uncancellable request.
type vmImageWaitingBody struct {
	ctx     context.Context
	started chan<- struct{}
	release <-chan struct{}
	closed  chan<- struct{}
	sent    bool
	once    sync.Once
}

func (b *vmImageWaitingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial image"), nil
	}
	b.once.Do(func() { b.started <- struct{}{} })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.release:
		return 0, io.ErrUnexpectedEOF
	}
}

func (b *vmImageWaitingBody) Close() error {
	b.closed <- struct{}{}
	return nil
}

func isolateVMImageCache(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(name, t.TempDir())
	}
	dir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertVMImageCacheEmpty(t *testing.T, dir string) {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("cancelled download left cache files: %v, %v", files, err)
	}
}

func TestDownloadImageContextAlreadyCancelledDoesNotStart(t *testing.T) {
	dir := isolateVMImageCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = vmImageTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("already-cancelled download started a request")
		return nil, errors.New("unexpected request")
	})
	info := &imageInfo{DownloadURL: "https://image.test/image"}
	if _, err := downloadImageContext(ctx, info); !errors.Is(err, context.Canceled) {
		t.Fatalf("progress download lost cancellation: %v", err)
	}
	if _, err := downloadImageIntoContext(ctx, info, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("download lost cancellation: %v", err)
	}
	if _, _, err := resolveVMImageContext(ctx, info); !errors.Is(err, context.Canceled) {
		t.Fatalf("image resolver lost cancellation: %v", err)
	}
	assertVMImageCacheEmpty(t, dir)
}

func TestDownloadImageContextCancelsInflightRequests(t *testing.T) {
	for _, phase := range []string{"head", "sequential", "parallel"} {
		t.Run(phase, func(t *testing.T) {
			dir := isolateVMImageCache(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{}, parallelDownloadWorkers)
			closed := make(chan struct{}, parallelDownloadWorkers)
			release := make(chan struct{})
			defer close(release)
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodHead {
					if phase == "head" {
						started <- struct{}{}
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-release:
							return nil, io.ErrUnexpectedEOF
						}
					}
					headers := make(http.Header)
					if phase == "parallel" {
						headers.Set("Accept-Ranges", "bytes")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: headers, ContentLength: 8192, Body: http.NoBody}, nil
				}
				status := http.StatusOK
				if phase == "parallel" {
					status = http.StatusPartialContent
				}
				return &http.Response{StatusCode: status, ContentLength: 1024, Body: &vmImageWaitingBody{
					ctx: req.Context(), started: started, release: release, closed: closed,
				}}, nil
			})
			done := make(chan error, 1)
			go func() {
				path, err := downloadImageIntoContext(ctx, &imageInfo{DownloadURL: "https://image.test/image"}, nil)
				if path != "" {
					err = errors.New("cancelled download returned a path")
				}
				done <- err
			}()
			workers := 1
			if phase == "parallel" {
				workers = parallelDownloadWorkers
			}
			for range workers {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("download did not reach the expected request")
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost download cancellation: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("download did not stop after cancellation")
			}
			if phase != "head" && len(closed) != workers {
				t.Fatalf("closed %d response bodies, want %d", len(closed), workers)
			}
			assertVMImageCacheEmpty(t, dir)
		})
	}
}

func TestDownloadImageContextCancelsWorkerOnProgressExit(t *testing.T) {
	for _, key := range []string{"q", "\x03", "parent"} {
		t.Run(key, func(t *testing.T) {
			dir := isolateVMImageCache(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, closed := make(chan struct{}, 1), make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodHead {
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: &vmImageWaitingBody{
					ctx: req.Context(), started: started, release: release, closed: closed,
				}}, nil
			})
			input, keys := io.Pipe()
			defer input.Close()
			defer keys.Close()
			done := make(chan error, 1)
			go func() {
				_, err := downloadImageContext(ctx, &imageInfo{DownloadURL: "https://image.test/image"},
					tea.WithInput(input), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignals())
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("download did not begin")
			}
			if key == "parent" {
				cancel()
			} else if _, err := io.WriteString(keys, key); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("progress exit lost cancellation: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("progress exit waited for an uncancelled download")
			}
			if len(closed) != 1 {
				t.Fatal("progress exit left response body open")
			}
			assertVMImageCacheEmpty(t, dir)
		})
	}
}

func TestVMImageCancelledCacheVerificationDoesNotDownload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := resolveVMImageWith(t.TempDir(), &imageInfo{Checksum: strings.Repeat("0", 64)},
		func(*imageInfo) (string, error) { t.Fatal("downloaded after cancelled verification"); return "", nil },
		func(path, digest string) error { return verifyVMImageContext(ctx, path, digest) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost verification cancellation: %v", err)
	}
}
