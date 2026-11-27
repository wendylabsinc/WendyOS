package services

import (
	"bytes"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"go.uber.org/zap"
)

// These tests exercise the actual private-patched ProvisioningService. The
// test-only strings are never parsed as certificates or used to authorize RPCs.
func worldConcurrentIdentity(t *testing.T) (*ProvisioningService, worldIdentity, uint64) {
	t.Helper()
	s := NewProvisioningService(zap.NewNop(), t.TempDir())
	s.mu.Lock()
	s.enrolled = true
	s.certPEM = "untrusted test old certificate"
	s.chainPEM = "untrusted test chain"
	s.keyPEM = []byte("untrusted test old key")
	err := s.saveState(&provisioningState{Enrolled: true, CertPEM: s.certPEM, ChainPEM: s.chainPEM})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s, worldIdentity{s}, s.worldIdentityToken.Load()
}

func worldWaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func worldWaitResult(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func worldHeldCallback(t *testing.T, i worldIdentity, token uint64, clock bool) (release func(), done <-chan error) {
	t.Helper()
	entered, unblock, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	release = func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	fn := i.WithCurrent
	if clock {
		fn = i.WithCurrentClock
	}
	go func() {
		result <- fn(token, func() error {
			if !i.Current(token) {
				return agentmodule.ErrDenied
			}
			close(entered)
			<-unblock
			if !i.Current(token) {
				return errors.New("identity changed inside its read fence")
			}
			return nil
		})
	}()
	worldWaitSignal(t, entered, "callback entry")
	return release, result
}

// A held reader excludes all writers. TryRLock stops succeeding precisely when
// the writer is queued, so this does not infer writer arrival from a sleep.
func worldWaitIdentityWriter(t *testing.T, s *ProvisioningService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.mu.TryRLock() {
		s.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("provisioning writer did not queue")
		}
		runtime.Gosched()
	}
}

func TestWorldClockIdentityOverlapPreservesDataSerialization(t *testing.T) {
	s, i, token := worldConcurrentIdentity(t)
	releaseData, dataDone := worldHeldCallback(t, i, token, false)
	releaseClock, clockDone := worldHeldCallback(t, i, token, true)
	// A second clock finishes while both the data callback and first clock are
	// held. This would deadlock with the old exclusive identity fence.
	quickClock := make(chan error, 1)
	go func() { quickClock <- i.WithCurrentClock(token, func() error { return nil }) }()
	if err := worldWaitResult(t, quickClock, "concurrent clock"); err != nil {
		t.Fatal(err)
	}
	if s.worldOperationMu.TryLock() {
		s.worldOperationMu.Unlock()
		t.Fatal("data callback lost its exclusive work gate")
	}
	secondEntered, secondDone := make(chan struct{}), make(chan error, 1)
	go func() {
		secondDone <- i.WithCurrent(token, func() error { close(secondEntered); return nil })
	}()
	// The clock round trip gives the waiting data goroutine an opportunity to
	// run; the work mutex remains the asserted exclusion mechanism.
	if err := i.WithCurrentClock(token, func() error {
		select {
		case <-secondEntered:
			return errors.New("ordinary callbacks overlapped")
		default:
			return nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	releaseData()
	if err := worldWaitResult(t, dataDone, "first data callback"); err != nil {
		t.Fatal(err)
	}
	worldWaitSignal(t, secondEntered, "second data callback with clock still held")
	if err := worldWaitResult(t, secondDone, "second data callback"); err != nil {
		t.Fatal(err)
	}
	releaseClock()
	if err := worldWaitResult(t, clockDone, "held clock callback"); err != nil {
		t.Fatal(err)
	}
}

func TestWorldClockIdentityActualMutationsWaitForAllReaders(t *testing.T) {
	for _, operation := range []string{"reset", "renew"} {
		t.Run(operation, func(t *testing.T) {
			s, i, token := worldConcurrentIdentity(t)
			oldKey := s.keyPEM
			oldInvalidated := s.worldIdentityInvalidated
			releaseData, dataDone := worldHeldCallback(t, i, token, false)
			releaseClock, clockDone := worldHeldCallback(t, i, token, true)
			mutated := make(chan error, 1)
			go func() {
				if operation == "reset" {
					_, err := s.unprovision("", "")
					mutated <- err
				} else {
					mutated <- s.commitRenewal("untrusted test old certificate", "untrusted test renewed certificate", "untrusted test renewed chain", "", []byte("untrusted test renewed key"))
				}
			}()
			worldWaitIdentityWriter(t, s)
			lateEntered := make(chan struct{}, 1)
			lateDone := make(chan error, 1)
			go func() {
				lateDone <- i.WithCurrentClock(token, func() error { lateEntered <- struct{}{}; return nil })
			}()
			releaseData()
			if err := worldWaitResult(t, dataDone, "data reader before mutation"); err != nil {
				t.Fatal(err)
			}
			// The remaining clock still pins the identity after data work ends.
			if !i.Current(token) {
				t.Fatal("mutation invalidated an active clock reader")
			}
			select {
			case err := <-mutated:
				t.Fatalf("mutation crossed a clock reader: %v", err)
			case <-oldInvalidated:
				t.Fatal("identity channel closed before readers returned")
			default:
			}
			releaseClock()
			if err := worldWaitResult(t, clockDone, "clock reader before mutation"); err != nil {
				t.Fatal(err)
			}
			if err := worldWaitResult(t, mutated, "actual provisioning mutation"); err != nil {
				t.Fatal(err)
			}
			if i.Current(token) || !bytes.Equal(oldKey, make([]byte, len(oldKey))) {
				t.Fatal("mutation did not invalidate the token and clear the old key")
			}
			worldWaitSignal(t, oldInvalidated, "synchronous invalidation signal")
			if err := worldWaitResult(t, lateDone, "queued old-token clock"); !errors.Is(err, agentmodule.ErrDenied) {
				t.Fatalf("queued old-token clock: %v", err)
			}
			select {
			case <-lateEntered:
				t.Fatal("queued reader entered after writer invalidation")
			default:
			}
			for _, fn := range []func(uint64, func() error) error{i.WithCurrent, i.WithCurrentClock} {
				if err := fn(token, func() error { t.Fatal("old token callback entered"); return nil }); !errors.Is(err, agentmodule.ErrDenied) {
					t.Fatalf("old token after mutation: %v", err)
				}
			}
		})
	}
}

func TestWorldClockIdentityDeniedStatesAndUnlock(t *testing.T) {
	for _, clock := range []bool{false, true} {
		name := "data"
		if clock {
			name = "clock"
		}
		t.Run(name, func(t *testing.T) {
			for _, state := range []string{"unenrolled", "cleanup", "recovery", "restart", "zero-token", "mismatched-token", "nil-callback"} {
				t.Run(state, func(t *testing.T) {
					s, i, token := worldConcurrentIdentity(t)
					fn := i.WithCurrent
					if clock {
						fn = i.WithCurrentClock
					}
					cb := func() error { t.Error("denied callback entered"); return nil }
					switch state {
					case "unenrolled":
						s.enrolled = false
					case "cleanup":
						s.resetCleanupErr = errors.New("test cleanup incomplete")
					case "recovery":
						s.resetRecoveryErr = errors.New("test recovery incomplete")
					case "restart":
						s.resetRestartPending = true
					case "zero-token":
						s.worldIdentityToken.Store(0)
						token = 0
					case "mismatched-token":
						token++
					case "nil-callback":
						cb = nil
					}
					if err := fn(token, cb); !errors.Is(err, agentmodule.ErrDenied) {
						t.Fatalf("denied state returned %v", err)
					}
					worldAssertIdentityUnlocked(t, s)
				})
			}
			s, i, token := worldConcurrentIdentity(t)
			fn := i.WithCurrent
			if clock {
				fn = i.WithCurrentClock
			}
			want := errors.New("callback failure")
			if err := fn(token, func() error { return want }); !errors.Is(err, want) {
				t.Fatalf("callback error changed: %v", err)
			}
			worldAssertIdentityUnlocked(t, s)
			func() {
				defer func() {
					if recover() != want {
						t.Error("callback panic changed or swallowed")
					}
				}()
				_ = fn(token, func() error { panic(want) })
			}()
			worldAssertIdentityUnlocked(t, s)
			if err := fn(token, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func worldAssertIdentityUnlocked(t *testing.T, s *ProvisioningService) {
	t.Helper()
	if !s.mu.TryLock() {
		t.Fatal("identity lock leaked")
	}
	s.mu.Unlock()
	if !s.worldOperationMu.TryLock() {
		t.Fatal("data work lock leaked")
	}
	s.worldOperationMu.Unlock()
}
