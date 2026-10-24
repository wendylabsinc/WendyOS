package devices

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

type raceConnection struct {
	*fakeConnection
	closer *dialRaceCloser
}

func (c *raceConnection) Close() error { return c.closer.Close() }

var dialMTLSCandidateFn func(context.Context, *mtlsWalk, string, bool) (platform.DeviceConnection, error)

func raceWithStub(ctx context.Context, walk *mtlsWalk, candidates []string) (platform.DeviceConnection, error) {
	dial := dialMTLSCandidateFn
	return raceMTLSCandidatesWithDial(ctx, walk, candidates, func(w *mtlsWalk, c context.Context, a string, p bool) (platform.DeviceConnection, error) {
		return dial(c, w, a, p)
	})
}

type dialRaceCloser struct{ calls atomic.Int32 }

func (c *dialRaceCloser) Close() error {
	c.calls.Add(1)
	return nil
}

func newDialRaceConnection() (platform.DeviceConnection, *dialRaceCloser) {
	closer := new(dialRaceCloser)
	return &raceConnection{fakeConnection: workingConnection(), closer: closer}, closer
}

type dialRaceOutcome struct {
	conn platform.DeviceConnection
	err  error
}

func stubMTLSCandidate(t *testing.T, dial func(context.Context, *mtlsWalk, string, bool) (platform.DeviceConnection, error)) {
	t.Helper()
	original := dialMTLSCandidateFn
	dialMTLSCandidateFn = dial
	t.Cleanup(func() { dialMTLSCandidateFn = original })
}

func awaitDialRaceSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for candidate race")
	}
}

func awaitDialRaceOutcome(t *testing.T, results <-chan dialRaceOutcome) dialRaceOutcome {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("candidate race did not return")
		return dialRaceOutcome{}
	}
}

func TestMTLSCandidateRaceBackupWinsAndDrainsLateConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backupStarted := make(chan struct{})
	primaryCanceled := make(chan struct{})
	finishPrimary := make(chan struct{})
	defer func() {
		select {
		case <-finishPrimary:
		default:
			close(finishPrimary)
		}
	}()
	winner, winnerCloser := newDialRaceConnection()
	loser, loserCloser := newDialRaceConnection()
	stubMTLSCandidate(t, func(ctx context.Context, _ *mtlsWalk, _ string, isPrimary bool) (platform.DeviceConnection, error) {
		if isPrimary {
			<-ctx.Done()
			close(primaryCanceled)
			<-finishPrimary
			// A successful RPC can finish just as the winning candidate cancels us.
			return loser, nil
		}
		close(backupStarted)
		return winner, nil
	})
	results := make(chan dialRaceOutcome, 1)
	go func() {
		conn, err := raceWithStub(ctx, &mtlsWalk{}, []string{"primary", "backup"})
		results <- dialRaceOutcome{conn, err}
	}()
	awaitDialRaceSignal(t, backupStarted)
	awaitDialRaceSignal(t, primaryCanceled)
	select {
	case <-results:
		t.Fatal("race returned before its canceled primary finished")
	default:
	}
	close(finishPrimary)
	result := awaitDialRaceOutcome(t, results)
	if result.conn != winner || result.err != nil {
		t.Fatalf("race = (%v, %v), want authenticated backup", result.conn, result.err)
	}
	if loserCloser.calls.Load() != 1 || winnerCloser.calls.Load() != 0 {
		t.Fatalf("close counts: loser=%d winner=%d, want 1 and 0", loserCloser.calls.Load(), winnerCloser.calls.Load())
	}
	_ = winner.Close()
}

func TestMTLSCandidateRaceFailureStartsNextWithoutDelay(t *testing.T) {
	// The backup must finish before the stagger would have started it.
	ctx, cancel := context.WithTimeout(context.Background(), mtlsCandidateDelay/2)
	defer cancel()
	winner, _ := newDialRaceConnection()
	stubMTLSCandidate(t, func(ctx context.Context, _ *mtlsWalk, _ string, isPrimary bool) (platform.DeviceConnection, error) {
		if isPrimary {
			return nil, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return winner, nil
	})
	conn, err := raceWithStub(ctx, &mtlsWalk{}, []string{"primary", "backup"})
	if conn != winner || err != nil {
		t.Fatalf("race = (%v, %v), want backup before stagger delay", conn, err)
	}
	_ = winner.Close()
}

func dialRaceRefusals() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"identity", &IdentityRefusalError{Key: "pi.local", Cause: errors.New("wrong identity")}},
		{"public key", &IdentityRefusalError{Key: "pi.local", Cause: &PinMismatchError{Key: "asset:42", Want: "original", Got: "changed"}}},
	}
}

// Signal when the scheduler reads the refusal, so tests can order another
// candidate's completion without relying on sleeps or goroutine scheduling.
type observedDialRaceRefusal struct {
	err      error
	observed chan struct{}
	once     sync.Once
}

func (e *observedDialRaceRefusal) Error() string { return e.err.Error() }
func (e *observedDialRaceRefusal) Unwrap() error { return e.err }
func (e *observedDialRaceRefusal) Is(target error) bool {
	if target == ErrIdentityRefused {
		e.once.Do(func() { close(e.observed) })
	}
	return errors.Is(e.err, target)
}

func TestMTLSCandidateRaceAuthenticatedWinnerOverridesOtherAddressRefusal(t *testing.T) {
	for _, refusal := range dialRaceRefusals() {
		t.Run(refusal.name, func(t *testing.T) {
			for _, refusalFirst := range []bool{false, true} {
				t.Run(map[bool]string{false: "winner first", true: "refusal first"}[refusalFirst], func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					winner, closed := newDialRaceConnection()
					observed := &observedDialRaceRefusal{err: refusal.err, observed: make(chan struct{})}
					var thirdStarted atomic.Bool
					stubMTLSCandidate(t, func(ctx context.Context, _ *mtlsWalk, candidate string, isPrimary bool) (platform.DeviceConnection, error) {
						if candidate == "third" {
							thirdStarted.Store(true)
							return nil, nil
						}
						if refusalFirst {
							if !isPrimary {
								return nil, observed
							}
							select {
							case <-observed.observed:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
							if err := ctx.Err(); err != nil {
								return nil, err
							}
							return winner, nil
						}
						if isPrimary {
							<-ctx.Done()
							// Success canceled this attempt after its mismatch.
							return nil, refusal.err
						}
						return winner, nil
					})
					conn, err := raceWithStub(ctx, &mtlsWalk{}, []string{"primary", "backup", "third"})
					if conn != winner || err != nil {
						t.Fatalf("race = (%v, %v), want authenticated winner", conn, err)
					}
					if closed.calls.Load() != 0 {
						t.Fatalf("winner closed %d times before return, want zero", closed.calls.Load())
					}
					if thirdStarted.Load() {
						t.Fatal("race started another candidate after success or refusal")
					}
					_ = conn.Close()
				})
			}
		})
	}
}

func TestMTLSCandidateRaceCancellationPreservesRefusal(t *testing.T) {
	for _, refusal := range dialRaceRefusals() {
		t.Run(refusal.name, func(t *testing.T) {
			for _, order := range []string{"refusal before cancellation", "refusal after cancellation", "winner before cancellation"} {
				t.Run(order, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					conn, closed := newDialRaceConnection()
					backupStarted := make(chan struct{})
					observed := &observedDialRaceRefusal{err: refusal.err, observed: make(chan struct{})}
					stubMTLSCandidate(t, func(workerCtx context.Context, _ *mtlsWalk, _ string, isPrimary bool) (platform.DeviceConnection, error) {
						switch order {
						case "refusal before cancellation":
							if !isPrimary {
								return nil, observed
							}
							<-observed.observed
							cancel()
							<-workerCtx.Done()
							return conn, nil
						case "refusal after cancellation":
							if isPrimary {
								<-backupStarted
								cancel()
								<-workerCtx.Done()
								return nil, refusal.err
							}
							close(backupStarted)
							<-workerCtx.Done()
							return conn, nil
						default:
							if isPrimary {
								<-workerCtx.Done()
								// The backup won, but cancellation arrived while
								// the race was draining this observed refusal.
								cancel()
								return nil, refusal.err
							}
							return conn, nil
						}
					})
					results := make(chan dialRaceOutcome, 1)
					go func() {
						conn, err := raceWithStub(ctx, &mtlsWalk{}, []string{"primary", "backup"})
						results <- dialRaceOutcome{conn, err}
					}()
					result := awaitDialRaceOutcome(t, results)
					if result.conn != nil || !errors.Is(result.err, refusal.err) {
						t.Fatalf("race = (%v, %v), want observed refusal %v", result.conn, result.err, refusal.err)
					}
					if closed.calls.Load() != 1 {
						t.Fatalf("connection closed %d times, want once", closed.calls.Load())
					}
				})
			}
		})
	}
}

func TestMTLSCandidateRacePreservesPrimaryFailureEvidence(t *testing.T) {
	for _, primaryRejects := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary unreachable", true: "primary rejected certificate"}[primaryRejects], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			primaryErr := errors.New("primary failure")
			extraErr := errors.New("backup failure")
			backupDone := make(chan struct{})
			stubMTLSCandidate(t, func(ctx context.Context, worker *mtlsWalk, candidate string, isPrimary bool) (platform.DeviceConnection, error) {
				if isPrimary {
					worker.recordMTLSErr(candidate, primaryErr, true)
					worker.primaryObservedOrg = 7
					if primaryRejects {
						worker.primaryMTLSPortCertFails = 1
						worker.anyCertRejection = true
					} else {
						worker.primaryMTLSPortNonCertFails = 1
					}
					select {
					case <-backupDone:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				} else {
					worker.recordMTLSErr(candidate, extraErr, false)
					worker.observedDeviceOrg = 99
					worker.anyCertRejection = !primaryRejects
					close(backupDone)
				}
				return nil, nil
			})
			walk := &mtlsWalk{}
			conn, err := raceWithStub(ctx, walk, []string{"primary", "backup"})
			if conn != nil || err != nil {
				t.Fatalf("race = (%v, %v), want all candidates exhausted", conn, err)
			}
			if walk.primaryRejectedOurCert() != primaryRejects || walk.primaryObservedOrg != 7 || !errors.Is(walk.primaryLastErr, primaryErr) {
				t.Fatalf("backup changed primary fallback evidence: %+v", walk)
			}
			if len(walk.attempts) != 2 || walk.attempts[0].addr != "primary" || walk.attempts[1].addr != "backup" {
				t.Fatalf("attempts = %v, want candidate order", walk.attempts)
			}
		})
	}
}

func TestMTLSCandidateRaceParentCancellationDrainsAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backupStarted := make(chan struct{})
	var returned atomic.Int32
	late, closed := newDialRaceConnection()
	stubMTLSCandidate(t, func(ctx context.Context, _ *mtlsWalk, _ string, isPrimary bool) (platform.DeviceConnection, error) {
		if !isPrimary {
			close(backupStarted)
		}
		<-ctx.Done()
		returned.Add(1)
		if isPrimary {
			return nil, ctx.Err()
		}
		return late, nil
	})
	results := make(chan dialRaceOutcome, 1)
	go func() {
		conn, err := raceWithStub(ctx, &mtlsWalk{}, []string{"primary", "backup"})
		results <- dialRaceOutcome{conn, err}
	}()
	awaitDialRaceSignal(t, backupStarted)
	cancel()
	result := awaitDialRaceOutcome(t, results)
	if result.conn != nil || !errors.Is(result.err, context.Canceled) || returned.Load() != 2 {
		t.Fatalf("race = (%v, %v), returned workers=%d; want cancellation after both workers finish", result.conn, result.err, returned.Load())
	}
	if closed.calls.Load() != 1 {
		t.Fatalf("late connection closed %d times, want once", closed.calls.Load())
	}
}

func TestMTLSCandidateRaceCopiesCertificateOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	primaryChanged := make(chan struct{})
	stubMTLSCandidate(t, func(ctx context.Context, worker *mtlsWalk, _ string, isPrimary bool) (platform.DeviceConnection, error) {
		if isPrimary {
			worker.probeOrder[0] = 2
			close(primaryChanged)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-primaryChanged
		if !reflect.DeepEqual(worker.probeOrder, []int{0, 1, 2}) {
			t.Errorf("backup inherited a concurrently modified order: %v", worker.probeOrder)
		}
		conn, _ := newDialRaceConnection()
		return conn, nil
	})
	walk := &mtlsWalk{probeOrder: []int{0, 1, 2}}
	conn, err := raceWithStub(ctx, walk, []string{"primary", "backup"})
	if err != nil || conn == nil {
		t.Fatalf("race = (%v, %v), want backup success", conn, err)
	}
	_ = conn.Close()
}
