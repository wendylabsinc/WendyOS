package tasking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

type liveResult struct {
	decision Decision
	err      error
}

func liveWait(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("live evaluation did not reach test gate")
	}
}
func liveDone(t *testing.T, done <-chan liveResult) liveResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("live evaluation did not finish")
	}
	return liveResult{}
}
func startLive(r *rig, ctx context.Context, id string, tele Telemetry) <-chan liveResult {
	done := make(chan liveResult, 1)
	go func() {
		d, err := r.s.EvaluateLiveContext(ctx, id, "app:nav", r.epoch, tele)
		done <- liveResult{d, err}
	}()
	return done
}
func assertLiveCanceled(t *testing.T, got liveResult) {
	t.Helper()
	if !errors.Is(got.err, context.Canceled) || got.decision.Action != mission.Stop || got.decision.Supervision != nil {
		t.Fatalf("canceled delivery returned permission: %+v %v", got.decision, got.err)
	}
}
func assertTaskFile(t *testing.T, r *rig, before []byte) {
	t.Helper()
	after, err := os.ReadFile(r.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("abandoned delivery changed durable task state or audit", err)
	}
}

// Signal after sampling the first nil cancellation result. The caller can now
// cancel while the store mutex is held, without timing-dependent sleeps.
type liveEntryContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *liveEntryContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.entered) })
	return err
}

func TestLiveEvaluationCanceledWhileWaitingTaskLock(t *testing.T) {
	r := setup(t)
	r.accept(t, "queued")
	tele := r.telemetry()
	before, _ := os.ReadFile(r.path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	r.s.mu.Lock()
	done := startLive(r, &liveEntryContext{Context: ctx, entered: entered}, "queued", tele)
	liveWait(t, entered)
	cancel()
	r.wall = r.wall.Add(time.Second)
	r.mono += time.Second
	r.s.mu.Unlock()
	assertLiveCanceled(t, liveDone(t, done))
	assertTaskFile(t, r, before)
	if d, err := r.s.EvaluateLiveContext(context.Background(), "queued", "app:nav", r.epoch, r.telemetry()); err != nil || d.Action != mission.Continue {
		t.Fatalf("fresh delivery inherited stale HOLD: %+v %v", d, err)
	}
}

func TestLiveEvaluationCancellationAfterBlockingValidation(t *testing.T) {
	for _, stage := range []string{"mission", "mission-denial", "supervisor", "inspection"} {
		t.Run(stage, func(t *testing.T) {
			var r *rig
			id := "blocked"
			var tele Telemetry
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			gate := func() { close(entered); <-release }
			switch stage {
			case "supervisor":
				sr := setupSupervision(t)
				r = sr.rig
				sr.acceptSupervised(t, id)
				sr.ack(t, sr.challenge(t, id, "operator-a", "live"))
				sr.verifyHook = gate
				tele = r.telemetry()
			case "inspection":
				var spec Spec
				r, spec, tele = inspectSetup(t)
				id = spec.ID
				r.s.cfg.ValidateInspection = func(Inspection) error { gate(); return nil }
			default:
				r = setup(t)
				r.accept(t, id)
				tele = r.telemetry()
				r.s.cfg.ValidateAuthority = func(Record) error { return nil }
				r.s.cfg.ValidateAuthorityContext = func(context.Context, Record) error {
					gate()
					if stage == "mission-denial" {
						return errors.New("denied after queue")
					}
					return nil
				}
			}
			before, _ := os.ReadFile(r.path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := startLive(r, ctx, id, tele)
			liveWait(t, entered)
			cancel()
			r.wall = r.wall.Add(time.Second)
			r.mono += time.Second
			unblock()
			assertLiveCanceled(t, liveDone(t, done))
			assertTaskFile(t, r, before)
		})
	}
}

func TestLiveEvaluationRetainsStartedCommitWithoutSecondTransition(t *testing.T) {
	for _, stage := range []string{"write", "post-write-authority", "post-write-denial", "successful-inspection-write"} {
		t.Run(stage, func(t *testing.T) {
			var r *rig
			id := "committed"
			var tele Telemetry
			want := mission.Running
			if stage == "successful-inspection-write" {
				var spec Spec
				r, spec, tele = inspectSetup(t)
				id, want = spec.ID, mission.Succeeded
			} else {
				r = setup(t)
				r.accept(t, id)
				tele = r.telemetry()
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			writes, checks := 0, 0
			r.s.beforeCommit = func() {
				writes++
				if stage == "write" || stage == "successful-inspection-write" {
					close(entered)
					<-release
				}
			}
			if stage == "post-write-authority" || stage == "post-write-denial" {
				r.s.cfg.ValidateAuthority = func(Record) error { return nil }
				r.s.cfg.ValidateAuthorityContext = func(context.Context, Record) error {
					checks++
					if checks == 2 {
						close(entered)
						<-release
						if stage == "post-write-denial" {
							return errors.New("denied after queue")
						}
					}
					return nil
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := startLive(r, ctx, id, tele)
			liveWait(t, entered)
			cancel()
			r.wall = r.wall.Add(time.Second)
			r.mono += time.Second
			unblock()
			got := liveDone(t, done)
			assertLiveCanceled(t, got)
			r.s.beforeCommit = nil
			stored, err := r.s.Get(id)
			if err != nil || stored.Status.Phase != want || got.decision.Record.Status != stored.Status || writes != 1 {
				t.Fatalf("commit lost or followed by stale mutation: %s %+v writes=%d err=%v", stage, stored.Status, writes, err)
			}
			var envelope struct {
				State json.RawMessage `json:"state"`
			}
			data, _ := os.ReadFile(r.path)
			if json.Unmarshal(data, &envelope) != nil {
				t.Fatal("invalid committed envelope")
			}
			var disk state
			if json.Unmarshal(envelope.State, &disk) != nil || disk.Records[id].Record.Status != stored.Status {
				t.Fatal("retained in-memory status differs from durable commit")
			}
			if want == mission.Succeeded && (stored.Result == nil || disk.Records[id].Record.Result == nil) {
				t.Fatal("committed inspection result lost")
			}
		})
	}
}

func TestLiveEvaluationStillEnforcesFreshnessAuthorityAndDeadline(t *testing.T) {
	for _, cause := range []string{"stale", "authority", "deadline"} {
		t.Run(cause, func(t *testing.T) {
			r := setup(t)
			r.accept(t, cause)
			tele := r.telemetry()
			want := mission.Failed
			switch cause {
			case "stale":
				tele.SampleUnixMS -= 1001
				want = mission.Held
			case "authority":
				r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
			case "deadline":
				r.wall = r.wall.Add(2 * time.Minute)
				r.mono += 2 * time.Minute
			}
			d, _ := r.s.EvaluateLiveContext(context.Background(), cause, "app:nav", r.epoch, tele)
			stored, err := r.s.Get(cause)
			if err != nil || d.Action == mission.Continue || stored.Status.Phase != want {
				t.Fatalf("live evaluation weakened %s: %+v %+v %v", cause, d, stored, err)
			}
		})
	}
}

func TestLiveCancellationDoesNotChangeStopCompletion(t *testing.T) {
	r := setup(t)
	r.accept(t, "stop")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := r.s.StopContext(ctx, "stop", r.auth())
	if err != nil || got.Status.Phase != mission.Canceled {
		t.Fatal("stop was abandoned", got, err)
	}
}
