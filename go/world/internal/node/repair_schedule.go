package node

import (
	"context"
	"net/http"
	"time"
)

const repairControlWait = 100 * time.Millisecond

// controlFirst marks control work before authentication so a bulk operation
// does not repeatedly consume its admission opportunity while control waits.
// It cannot preempt already running I/O and is not a per-principal quota.
func (n *Node) controlFirst(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tasks/stop", "/v1/tasks/evaluate", "/v1/tasks/supervision/challenge", "/v1/tasks/supervision/ack":
			n.startControl()
			defer n.endControl()
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Node) startControl() {
	n.controlMu.Lock()
	if n.controlActive.Load() == 0 {
		n.controlIdle = make(chan struct{})
	}
	n.controlActive.Add(1)
	n.controlMu.Unlock()
}

func (n *Node) endControl() {
	n.controlMu.Lock()
	if n.controlActive.Add(-1) == 0 {
		close(n.controlIdle)
		n.controlIdle = nil
	}
	n.controlMu.Unlock()
}

// waitRepairTurn observes an idle instant, not an exclusive reservation against
// future control arrivals. All wakeups share one fixed budget; a new control
// request after a wakeup cannot restart the timer. The short mutex protects only
// the count/channel transition, never authority, network or persistence work.
func (n *Node) waitRepairTurn(ctx context.Context) error {
	return n.waitRepairTurnBefore(ctx, time.Now().Add(repairControlWait))
}

// The explicit deadline also lets tests exercise a delayed first mutex
// acquisition without guessing whether a goroutine reached a particular line.
func (n *Node) waitRepairTurnBefore(ctx context.Context, deadline time.Time) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n.controlMu.Lock()
		idle := n.controlActive.Load() == 0
		changed := n.controlIdle
		n.controlMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return errRepairBusy
		}
		if idle {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(time.Until(deadline))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errRepairBusy
		case <-changed:
			// The last control request completed. Recheck under controlMu:
			// another request may already have created the next idle channel.
		}
	}
}

// Reserve the only incoming bulk slot before waiting. Other bulk requests are
// refused immediately rather than accumulating one waiter per connection.
func (n *Node) admitRepair(w http.ResponseWriter, r *http.Request) bool {
	if !n.repairIn.TryLock() {
		http.Error(w, "bulk repair is busy", http.StatusTooManyRequests)
		return false
	}
	if err := n.waitRepairTurn(r.Context()); err != nil {
		n.repairIn.Unlock()
		http.Error(w, "bulk repair interrupted or control request pending", http.StatusTooManyRequests)
		return false
	}
	return true
}
