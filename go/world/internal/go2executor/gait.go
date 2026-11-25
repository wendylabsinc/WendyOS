package go2executor

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

type motionAxis uint8

const (
	axisNone motionAxis = iota
	axisForward
	axisYaw
)

// Process-local only: a zero publication does not erase a live prior target.
type gaitTransition struct {
	startupSettled bool
	axis           motionAxis
	prior          go2nav.Command
	priorNotAfter  int64
	firstZero      *BridgeCommandReceipt
	clearedCapture uint64
}

func commandAxis(command go2nav.Command) motionAxis {
	if command.ForwardMPS != 0 {
		return axisForward
	}
	if command.YawRateRPS != 0 {
		return axisYaw
	}
	return axisNone
}

func fullySettled(s State) bool {
	return finite(s.BodyVelocity[0]) && finite(s.BodyVelocity[1]) && finite(s.BodyVelocity[2]) &&
		math.Hypot(s.BodyVelocity[0], s.BodyVelocity[1]) <= .01 && math.Abs(s.BodyVelocity[2]) <= .01
}

func (c *Controller) gaitBarrierSatisfied(s State) bool {
	z := c.gait.firstZero
	if z == nil || s.CaptureSequence <= z.Request.CaptureSequence || !fullySettled(s) {
		return false
	}
	boundary := z.ResponseCompletedUnixNS
	if c.gait.priorNotAfter > 0 {
		if c.gait.priorNotAfter > math.MaxInt64/int64(time.Millisecond) {
			return false
		}
		boundary = max(boundary, c.gait.priorNotAfter*int64(time.Millisecond))
	}
	errorNS := c.profile.ClockErrorMS * int64(time.Millisecond)
	// Original capture lower bounds, not new delivery times, cross both fences.
	return s.OdomCapturedUnixNS > errorNS && s.ScanCapturedUnixNS > errorNS &&
		s.OdomCapturedUnixNS-errorNS > boundary && s.ScanCapturedUnixNS-errorNS > boundary
}

func (c *Controller) gaitProposal(s State, proposed go2nav.Command) go2nav.Command {
	axis := commandAxis(proposed)
	transition := !c.gait.startupSettled || (axis != axisNone && c.gait.axis != axisNone && axis != c.gait.axis)
	cleared := c.gaitBarrierSatisfied(s)
	if cleared {
		c.gait.startupSettled = true
		c.gait.clearedCapture = s.CaptureSequence
		// Settled original samples are after expiry of the retained nonzero target.
		// This also discharges a stopped target at the goal. Keep the axis history.
		c.gait.prior = go2nav.Command{}
		c.gait.priorNotAfter = 0
	}
	if transition && !cleared {
		return go2nav.Command{}
	}
	return proposed
}

func (c *Controller) acknowledgeGaitCommand(request CommandRequest) error {
	receipt := c.lastAcknowledgedReceipt
	if receipt == nil || receipt.Request != request || !acknowledgmentMatches(receipt.Acknowledgment, request) {
		return errors.New("gait transition requires a matched bridge publication receipt")
	}
	w, _, err := c.clock()
	if err != nil {
		return err
	}
	if receipt.RequestStartedUnixNS <= 0 || receipt.ResponseCompletedUnixNS < receipt.RequestStartedUnixNS ||
		receipt.ResponseCompletedUnixNS/int64(time.Millisecond) > w || request.NotAfterUnixMS > math.MaxInt64/int64(time.Millisecond) {
		return errors.New("gait publication receipt timing is unavailable or inconsistent")
	}
	command := go2nav.Command{ForwardMPS: request.BodyVX, YawRateRPS: request.YawRate}
	axis := commandAxis(command)
	if axis == axisNone {
		if c.gait.firstZero == nil {
			c.gait.firstZero = copyReceipt(receipt)
		}
		return nil
	}
	if !c.gait.startupSettled {
		return errors.New("gait startup settlement is unavailable")
	}
	c.gait.axis, c.gait.prior, c.gait.priorNotAfter = axis, command, request.NotAfterUnixMS
	c.gait.firstZero = nil
	return nil
}

// CheckContinuation computes independent prior maxima. Preserve the same maxima
// across all three validated axis targets without expanding its two-entry API.
func (c *Controller) gaitPriors(s State) ([]go2nav.Command, error) {
	if s.PreviousCommand != c.lastCommand {
		return nil, errors.New("bridge command differs from acknowledged gait history")
	}
	values := []go2nav.Command{
		{ForwardMPS: s.PreviousCommand[0], YawRateRPS: s.PreviousCommand[2]},
		{ForwardMPS: c.lastCommand[0], YawRateRPS: c.lastCommand[2]}, c.gait.prior,
	}
	pv, pw := 0.0, 0.0
	for _, value := range values {
		if !finite(value.ForwardMPS) || !finite(value.YawRateRPS) || value.ForwardMPS < 0 || value.ForwardMPS > c.record.Spec.SpeedMPS || math.Abs(value.YawRateRPS) > c.profile.MaxYawRateRPS || (value.ForwardMPS != 0 && value.YawRateRPS != 0) {
			return nil, errors.New("gait prior command is outside its signed axis envelope")
		}
		pv, pw = max(pv, value.ForwardMPS), max(pw, math.Abs(value.YawRateRPS))
	}
	return []go2nav.Command{{ForwardMPS: pv}, {YawRateRPS: pw}}, nil
}

// GaitDiagnostic copies process-local transition evidence. It is not a robot
// receipt or a separate permission check; producing it does not sample clocks.
type GaitDiagnostic struct {
	StartupSettled           bool                  `json:"startup_settled"`
	AcknowledgedAxis         string                `json:"acknowledged_nonzero_axis"`
	RetainedTarget           [3]float64            `json:"retained_target"`
	RetainedNotAfterUnixMS   int64                 `json:"retained_not_after_unix_ms"`
	FirstZeroReceipt         *BridgeCommandReceipt `json:"first_zero_bridge_publication_receipt,omitempty"`
	ClearedAtCaptureSequence uint64                `json:"last_barrier_cleared_capture_sequence"`
}

func copyGaitDiagnostic(in *GaitDiagnostic) *GaitDiagnostic {
	if in == nil {
		return nil
	}
	out := *in
	out.FirstZeroReceipt = copyReceipt(in.FirstZeroReceipt)
	return &out
}

func (c *Controller) gaitDiagnostic() *GaitDiagnostic {
	if c.profile.MotionEnvelope != go2nav.IsotropicGaitV1 {
		return nil
	}
	axis := "none"
	if c.gait.axis == axisForward {
		axis = "forward"
	} else if c.gait.axis == axisYaw {
		axis = "yaw"
	}
	return &GaitDiagnostic{StartupSettled: c.gait.startupSettled, AcknowledgedAxis: axis,
		RetainedTarget: [3]float64{c.gait.prior.ForwardMPS, 0, c.gait.prior.YawRateRPS}, RetainedNotAfterUnixMS: c.gait.priorNotAfter,
		FirstZeroReceipt: copyReceipt(c.gait.firstZero), ClearedAtCaptureSequence: c.gait.clearedCapture}
}
