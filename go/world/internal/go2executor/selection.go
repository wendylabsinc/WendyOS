package go2executor

import (
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// SelectionDiagnostic describes geometry selection on one original capture.
// It is not dispatch evidence. CommandSequence is populated only when the
// matching selected target is actually attempted; its ACK remains separate.
type SelectionDiagnostic struct {
	TaskID          string        `json:"task_id"`
	RunID           string        `json:"run_id"`
	SpecSHA256      string        `json:"spec_sha256"`
	ExecutorEpoch   uint64        `json:"executor_epoch"`
	CaptureSequence uint64        `json:"capture_sequence"`
	CommandSequence uint64        `json:"command_sequence,omitempty"`
	Stage           string        `json:"stage"`
	Candidate       [3]float64    `json:"candidate"`
	PriorCount      int           `json:"prior_count"`
	Priors          [2][3]float64 `json:"priors"`
	CandidateResult string        `json:"candidate_result"`
	ZeroResult      string        `json:"zero_result"`
	Outcome         string        `json:"outcome"`
	Selected        *[3]float64   `json:"selected,omitempty"`
}

func copySelection(in *SelectionDiagnostic) *SelectionDiagnostic {
	if in == nil {
		return nil
	}
	v := *in
	if in.Selected != nil {
		s := *in.Selected
		v.Selected = &s
	}
	return &v
}

func commandTarget(c go2nav.Command) [3]float64 { return [3]float64{c.ForwardMPS, 0, c.YawRateRPS} }

func clearanceResult(err error) string {
	switch err {
	case nil:
		return "clear"
	case go2nav.ErrUnknown:
		return "unknown"
	case go2nav.ErrBlocked:
		return "blocked"
	case go2nav.ErrInvalid:
		return "invalid"
	case go2nav.ErrBounds:
		return "bounds"
	default:
		return "other"
	}
}

// selectCandidate is called only after normal proposal and gait transition.
// Both checks use the same grid, original measured motion, full prior union and
// limits. Selection never discharges the retained target or changes gait state.
func (c *Controller) selectCandidate(grid *go2nav.Grid, s State, prior []go2nav.Command, candidate go2nav.Command, limits go2nav.MotionLimits) (go2nav.Command, error) {
	measured := go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}
	err := grid.CheckContinuation(s.Pose, measured, prior, candidate, limits)
	if c.profile.MotionEnvelope != go2nav.IsotropicGaitV1 {
		return candidate, err
	}
	d := &SelectionDiagnostic{TaskID: c.record.Spec.ID, RunID: c.cfg.Binding.RunID, SpecSHA256: tasking.SpecDigest(c.record.Spec), ExecutorEpoch: c.epoch, CaptureSequence: s.CaptureSequence, Stage: "joint-continuation", Candidate: commandTarget(candidate), PriorCount: len(prior), CandidateResult: clearanceResult(err), ZeroResult: "not_attempted", Outcome: "refused"}
	for i := 0; i < min(len(prior), len(d.Priors)); i++ {
		d.Priors[i] = commandTarget(prior[i])
	}
	c.selectionDiagnostic = d
	selected := candidate
	clearanceRejected := false
	switch err {
	case go2nav.ErrUnknown, go2nav.ErrBlocked:
		clearanceRejected = true
	}
	if clearanceRejected && (candidate.ForwardMPS != 0 || candidate.YawRateRPS != 0) {
		// A rejected positive envelope can extend beyond the full stopping
		// envelope. Zero is an alternative only if that entire envelope clears.
		selected = go2nav.Command{}
		err = grid.CheckContinuation(s.Pose, measured, prior, selected, limits)
		d.ZeroResult = clearanceResult(err)
		if err == nil {
			d.Outcome = "zero_after_" + d.CandidateResult
		}
	} else if err == nil {
		d.Outcome = "original"
	}
	if err == nil {
		target := commandTarget(selected)
		d.Selected = &target
	}
	return selected, err
}

func (c *Controller) selectionForCommand(r CommandRequest) *SelectionDiagnostic {
	d := c.selectionDiagnostic
	if d == nil || d.Selected == nil || d.TaskID != r.TaskID || d.RunID != c.cfg.Binding.RunID || d.SpecSHA256 != r.SpecDigest || d.ExecutorEpoch != r.ExecutorEpoch || d.CaptureSequence != r.CaptureSequence || *d.Selected != [3]float64{r.BodyVX, r.BodyVY, r.YawRate} {
		return nil
	}
	d.CommandSequence = r.Sequence
	return copySelection(d)
}

// SelectionObservation retains selection even if a later freshness/deadline
// gate refuses dispatch or an already-used capture is skipped. Call only from
// the controller owner, and join by the explicit capture identity.
func (c *Controller) SelectionObservation() (*SelectionDiagnostic, bool) {
	return copySelection(c.selectionDiagnostic), c.selectionDiagnostic != nil
}
