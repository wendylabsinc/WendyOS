// Package robotcal is what a robot knows about its own calibration: the record
// one procedure leaves behind, the gate that decides whether it counts, and the
// device-local store that keeps both.
//
// Nothing here knows what a robot is. A humanoid's camera extrinsics and a
// 6-DOF arm's per-joint zero offsets produce the same record, because the
// universal part of a calibration is "did it pass, by how much, when, and by
// what method" — not what was measured. Everything robot-specific arrives as a
// Profile, which the platform reads for the few fields it can enforce something
// with and otherwise carries opaquely.
//
// It lives in shared/ for the same reason streamreason does: the store is
// device-local and will be owned by the agent, while the wizard that writes
// into it runs on a laptop. One spelling of the record, or the two sides
// disagree silently about whether a robot is calibrated.
package robotcal

import (
	"encoding/json"
	"fmt"
	"time"
)

// Quantity is a magnitude with the unit it was measured in.
//
// Unit is a free string on purpose. The SO-101's joint travel is in encoder
// ticks, the G1's is in radians, a camera's extrinsic error is in metres, and
// the next robot will bring something none of those anticipate. An enumeration
// here would have to be edited for every robot family, which is the failure
// this package exists to avoid.
//
// This is deliberately not robotinspect.Quantity, which requires an axis for an
// angle and a frame for a translation. Those qualifiers belong to a measurement
// of a thing; a residual is the magnitude of that measurement's error, and has
// no axis of its own. Axis and Frame are carried here only when a method has
// them to give, and two quantities are comparable only when all three match.
type Quantity struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	// Axis and Frame qualify the quantity when the method produced it along a
	// particular axis or in a particular frame. Empty for a scalar residual.
	Axis  string `json:"axis,omitempty"`
	Frame string `json:"frame,omitempty"`
}

func (q Quantity) String() string {
	s := fmt.Sprintf("%g %s", q.Value, q.Unit)
	if q.Axis != "" {
		s += " (" + q.Axis + ")"
	}
	if q.Frame != "" {
		s += " in " + q.Frame
	}
	return s
}

// comparableWith reports whether two quantities describe the same kind of
// magnitude, so that comparing them is meaningful.
func (q Quantity) comparableWith(other Quantity) bool {
	return q.Unit == other.Unit && q.Axis == other.Axis && q.Frame == other.Frame
}

// Record is the outcome of one calibration procedure, whatever produced it.
//
// Residual is a pointer because "not measured" and "measured as zero" are
// different answers and must stay different: a skipped joint that recorded a
// residual of 0 would qualify, which is the exact failure a skip is supposed to
// make visible. A nil Residual never qualifies.
type Record struct {
	ID         string          `json:"id"`
	Qualified  bool            `json:"qualified"`
	Residual   *Quantity       `json:"residual"`
	Budget     Quantity        `json:"budget"`
	MeasuredAt time.Time       `json:"measured_at"`
	Method     string          `json:"method"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Verdict is why a record did or did not qualify. It is machine-readable and
// part of the store's contract, the same discipline as robotinspect.Unknown —
// an operator reading "not qualified" must be able to tell "nobody measured it"
// from "we measured it and it is outside the budget".
type Verdict string

const (
	// VerdictQualified: the residual fits inside the budget.
	VerdictQualified Verdict = "QUALIFIED"
	// VerdictOverBudget: it was measured, and it does not fit.
	VerdictOverBudget Verdict = "OVER_BUDGET"
	// VerdictNotMeasured: the procedure produced no residual — it was skipped,
	// or every step in it was. Never the same as a residual of zero.
	VerdictNotMeasured Verdict = "NOT_MEASURED"
	// VerdictNeverRun: the profile asks for this calibration and the store has
	// no record of it at all.
	VerdictNeverRun Verdict = "NEVER_RUN"
	// VerdictIncomparable: the residual and the budget are not the same kind of
	// magnitude — a travel shortfall in ticks against a budget in radians. This
	// is a refusal, not a comparison: the numbers would compare fine and the
	// answer would be meaningless.
	VerdictIncomparable Verdict = "INCOMPARABLE"
	// VerdictNoBudget: no budget was declared, so there is nothing to pass. A
	// calibration with no budget cannot qualify, because "it ran" is not the
	// gate.
	VerdictNoBudget Verdict = "NO_BUDGET"
	// VerdictStale: the record qualified, but something it depended on has
	// changed since — a repair, a re-bind, a profile revision.
	VerdictStale Verdict = "STALE"
)

// Qualify is the gate, and the only place a calibration is judged.
//
// Running a procedure to completion qualifies nothing; fitting inside the
// budget does. Everything else in this package exists to get a residual and a
// budget in front of this function.
func Qualify(residual *Quantity, budget Quantity) (bool, Verdict) {
	if budget.Unit == "" || budget.Value <= 0 {
		return false, VerdictNoBudget
	}
	if residual == nil {
		return false, VerdictNotMeasured
	}
	if !residual.comparableWith(budget) {
		return false, VerdictIncomparable
	}
	if residual.Value <= budget.Value {
		return true, VerdictQualified
	}
	return false, VerdictOverBudget
}

// NewRecord builds a record with the gate already applied, so no caller can
// construct one whose Qualified flag disagrees with its own numbers.
func NewRecord(id, method string, residual *Quantity, budget Quantity, at time.Time, payload json.RawMessage) (Record, Verdict) {
	qualified, verdict := Qualify(residual, budget)
	return Record{
		ID:         id,
		Qualified:  qualified,
		Residual:   residual,
		Budget:     budget,
		MeasuredAt: at,
		Method:     method,
		Payload:    payload,
	}, verdict
}

// Verdict re-derives why a stored record stands where it does, without trusting
// the stored flag. A record written by an older CLI, or edited by hand, is
// judged by the same gate as a fresh one.
func (r Record) Verdict() Verdict {
	_, v := Qualify(r.Residual, r.Budget)
	return v
}

// Status is one row of `calibrate status`: what the profile asks for, beside
// what the store has.
type Status struct {
	ID      string  `json:"id"`
	Method  string  `json:"method"`
	Class   string  `json:"class"`
	Verdict Verdict `json:"verdict"`
	// Record is nil when the procedure has never been run.
	Record *Record `json:"record,omitempty"`
	// Runnable is false when this build cannot run the procedure at all, with
	// Reason naming what is missing. A status listing says so rather than
	// offering a procedure that will refuse the moment it is chosen.
	Runnable bool   `json:"runnable"`
	Reason   string `json:"reason,omitempty"`
}
