package patrol

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
)

func (e *Engine) SetWork(p patrolwork.Policy, s patrolwork.Sample, now int64, runID, model string) error {
	if e.State.Work != nil || e.State.State != "waiting" || e.goalNumber != 0 || e.planning != nil {
		return errors.New("work allowance must be set once before patrol starts")
	}
	if err := p.Validate(e.State.Return != nil); err != nil {
		return err
	}
	ledger, err := patrolwork.New(p, s, now, runID, model)
	if err != nil {
		return err
	}
	e.State.Work = ledger
	return nil
}

// ObserveWork never dispatches or changes the return destination. It may run at
// the final motion fence after a route request, without reentrant goal planning.
func (e *Engine) ObserveWork(s *patrolwork.Sample, now int64) {
	w := e.State.Work
	if w == nil || e.Terminal() {
		return
	}
	w.Update(s, now)
	if !w.Valid {
		if e.State.State != "work_held" {
			_ = e.adapter.Cancel()
		}
		e.State.State = "work_held"
		e.State.Reason = w.Reason
		e.planning = nil
		e.settledAt = 0
		e.returnSettledMS = 0
		e.returnLastReceiptMS = 0
		e.returnFirstStamp = 0
		e.returnLastStamp = 0
	}
}
func (e *Engine) WorkMotionReady() bool {
	w := e.State.Work
	return w == nil || (w.Valid && e.State.State != "work_held" && (!w.ReserveReached() || (e.State.Return != nil && e.State.Return.Requested)))
}
func (e *Engine) tickWork(now int64) (bool, error) {
	w := e.State.Work
	if w == nil {
		return false, nil
	}
	last := w.Last
	e.ObserveWork(&last, now)
	if !w.Valid || e.State.State == "work_held" {
		return true, nil
	}
	if w.ReserveReached() && !e.State.Return.Requested {
		return false, e.recall(now, "work_reserve")
	}
	return false, nil
}
