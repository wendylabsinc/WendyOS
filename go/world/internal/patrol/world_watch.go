package patrol

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

func (e *Engine) SetWorldWatch(p patrolwatch.Policy, run string, frame world.FrameRef) error {
	if e.State.WorldWatch != nil || e.State.State != "waiting" || e.goalNumber != 0 || e.planning != nil {
		return errors.New("World watch must be set once before patrol starts")
	}
	l, err := patrolwatch.New(p, run, frame)
	if err != nil {
		return err
	}
	e.State.WorldWatch = l
	return nil
}
func (e *Engine) worldWatchHold() {
	if e.Terminal() {
		return
	}
	if e.State.State != "world_held" {
		_ = e.adapter.Cancel()
	}
	e.State.State = "world_held"
	e.State.Reason = e.State.WorldWatch.Reason
	e.planning = nil
	e.settledAt = 0
	e.returnSettledMS = 0
	e.returnLastReceiptMS = 0
	e.returnFirstStamp = 0
	e.returnLastStamp = 0
}
func (e *Engine) ObserveWorld(records []world.Component, transport string, until, now time.Time) {
	if e.State.WorldWatch == nil || e.Terminal() {
		return
	}
	e.State.WorldWatch.Observe(records, transport, until, now)
	if !e.WorldMotionReady() {
		e.worldWatchHold()
	}
}
func (e *Engine) WorldMotionReady() bool {
	l := e.State.WorldWatch
	return l == nil || (!l.Permanent && l.Ready && !l.Held)
}
func (e *Engine) tickWorld(now int64) bool {
	if e.State.WorldWatch == nil {
		return false
	}
	e.State.WorldWatch.CheckTick(now)
	if !e.WorldMotionReady() {
		e.worldWatchHold()
		return true
	}
	return false
}
