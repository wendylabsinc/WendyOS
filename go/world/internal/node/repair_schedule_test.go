package node

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Observe the reserved bulk slot rather than assuming a goroutine has started
// after a sleep. No authority or I/O is involved in this test synchronization.
func awaitRepairSlot(t *testing.T, n *Node) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !n.repairIn.TryLock() {
			return
		}
		n.repairIn.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repair admission did not reserve its slot")
}

func TestRepairScheduleWaitsForControlInsteadOfImmediateRefusal(t *testing.T) {
	n := &Node{}
	n.startControl()
	active := true
	defer func() {
		if active {
			n.endControl()
		}
	}()
	done := make(chan bool, 1)
	response := httptest.NewRecorder()
	go func() {
		ok := n.admitRepair(response, httptest.NewRequest("POST", "/v1/repair/inventory", nil))
		if ok {
			n.repairIn.Unlock()
		}
		done <- ok
	}()
	awaitRepairSlot(t, n)
	select {
	case <-done:
		t.Fatal("repair refused immediately instead of retaining one bounded waiter")
	default:
	}
	second := httptest.NewRecorder()
	if n.admitRepair(second, httptest.NewRequest("POST", "/v1/repair/fetch", nil)) {
		n.repairIn.Unlock()
		t.Fatal("second bulk request entered while first waited")
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d", second.Code)
	}
	n.endControl()
	active = false
	select {
	case ok := <-done:
		if !ok || response.Code != http.StatusOK {
			t.Fatalf("bounded waiter did not continue after control completion: ok=%v status=%d", ok, response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("idle transition lost its wakeup")
	}
}

func TestRepairScheduleCancellationAndBudgetReleaseSlot(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "budget"} {
		t.Run(mode, func(t *testing.T) {
			n := &Node{}
			n.startControl()
			defer n.endControl()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer deadlineCancel()
			}
			response := httptest.NewRecorder()
			done := make(chan bool, 1)
			started := time.Now()
			go func() {
				done <- n.admitRepair(response, httptest.NewRequest("POST", "/v1/repair/inventory", nil).WithContext(ctx))
			}()
			awaitRepairSlot(t, n)
			if mode == "cancel" {
				cancel()
			}
			select {
			case admitted := <-done:
				if admitted || response.Code != http.StatusTooManyRequests {
					t.Fatal("interrupted waiter was admitted")
				}
			case <-time.After(time.Second):
				t.Fatal("wait exceeded its bounded budget")
			}
			if mode == "budget" && time.Since(started) < repairControlWait {
				t.Fatal("control wait reverted to immediate refusal")
			}
			if !n.repairIn.TryLock() {
				t.Fatal("interrupted admission leaked incoming bulk slot")
			}
			n.repairIn.Unlock()
		})
	}
}

func TestRepairScheduleWaitErrorsAndIdleCancellation(t *testing.T) {
	n := &Node{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.waitRepairTurn(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("idle fast path ignored cancellation", err)
	}
	if err := n.waitRepairTurn(context.Background()); err != nil {
		t.Fatal("idle node refused a turn", err)
	}
	n.startControl()
	defer n.endControl()
	if err := n.waitRepairTurn(context.Background()); !errors.Is(err, errRepairBusy) {
		t.Fatal("wait budget did not return the shared busy error", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := n.waitRepairTurn(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline not preserved", err)
	}
}

func TestRepairScheduleInitialMutexDelayCannotGrantExpiredIdleTurn(t *testing.T) {
	n := &Node{}
	n.controlMu.Lock()
	deadline := time.Now().Add(10 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- n.waitRepairTurnBefore(context.Background(), deadline) }()
	// The absolute deadline is supplied before launching the waiter, so this
	// assertion is independent of when its goroutine is first scheduled. The
	// node remains idle throughout: no active-control timer has been created.
	<-time.After(time.Until(deadline))
	n.controlMu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, errRepairBusy) {
			t.Fatal("initial lock delay granted an expired idle turn", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delayed idle waiter did not finish")
	}
}

func TestRepairScheduleBroadcastAfterLastControlAndNoLostWakeup(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		n := &Node{}
		n.startControl()
		n.startControl()
		var workers sync.WaitGroup
		results := make(chan error, 4)
		for i := 0; i < cap(results); i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				results <- n.waitRepairTurn(context.Background())
			}()
		}
		n.endControl()
		if n.controlActive.Load() != 1 {
			t.Fatal("overlapping control count lost")
		}
		select {
		case <-results:
			t.Fatal("one completion released waiters while another control remained")
		default:
		}
		// Closing the previous generation and starting a new one exercises both
		// sides of the capture/select race. Either idle instant is legitimate.
		n.endControl()
		n.startControl()
		n.endControl()
		workers.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal("broadcast waiter missed completion", err)
			}
		}
		if n.controlActive.Load() != 0 {
			t.Fatal("control count leaked")
		}
	}
}

func TestRepairScheduleControlHandlerNeverWaitsForBulkSlot(t *testing.T) {
	n := &Node{}
	n.repairIn.Lock()
	defer n.repairIn.Unlock()
	entered := make(chan struct{})
	wrapped := n.controlFirst(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.controlActive.Load() != 1 {
			t.Error("handler ran without control marker")
		}
		close(entered)
	}))
	done := make(chan struct{})
	go func() {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/tasks/stop", nil))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("control waited for bulk admission lock")
	}
	<-done
	if n.controlActive.Load() != 0 {
		t.Fatal("completed control handler left marker active")
	}
}
