package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
)

type controlAdapter struct {
	blocked *atomic.Bool
	cancel  func() error
	goals   int
}

func (a *controlAdapter) Cancel() error {
	if a.cancel != nil {
		return a.cancel()
	}
	return nil
}
func (a *controlAdapter) Plan(p patrol.Point) (patrol.Route, error) {
	return patrol.Route{Reachable: true, Length: 1}, nil
}
func (a *controlAdapter) Goal(_ string, _ patrol.Point) error {
	if a.blocked.Load() {
		return patrol.ErrInterrupted
	}
	a.goals++
	return nil
}
func (a *controlAdapter) Capture() (patrol.Capture, error) { return patrol.Capture{}, nil }
func controlRig(t *testing.T) (*patrol.Engine, *controlFence, *controlAdapter) {
	t.Helper()
	f := &controlFence{blocked: new(atomic.Bool)}
	a := &controlAdapter{blocked: f.blocked}
	e, err := patrol.New("mission", 100000, []patrol.Room{{ID: "room", Polygon: [][2]float64{{0, 0}, {2, 0}, {2, 2}, {0, 2}}}}, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SetReturn(patrol.Point{X: 1, Y: 1}, 0); err != nil {
		t.Fatal(err)
	}
	return e, f, a
}
func waitControl(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("control did not reach barrier")
	}
}

func TestStopInterruptDuringRecallNeverRestoresMotion(t *testing.T) {
	e, f, a := controlRig(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	a.cancel = func() error { close(entered); <-release; return nil }
	recall := f.begin("recall")
	go func() { done <- applyControl(recall, f, e, 1000) }()
	waitControl(t, entered)
	// Stop arrives while recall cancellation is in flight. Its queued command
	// has not run when recall returns or when the next ticker dispatches.
	stop := f.begin("stop")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.cancel = nil
	if !f.blocked.Load() {
		t.Fatal("recall cleared Stop")
	}
	if err := e.Tick(1200, patrol.Navigation{Ready: true, Stamp: 1}); err != nil {
		t.Fatal(err)
	}
	if a.goals != 0 {
		t.Fatal("goal dispatched between Stop interrupt and queued Stop")
	}
	// Duplicate recall follows the failure path that previously cleared Stop.
	if err := applyControl(f.begin("recall"), f, e, 1300); err == nil {
		t.Fatal("duplicate recall accepted")
	}
	if !f.blocked.Load() {
		t.Fatal("failed recall cleared Stop")
	}
	if err := applyControl(stop, f, e, 1400); err != nil {
		t.Fatal(err)
	}
	if e.State.State != "stopped" || !f.blocked.Load() {
		t.Fatal("Stop did not remain terminal and blocked")
	}
}

func TestQueuedResumeCannotClearNewerOrPendingInterrupt(t *testing.T) {
	for _, action := range []string{"pause", "stop"} {
		for _, resumeFirst := range []bool{true, false} {
			t.Run(action+map[bool]string{true: "/older-resume", false: "/pending-interrupt"}[resumeFirst], func(t *testing.T) {
				e, f, _ := controlRig(t)
				if err := applyControl(f.begin("pause"), f, e, 1000); err != nil {
					t.Fatal(err)
				}
				var resume, interrupt command
				if resumeFirst {
					resume = f.begin("resume")
					interrupt = f.begin(action)
				} else {
					interrupt = f.begin(action)
					resume = f.begin("resume")
				}
				if err := applyControl(resume, f, e, 1100); !errors.Is(err, patrol.ErrInterrupted) {
					t.Fatalf("resume: %v", err)
				}
				if !f.blocked.Load() || e.State.State != "paused" {
					t.Fatal("queued resume removed interrupt")
				}
				if err := applyControl(interrupt, f, e, 1200); err != nil {
					t.Fatal(err)
				}
				retry := applyControl(f.begin("resume"), f, e, 1300)
				if action == "stop" {
					if !errors.Is(retry, patrol.ErrInterrupted) || !f.blocked.Load() {
						t.Fatal("new resume cleared latched Stop")
					}
				} else if retry != nil || f.blocked.Load() || e.State.State != "waiting" {
					t.Fatal("explicit retry after Pause did not resume", retry)
				}
			})
		}
	}
}

func TestStaleQueuedResumeAfterInterruptAlreadyProcessed(t *testing.T) {
	for _, action := range []string{"pause", "stop"} {
		t.Run(action, func(t *testing.T) {
			e, f, _ := controlRig(t)
			if err := applyControl(f.begin("pause"), f, e, 1000); err != nil {
				t.Fatal(err)
			}
			olderResume := f.begin("resume")
			if err := applyControl(f.begin(action), f, e, 1100); err != nil {
				t.Fatal(err)
			}
			if err := applyControl(olderResume, f, e, 1200); !errors.Is(err, patrol.ErrInterrupted) || !f.blocked.Load() {
				t.Fatal("older queued resume cleared a completed newer interrupt", err)
			}
		})
	}
}

func TestInterruptBetweenResumeChecksCannotClearBlock(t *testing.T) {
	_, f, _ := controlRig(t)
	pause := f.begin("pause")
	f.complete(pause)
	resume := f.begin("resume")
	if err := f.resume(resume, false); err != nil {
		t.Fatal(err)
	}
	stop := f.begin("stop")
	if err := f.resume(resume, true); !errors.Is(err, patrol.ErrInterrupted) || !f.blocked.Load() {
		t.Fatal("check/clear race lost Stop")
	}
	f.complete(stop)
}

func TestHTTPStopLatchesBeforeCancellationAndBusyQueue(t *testing.T) {
	_, f, _ := controlRig(t)
	q := make(chan command, 1)
	q <- f.begin("recall") // Keep the queue full without an executor consumer.
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := controlHandler("token", q, f, func() { close(entered); <-release })
	r := httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"stop"}`))
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	go func() { h(w, r); close(done) }()
	waitControl(t, entered)
	if !f.blocked.Load() {
		t.Fatal("Stop not blocked before cancellation")
	}
	resume := f.begin("resume")
	if err := f.resume(resume, true); !errors.Is(err, patrol.ErrInterrupted) {
		t.Fatal("pending Stop allowed resume")
	}
	close(release)
	waitControl(t, done)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	if f.pending != 0 {
		t.Fatal("failed enqueue leaked pending count")
	}
	if err := f.resume(f.begin("resume"), true); !errors.Is(err, patrol.ErrInterrupted) || !f.blocked.Load() {
		t.Fatal("busy queue erased Stop")
	}
}
