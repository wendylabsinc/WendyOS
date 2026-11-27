package patrol

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func (e *Engine) SetEnvironment(p patrolenvironment.Policy, run, model, boot string, frame world.FrameRef) error {
	if e.State.Environment != nil || e.State.State != "waiting" || e.goalNumber != 0 || e.planning != nil {
		return errors.New("environment policy must be set once before patrol starts")
	}
	ledger, err := patrolenvironment.New(p, run, model, boot, frame)
	if err != nil {
		return err
	}
	e.State.Environment = ledger
	return nil
}
func (e *Engine) environmentHold() {
	if e.Terminal() {
		return
	}
	if e.State.State != "environment_held" {
		_ = e.adapter.Cancel()
	}
	e.State.State = "environment_held"
	e.State.Reason = e.State.Environment.Reason
	e.planning = nil
	e.settledAt = 0
	e.returnSettledMS = 0
	e.returnLastReceiptMS = 0
	e.returnFirstStamp = 0
	e.returnLastStamp = 0
}
func (e *Engine) ObserveEnvironment(samples []patrolenvironment.Sample, now int64) {
	if e.State.Environment == nil || e.Terminal() {
		return
	}
	e.State.Environment.Update(samples, now)
	if !e.EnvironmentMotionReady() {
		e.environmentHold()
	}
}
func (e *Engine) EnvironmentMotionReady() bool {
	l := e.State.Environment
	return l == nil || (!l.Permanent && l.Ready && !l.Held)
}
func (e *Engine) tickEnvironment(now int64) bool {
	if e.State.Environment == nil {
		return false
	}
	e.State.Environment.Check(now)
	if !e.EnvironmentMotionReady() {
		e.environmentHold()
		return true
	}
	return false
}
