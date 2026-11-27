package patrol

import (
	"errors"
	"math"
)

// ReturnState retains the admitted point and original wall-clock return window.
// It does not create motion authority or extend the enclosing mission.
type ReturnState struct {
	Destination         Point  `json:"destination"`
	BeforeExpirySeconds int    `json:"before_expiry_seconds"`
	BeginAtMS           int64  `json:"begin_at_unix_ms,omitempty"`
	Requested           bool   `json:"requested"`
	RequestedAtMS       int64  `json:"requested_at_unix_ms,omitempty"`
	Trigger             string `json:"trigger,omitempty"`
	ArrivedAtMS         int64  `json:"arrived_at_unix_ms,omitempty"`
}

func (e *Engine) SetReturn(p Point, beforeSeconds int) error {
	if e.State.Return != nil || e.State.State != "waiting" || e.goalNumber != 0 || e.planning != nil || !finite(p.X) || !finite(p.Y) || !finite(p.Yaw) || math.Abs(p.Yaw) > math.Pi || beforeSeconds < 0 || beforeSeconds > 86400 {
		return errors.New("return policy must be set once before patrol starts")
	}
	r := &ReturnState{Destination: p, BeforeExpirySeconds: beforeSeconds}
	if beforeSeconds > 0 {
		r.BeginAtMS = e.State.ExpiresAtMS - int64(beforeSeconds)*1000
	}
	e.State.Return = r
	return nil
}
func (e *Engine) recall(now int64, trigger string) error {
	r := e.State.Return
	if r == nil {
		return errors.New("this mission has no approved rendezvous")
	}
	if r.Requested {
		return errors.New("return already requested; resume a held return explicitly")
	}
	if err := e.adapter.Cancel(); err != nil {
		return e.fail(err)
	}
	if e.State.Tour != nil && !e.TourComplete() {
		e.State.Tour.Outcome = "incomplete"
	}
	r.Requested = true
	r.RequestedAtMS = now
	r.Trigger = trigger
	e.planning = nil
	e.State.CurrentRoom = ""
	e.State.Destination = &r.Destination
	e.settledAt = 0
	e.returnLastReceiptMS = 0
	e.returnSettledMS = 0
	e.returnFirstStamp = 0
	e.returnLastStamp = 0
	if e.State.State != "paused" {
		e.State.State = "return_planning"
	}
	e.State.Reason = "Returning to the signed rendezvous; remaining observations are canceled"
	return nil
}
func (e *Engine) holdReturn(reason string) error {
	e.State.State = "return_held"
	e.State.Reason = reason
	e.returnLastReceiptMS = 0
	e.returnSettledMS = 0
	e.returnFirstStamp = 0
	e.returnLastStamp = 0
	return e.adapter.Cancel()
}
func (e *Engine) tickReturn(now int64, n Navigation) error {
	if e.State.State == "return_held" {
		return nil
	}
	if !n.Ready || !finite(n.SensorAge) || n.SensorAge < 0 || n.SensorAge > 1 || !finite(n.Pose.X) || !finite(n.Pose.Y) || !finite(n.Pose.Yaw) || !finite(n.Speed) || !finite(n.AngularSpeed) || !finite(n.Stamp) || n.Stamp <= 0 {
		return e.holdReturn("Return held: fresh localization, LiDAR and odometry required. Resume after recovery.")
	}
	if e.State.State == "return_planning" {
		route, err := e.adapter.Plan(e.State.Return.Destination)
		if err != nil || !route.Reachable {
			return e.holdReturn("Return held: no current route to the approved rendezvous. Resume to retry.")
		}
		// Goal dispatch rechecks retained authority and readiness after planning.
		e.goalNumber++
		e.goalID = e.State.MissionID + "/return/" + itoa(e.goalNumber)
		if err = e.adapter.Goal(e.goalID, e.State.Return.Destination); err != nil {
			if errors.Is(err, ErrInterrupted) {
				return nil
			}
			return e.holdReturn("Return held: " + err.Error())
		}
		e.progressAt = now
		e.bestDistance = math.Inf(1)

		e.State.State = "returning"
		e.State.Reason = "Returning to the approved rendezvous"
		return nil
	}
	if e.State.State == "returning" {
		if n.Goal.ID != e.goalID {
			if now-e.progressAt >= 15000 {
				return e.holdReturn("Return held: navigation goal unavailable")
			}
			return nil
		}
		switch n.Goal.State {
		case "aborted", "canceled", "rejected", "failed", "policy_hold", "sensor_hold", "executor_lost":
			return e.holdReturn("Return held: route " + n.Goal.State + ". Resume to retry.")
		case "succeeded":
			if math.Hypot(n.Pose.X-e.State.Return.Destination.X, n.Pose.Y-e.State.Return.Destination.Y) > .4 {
				return e.holdReturn("Return held: navigation result is outside rendezvous tolerance")
			}
			if err := e.adapter.Cancel(); err != nil {
				return e.fail(err)
			}
			e.State.State = "return_settling"
			e.returnLastReceiptMS = 0
			e.returnSettledMS = 0
			e.returnFirstStamp = 0
		default:
			// Initial zero/null path feedback must not become the best distance.
			// Arrival still requires succeeded and the rendezvous pose checks.
			if finite(n.Goal.Distance) && n.Goal.Distance > 0 && n.Goal.Distance < e.bestDistance-.08 {
				e.bestDistance = n.Goal.Distance
				e.progressAt = now
			}
			if now-e.progressAt >= 15000 {
				return e.holdReturn("Return held: no navigation progress for 15 seconds")
			}
			return nil
		}
	}
	if e.State.State == "return_settling" || e.State.State == "return_arrived" {
		if math.Hypot(n.Pose.X-e.State.Return.Destination.X, n.Pose.Y-e.State.Return.Destination.Y) > .4 {
			return e.holdReturn("Return held: robot left the rendezvous tolerance")
		}
		if math.Abs(n.Speed) > .12 || math.Abs(n.AngularSpeed) > .15 {
			e.returnLastReceiptMS = 0
			e.returnSettledMS = 0
			e.returnFirstStamp = 0
			e.State.State = "return_settling"
			return nil
		}
		if n.Stamp < e.returnLastStamp {
			return e.holdReturn("Return held: sensor clock moved backwards")
		}
		if n.Stamp <= e.returnLastStamp {
			return nil
		}
		// The executor polls every 200 ms. At most one second may pass
		// between fresh samples in either clock; missing time is not dwell.
		if e.returnLastReceiptMS != 0 && (now-e.returnLastReceiptMS > 1000 || n.Stamp-e.returnLastStamp > 1) {
			e.returnSettledMS = 0
			e.returnFirstStamp = 0
			e.State.State = "return_settling"
		}
		e.returnLastReceiptMS = now
		e.returnLastStamp = n.Stamp
		if e.returnSettledMS == 0 {
			e.returnSettledMS = now
			e.returnFirstStamp = n.Stamp
		}
		if now-e.returnSettledMS >= 2000 && n.Stamp-e.returnFirstStamp >= 2 {
			e.State.State = "return_arrived"
			e.State.Reason = "Measured settled arrival; recording mission completion"
		}
	}
	return nil
}

// Arrival reports the original continuous capture interval only after settling.
func (e *Engine) Arrival() (since int64, first, last float64, ok bool) {
	return e.returnSettledMS, e.returnFirstStamp, e.returnLastStamp, e.State.State == "return_arrived"
}
