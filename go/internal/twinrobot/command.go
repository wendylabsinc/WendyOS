package twinrobot

import (
	"context"
	"errors"
	"math"
	"time"
)

// Command is a finite operator goal, never a velocity or an LLM control loop.
// Turning reuses the measured 60-degree controller, with a stop between steps.
type Command struct {
	Action  string  `json:"action"`
	Degrees int     `json:"degrees,omitempty"`
	Frame   string  `json:"frame,omitempty"`
	X       float64 `json:"x,omitempty"`
	Z       float64 `json:"z,omitempty"`
}

type CommandResult struct {
	State    string            `json:"state"`
	Message  string            `json:"message"`
	Snapshot SiteRobotSnapshot `json:"snapshot"`
}

func (r *SiteRobot) CommandActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.intentActive
}

// ExecuteCommand shares the existing controller and does not consume desktop
// sequence numbers. Any newer desktop order supersedes it. Cancellation stops
// this goal, but never sends a late stop over a newer operator's goal.
func (r *SiteRobot) ExecuteCommand(ctx context.Context, q Command) CommandResult {
	result := func(state, message string) CommandResult {
		return CommandResult{State: state, Message: message, Snapshot: r.Status()}
	}
	if q.Action != "stop" && q.Action != "turn" && q.Action != "navigate" {
		return result("rejected", "Unknown robot command.")
	}
	if q.Action == "turn" && (q.Degrees == 0 || q.Degrees < -180 || q.Degrees > 180) {
		return result("rejected", "Turn by 1 to 180 degrees; positive is left, negative is right.")
	}
	if q.Action == "navigate" && (q.Frame == "" || math.IsNaN(q.X) || math.IsInf(q.X, 0) || math.IsNaN(q.Z) || math.IsInf(q.Z, 0)) {
		return result("rejected", "A saved destination in the current map is required.")
	}
	r.mu.Lock()
	if q.Action == "stop" {
		r.intentGeneration++
		r.intentActive = false
		r.motionRecoveryAllowed = false
		r.message = "Stopped"
		err := r.stopLocked()
		r.mu.Unlock()
		if err != nil {
			return result("failed", err.Error())
		}
		return result("completed", "Stop acknowledged.")
	}
	if ctx.Err() != nil || r.intentActive || r.moving || r.stopUnconfirmed || r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return result("rejected", "Robot is busy, stopped without acknowledgement, disconnected, or the request was canceled.")
	}
	r.intentGeneration++
	generation, sequence := r.intentGeneration, r.commandSequence
	r.intentActive = true
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.intentGeneration == generation {
			r.intentActive = false
		}
	}()

	steps, direction, remaining := 1, 1, q.Degrees
	if q.Action == "turn" {
		if remaining < 0 {
			remaining, direction = -remaining, -1
		}
		steps = (remaining + 59) / 60
	}
	for step := 0; step < steps; step++ {
		r.mu.Lock()
		if r.intentGeneration != generation || r.commandSequence != sequence {
			r.mu.Unlock()
			return result("superseded", "A newer operator command replaced this goal.")
		}
		if ctx.Err() != nil {
			err := r.stopLocked()
			r.mu.Unlock()
			return result("canceled", errors.Join(ctx.Err(), err).Error())
		}
		r.heartbeat = time.Now()
		var err error
		if q.Action == "turn" {
			angle := min(remaining, 60)
			err = r.lookAngleLocked(direction, float64(angle)*math.Pi/180)
			remaining -= angle
		} else {
			err = r.destinationLocked(q.Frame, q.X, q.Z, true)
		}
		r.mu.Unlock()
		if err != nil {
			return result("rejected", err.Error())
		}
		for {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			r.mu.Lock()
			if r.intentGeneration != generation || r.commandSequence != sequence {
				r.mu.Unlock()
				return result("superseded", "A newer operator command replaced this goal.")
			}
			if ctx.Err() != nil {
				err := r.stopLocked()
				r.mu.Unlock()
				return result("canceled", errors.Join(ctx.Err(), err).Error())
			}
			r.heartbeat = time.Now()
			moving, message := r.moving, r.message
			success := !r.stopUnconfirmed && !r.closed && r.ctx.Err() == nil &&
				((q.Action == "turn" && r.lookState == 2) || (q.Action == "navigate" && message == "Arrived"))
			r.mu.Unlock()
			if !moving {
				if !success {
					return result("failed", message)
				}
				break
			}
		}
	}
	return result("completed", "Goal completed and Stop acknowledged.")
}
