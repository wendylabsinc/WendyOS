// Package patrolwork accounts for positive actuator mechanical work in simulation.
// Joules here are not battery energy, state of charge or a return guarantee.
package patrolwork

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
)

const Schema = "wendy.sim.positive-actuator-work.v1"
const Method = "post-step-positive-joint-power-v1"
const MaxAgeMS int64 = 1000

type Policy struct {
	LimitJ         float64 `json:"limit_j"`
	ReturnReserveJ float64 `json:"return_reserve_j"`
}

func (p Policy) Validate(hasReturn bool) error {
	if !finite(p.LimitJ) || !finite(p.ReturnReserveJ) || p.LimitJ <= 0 || p.LimitJ > 1e9 || p.ReturnReserveJ < 0 || p.ReturnReserveJ >= p.LimitJ || (p.ReturnReserveJ > 0 && !hasReturn) {
		return errors.New("work allowance requires finite 0 < limit <= 1e9 joules and 0 <= reserve < limit; reserve requires approved return")
	}
	return nil
}
func (p *Policy) UnmarshalJSON(raw []byte) error {
	var fields struct {
		Limit   *float64 `json:"limit_j"`
		Reserve *float64 `json:"return_reserve_j"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&fields); err != nil {
		return err
	}
	if fields.Limit == nil || fields.Reserve == nil {
		return errors.New("work allowance requires explicit non-null limit_j and return_reserve_j")
	}
	p.LimitJ = *fields.Limit
	p.ReturnReserveJ = *fields.Reserve
	return nil
}

type Sample struct {
	Schema         string  `json:"schema"`
	Method         string  `json:"method"`
	RunID          string  `json:"run_id"`
	BootID         string  `json:"boot_id"`
	ModelSHA256    string  `json:"model_sha256"`
	Sequence       uint64  `json:"sequence"`
	SimulationNS   int64   `json:"simulation_ns"`
	CapturedUnixMS int64   `json:"captured_unix_ms"`
	TotalJ         float64 `json:"total_j"`
	PowerW         float64 `json:"power_w"`
	Valid          bool    `json:"valid"`
}

// Missing or null measurements are unknown, never valid numeric zeroes.
func (s *Sample) UnmarshalJSON(raw []byte) error {
	type wire Sample
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"schema", "method", "run_id", "boot_id", "model_sha256", "sequence", "simulation_ns", "captured_unix_ms", "total_j", "power_w", "valid"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("work sample requires explicit non-null measurements and scope")
		}
	}
	var value wire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*s = Sample(value)
	return nil
}

func (s Sample) fresh(now int64) bool {
	return s.Valid && s.Schema == Schema && s.Method == Method && s.RunID != "" && s.BootID != "" && len(s.ModelSHA256) == 64 && s.Sequence > 0 && s.SimulationNS > 0 && s.CapturedUnixMS > 0 && s.CapturedUnixMS <= now && now-s.CapturedUnixMS <= MaxAgeMS && finite(s.TotalJ) && s.TotalJ >= 0 && finite(s.PowerW) && s.PowerW >= 0
}

// Ledger retains the admission baseline for the entire mission, including holds.
// A reset permanently invalidates it; recovery requires a separately admitted mission.
type Ledger struct {
	Policy     Policy  `json:"policy"`
	Baseline   Sample  `json:"baseline"`
	Last       Sample  `json:"last"`
	UsedJ      float64 `json:"used_j"`
	RemainingJ float64 `json:"remaining_j"`
	Valid      bool    `json:"valid"`
	Permanent  bool    `json:"permanent"`
	Exhausted  bool    `json:"exhausted"`
	Reason     string  `json:"reason,omitempty"`
}

func New(p Policy, s Sample, now int64, runID, model string) (*Ledger, error) {
	if err := p.Validate(true); err != nil {
		return nil, err
	}
	if !s.fresh(now) || s.RunID != runID || s.ModelSHA256 != model {
		return nil, errors.New("fresh original-step work sample with current run and model required")
	}
	return &Ledger{Policy: p, Baseline: s, Last: s, RemainingJ: p.LimitJ, Valid: true}, nil
}
func (l *Ledger) Update(s *Sample, now int64) {
	if l.Permanent {
		return
	}
	l.Valid = false
	if s == nil {
		l.Reason = "Work sample unavailable; navigation held. Resume explicitly after recovery."
		return
	}
	if s.RunID == "" || s.BootID == "" || s.ModelSHA256 == "" || s.Schema == "" || s.Method == "" {
		l.Reason = "Work sample scope unknown; navigation held. Resume explicitly after recovery."
		return
	}
	last := l.Last
	if s.RunID != last.RunID || s.BootID != last.BootID || s.ModelSHA256 != last.ModelSHA256 || s.Method != last.Method || s.Schema != last.Schema {
		l.Permanent = true
		l.Reason = "Work source changed or restarted; active allowance cannot be restored. New mission required."
		return
	}
	if s.Sequence < last.Sequence || s.SimulationNS < last.SimulationNS || s.TotalJ < last.TotalJ || (s.Sequence == last.Sequence && *s != last) || (s.Sequence > last.Sequence && s.SimulationNS <= last.SimulationNS) || s.CapturedUnixMS < last.CapturedUnixMS {
		l.Permanent = true
		l.Reason = "Work counter or original clock reset/replayed; active allowance cannot be restored. New mission required."
		return
	}
	if !s.fresh(now) {
		l.Reason = "Work sample unknown, invalid or stale; navigation held. Resume explicitly after recovery."
		return
	}
	l.Last = *s
	l.UsedJ = s.TotalJ - l.Baseline.TotalJ
	l.RemainingJ = math.Max(0, l.Policy.LimitJ-l.UsedJ)
	if l.UsedJ >= l.Policy.LimitJ {
		l.Exhausted = true
		l.Permanent = true
		l.Reason = "Positive actuator-work allowance exhausted; navigation held. New mission required."
		return
	}
	l.Valid = true
	l.Reason = ""
}
func (l *Ledger) ReserveReached() bool {
	return l.Valid && l.Policy.ReturnReserveJ > 0 && l.RemainingJ <= l.Policy.ReturnReserveJ
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
