package patrolkit

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"math"
)

// Boot retains activation identity independently of an optional work allowance.
// Work telemetry is used only for its scoped, monotonic physics-step identity;
// truth pose is never promoted into navigation authority.
type Boot struct {
	RunID              string `json:"run_id"`
	ModelSHA256        string `json:"model_sha256"`
	BootID             string `json:"boot_id"`
	LastSequence       uint64 `json:"last_sequence"`
	LastSimulationNS   int64  `json:"last_simulation_ns"`
	LastCapturedUnixMS int64  `json:"last_captured_unix_ms"`
	Ready              bool   `json:"ready"`
	Failed             bool   `json:"failed"`
	Reason             string `json:"reason,omitempty"`
}

func NewBoot(p Profile, s patrolwork.Sample, now int64) (*Boot, error) {
	if s.RunID != p.RunID || s.ModelSHA256 != p.ModelSHA256 || s.BootID == p.PreparationBootID {
		return nil, errors.New("activation requires the intended run and a fresh physics boot")
	}
	b := &Boot{RunID: p.RunID, ModelSHA256: p.ModelSHA256, BootID: s.BootID}
	if e := b.Observe(&s, now); e != nil {
		return nil, e
	}
	return b, nil
}
func (b *Boot) Observe(s *patrolwork.Sample, now int64) error {
	if b.Failed {
		return errors.New(b.Reason)
	}
	b.Ready = false
	// Lost/frozen delivery is a temporary hold. A changed identity or regressing
	// original-step clock is permanent; neither fresh data nor Resume can reset it.
	if s == nil || !s.Valid || s.CapturedUnixMS > now || now-s.CapturedUnixMS > patrolwork.MaxAgeMS {
		b.Reason = "Offline activation identity unavailable or stale; Resume requires fresh evidence from the same boot"
		return errors.New(b.Reason)
	}
	reason := ""
	if s.Schema != patrolwork.Schema || s.Method != patrolwork.Method || s.RunID != b.RunID || s.ModelSHA256 != b.ModelSHA256 || s.BootID == "" || s.BootID != b.BootID {
		reason = "Offline activation physics identity changed; a new approval is required"
	} else if s.Sequence == 0 || s.SimulationNS <= 0 || s.Sequence < b.LastSequence || s.SimulationNS < b.LastSimulationNS || s.CapturedUnixMS < b.LastCapturedUnixMS || math.IsNaN(s.TotalJ) || math.IsInf(s.TotalJ, 0) || s.TotalJ < 0 {
		reason = "Offline activation physics clock reset; a new approval is required"
	}
	if reason != "" {
		b.Failed = true
		b.Reason = reason
		return errors.New(reason)
	}
	b.Ready = true
	b.Reason = ""
	b.LastSequence = s.Sequence
	b.LastSimulationNS = s.SimulationNS
	b.LastCapturedUnixMS = s.CapturedUnixMS
	return nil
}
