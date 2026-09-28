//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func TestInitialTuneClassifiesSubmissionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                string
		prepareErr, sendErr error
		short               bool
		packets             [][]byte
		fallback, success   bool
	}{
		{name: "monitor unsupported", prepareErr: unix.EAFNOSUPPORT, fallback: true},
		{name: "raw permission denied", prepareErr: unix.EPERM, fallback: true},
		{name: "before writable deadline", prepareErr: context.DeadlineExceeded, fallback: true},
		{name: "attempted write failed", sendErr: unix.EIO},
		{name: "short submitted write", short: true},
		{name: "monitor lost", packets: nil},
		{name: "command disallowed", packets: [][]byte{leConnectionUpdateCommand(0, meshIntervalUnits), statusPacket(0x0c)}},
		{name: "submitted unsupported", packets: [][]byte{leConnectionUpdateCommand(0, meshIntervalUnits), statusPacket(0x01)}},
		{name: "accepted timeout", packets: [][]byte{leConnectionUpdateCommand(0, meshIntervalUnits), statusPacket(0)}},
		{name: "completion rejected", packets: [][]byte{leConnectionUpdateCommand(0, meshIntervalUnits), statusPacket(0), completePacket(0, 12, 0, meshTimeoutUnits, 0x1f)}},
		{name: "success", packets: [][]byte{leConnectionUpdateCommand(0, meshIntervalUnits), statusPacket(0), completePacket(0, 12, 0, meshTimeoutUnits, 0)}, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent, closed := 0, 0
			interval, fallback, err := tuneMeshBeforeTLS(context.Background(), func(ctx context.Context) (time.Duration, error) {
				return submitMeshIntervalUpdate(ctx, 0, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) {
					if tc.prepareErr != nil {
						return nil, tc.prepareErr
					}
					return &leUpdateIO{send: func(command []byte) (int, error) {
						sent++
						if binary.LittleEndian.Uint16(command[4:6]) != 0 {
							t.Error("handle0 changed")
						}
						if tc.sendErr != nil {
							return 0, tc.sendErr
						}
						if tc.short {
							return len(command) - 1, nil
						}
						return len(command), nil
					}, read: eventReader(tc.packets...), close: func() { closed++ }}, nil
				})
			})
			if fallback != tc.fallback || (err == nil) != tc.success {
				t.Fatalf("interval=%s fallback=%v err=%v", interval, fallback, err)
			}
			if tc.prepareErr != nil {
				if sent != 0 || closed != 0 {
					t.Fatalf("unprepared IO used: sent=%d closed=%d", sent, closed)
				}
			} else if sent != 1 || closed != 1 {
				t.Fatalf("submitted IO lifecycle: sent=%d closed=%d", sent, closed)
			}
			if tc.success && interval != 15*time.Millisecond {
				t.Fatalf("interval=%s", interval)
			}
		})
	}
}

func TestInitialTuneDoesNotReturnBeforeCompletionAndClosesIO(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var closed atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, fallback, err := tuneMeshBeforeTLS(context.Background(), func(ctx context.Context) (time.Duration, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 4*time.Second {
				t.Error("missing initial4s bound")
			}
			packets := eventReader(leConnectionUpdateCommand(0x0eff, meshIntervalUnits), statusPacket(0), completePacket(0x0eff, 12, 0, meshTimeoutUnits, 0))
			return submitMeshIntervalUpdate(ctx, 0x0eff, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) {
				return &leUpdateIO{send: func(b []byte) (int, error) {
					if binary.LittleEndian.Uint16(b[4:6]) != 0x0eff {
						t.Error("maxhandle changed")
					}
					close(entered)
					return len(b), nil
				}, read: func(ctx context.Context) ([]byte, error) {
					select {
					case <-release:
						return packets(ctx)
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}, close: func() { closed.Store(true) }}, nil
			})
		})
		if fallback {
			t.Error("successful tuning fell back")
		}
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("TLS gate returned while update active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil || !closed.Load() {
			t.Fatalf("gate returned before IO closure: %v %v", err, closed.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("completion stuck")
	}
}

func TestInitialTuneParentCancellationOverridesFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var prepared bool
	_, fallback, err := tuneMeshBeforeTLS(ctx, func(ctx context.Context) (time.Duration, error) {
		return submitMeshIntervalUpdate(ctx, 0, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) { prepared = true; return nil, io.ErrClosedPipe })
	})
	if prepared || fallback || !errors.Is(err, context.Canceled) {
		t.Fatalf("prepared=%v fallback=%v error=%v", prepared, fallback, err)
	}
}

func TestInitialTuneCancellationAfterSubmissionClosesBeforeReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var closed atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, fallback, err := tuneMeshBeforeTLS(ctx, func(ctx context.Context) (time.Duration, error) {
			return submitMeshIntervalUpdate(ctx, 0, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) {
				return &leUpdateIO{send: func(b []byte) (int, error) { close(entered); return len(b), nil }, read: func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }, close: func() { closed.Store(true) }}, nil
			})
		})
		if fallback {
			t.Error("submitted cancellation fell back")
		}
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !closed.Load() {
			t.Fatalf("err=%v closed=%v", err, closed.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not join controller IO")
	}
}

func TestProviderStopReleasesListenerAndStalledInboundTLS(t *testing.T) {
	listener, path := unixPacketListener(t)
	r := &runtime{cfg: Config{Logger: zap.NewNop()}, serverTLS: &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop := make(chan error, 1)
	go func() { loop <- r.acceptLoop(ctx, listener) }()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Connect(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	// Peer intentionally sends no ClientHello, so the accepted TLS worker waits.
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-loop:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("accept loop survived stop")
	}
	joined := make(chan struct{})
	go func() { r.links.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("inbound TLS worker survived listener stop")
	}
}

func TestInitialTuneCancellationAfterPreparationNeverSubmits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent, closed := false, false
	_, fallback, err := tuneMeshBeforeTLS(ctx, func(ctx context.Context) (time.Duration, error) {
		return submitMeshIntervalUpdate(ctx, 0, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) {
			cancel()
			return &leUpdateIO{send: func(b []byte) (int, error) { sent = true; return len(b), nil }, read: eventReader(), close: func() { closed = true }}, nil
		})
	})
	if sent || !closed || fallback || !errors.Is(err, context.Canceled) {
		t.Fatalf("sent=%v closed=%v fallback=%v err=%v", sent, closed, fallback, err)
	}
}

func TestInitialTuneSubmittedUpdateHasBoundedDeadline(t *testing.T) {
	var closed bool
	started := time.Now()
	_, fallback, err := tuneMeshBeforeTLS(context.Background(), func(ctx context.Context) (time.Duration, error) {
		return submitMeshIntervalUpdate(ctx, 0, meshIntervalUnits, func(context.Context) (*leUpdateIO, error) {
			return &leUpdateIO{send: func(b []byte) (int, error) { return len(b), nil }, read: func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }, close: func() { closed = true }}, nil
		})
	})
	elapsed := time.Since(started)
	if fallback || !closed || !errors.Is(err, context.DeadlineExceeded) || elapsed < meshInitialTuneBudget || elapsed > meshInitialTuneBudget+time.Second {
		t.Fatalf("fallback=%v closed=%v elapsed=%s err=%v", fallback, closed, elapsed, err)
	}
}
