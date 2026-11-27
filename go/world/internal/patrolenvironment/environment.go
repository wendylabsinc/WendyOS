// Package patrolenvironment evaluates an opt-in local simulation gas policy.
// Its screening quantity is measured concentration plus declared uncertainty,
// not a toxicology limit, calibrated confidence interval, or physical safety claim.
package patrolenvironment

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
)

const Schema = "wendy.sim.local-environment.v1"
const Method = "original-step-local-gas-v1"
const MaxAgeMS int64 = 1000
const MaxGapNS int64 = 500000000
const MaxBatch = 64

type Policy struct {
	SensorID    string  `json:"sensor_id"`
	Calibration string  `json:"calibration_revision"`
	Species     string  `json:"species"`
	Unit        string  `json:"unit"`
	HoldPPM     float64 `json:"hold_ppm"`
	ClearPPM    float64 `json:"clear_ppm"`
	ClearForMS  int64   `json:"clear_for_ms"`
}

func (p Policy) Validate() error {
	if p.SensorID != "gas-1" || p.Calibration != "go2-virtual-mounts-v1" || p.Species != "synthetic-test-gas" || p.Unit != "ppm" || !finite(p.HoldPPM) || !finite(p.ClearPPM) || p.ClearPPM < 0 || p.HoldPPM <= p.ClearPPM || p.HoldPPM > 1e6 || p.ClearForMS < 1000 || p.ClearForMS > 30000 {
		return errors.New("environment watch requires gas-1 synthetic-test-gas ppm, known calibration, 0 <= clear < hold <= 1e6 and 1000..30000 ms clear interval")
	}
	return nil
}
func (p *Policy) UnmarshalJSON(raw []byte) error {
	type wire Policy
	var v wire
	if err := decodeRequired(raw, &v, []string{"sensor_id", "calibration_revision", "species", "unit", "hold_ppm", "clear_ppm", "clear_for_ms"}); err != nil {
		return err
	}
	*p = Policy(v)
	return nil
}

type Sample struct {
	Schema         string         `json:"schema"`
	Method         string         `json:"method"`
	RunID          string         `json:"run_id"`
	BootID         string         `json:"boot_id"`
	ModelSHA256    string         `json:"model_sha256"`
	Frame          world.FrameRef `json:"frame"`
	SensorID       string         `json:"sensor_id"`
	Calibration    string         `json:"calibration_revision"`
	Species        string         `json:"species"`
	Unit           string         `json:"unit"`
	Provenance     string         `json:"provenance"`
	Sequence       uint64         `json:"sequence"`
	SimulationNS   int64          `json:"simulation_ns"`
	CapturedUnixMS int64          `json:"captured_unix_ms"`
	ValuePPM       float64        `json:"value_ppm"`
	UncertaintyPPM float64        `json:"uncertainty_ppm"`
	Valid          bool           `json:"valid"`
}

func (s *Sample) UnmarshalJSON(raw []byte) error {
	type wire Sample
	var v wire
	if err := decodeRequired(raw, &v, []string{"schema", "method", "run_id", "boot_id", "model_sha256", "frame", "sensor_id", "calibration_revision", "species", "unit", "provenance", "sequence", "simulation_ns", "captured_unix_ms", "value_ppm", "uncertainty_ppm", "valid"}); err != nil {
		return err
	}
	*s = Sample(v)
	return nil
}
func decodeRequired(raw []byte, v any, required []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range required {
		b, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return errors.New("environment policy/sample requires explicit non-null values and scope")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type Ledger struct {
	Policy         Policy         `json:"policy"`
	RunID          string         `json:"run_id"`
	ModelSHA256    string         `json:"model_sha256"`
	Frame          world.FrameRef `json:"frame"`
	BootID         string         `json:"boot_id"`
	Last           Sample         `json:"last"`
	UpperPPM       float64        `json:"screening_ppm"`
	ClearSinceNS   int64          `json:"clear_since_ns"`
	ClearElapsedMS int64          `json:"clear_elapsed_ms"`
	Ready          bool           `json:"ready"`
	Held           bool           `json:"held"`
	Permanent      bool           `json:"permanent"`
	Reason         string         `json:"reason"`
	lastWallMS     int64
}

func New(p Policy, runID, model, boot string, frame world.FrameRef) (*Ledger, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if runID == "" || len(model) != 64 || boot == "" || frame.ID == "" || frame.Revision == "" {
		return nil, errors.New("environment watch requires current run, boot, model and frame")
	}
	return &Ledger{Policy: p, RunID: runID, ModelSHA256: model, Frame: frame, BootID: boot, Held: true, Reason: "Waiting for original local gas samples and a complete clear interval; Resume explicitly."}, nil
}
func (l *Ledger) hold(reason string) {
	l.Ready = false
	l.Held = true
	l.ClearSinceNS = 0
	l.ClearElapsedMS = 0
	l.Reason = reason
}
func (l *Ledger) fresh(now int64) bool {
	return l.Last.CapturedUnixMS > 0 && l.Last.CapturedUnixMS <= now && now-l.Last.CapturedUnixMS <= MaxAgeMS
}

// Update consumes a bounded retained window. Overlap is permitted; duplicate
// reads cannot advance clearance. A lost sequence or excessive original gap
// starts a new clear interval. HTTP receipt time never contributes dwell.
func (l *Ledger) Update(batch []Sample, now int64) {
	if l.Permanent {
		return
	}
	if now < l.lastWallMS {
		l.Permanent = true
		l.hold("Local clock regressed; new mission required.")
		return
	}
	l.lastWallMS = now
	if len(batch) == 0 || len(batch) > MaxBatch {
		l.hold("Local gas samples unavailable; navigation held.")
		return
	}
	if l.Last.Sequence == 0 {
		batch = batch[len(batch)-1:]
	}
	if batch[len(batch)-1].Sequence < l.Last.Sequence {
		l.hold("Replayed gas window; navigation held.")
		return
	}
	var previous uint64
	for i, s := range batch {
		if i > 0 && s.Sequence <= previous {
			l.hold("Gas sample batch is reordered; navigation held.")
			return
		}
		previous = s.Sequence
		if s.RunID != l.RunID || s.BootID != l.BootID || s.ModelSHA256 != l.ModelSHA256 || s.Frame != l.Frame {
			l.Permanent = true
			l.hold("Gas source run, boot, model or frame changed; new mission required.")
			return
		}
		if s.Schema != Schema || s.Method != Method || s.SensorID != l.Policy.SensorID || s.Calibration != l.Policy.Calibration || s.Species != l.Policy.Species || s.Unit != l.Policy.Unit || s.Provenance != "synthetic" || s.Sequence == 0 || s.SimulationNS <= 0 || s.CapturedUnixMS <= 0 || s.CapturedUnixMS > now || !s.Valid || !finite(s.ValuePPM) || !finite(s.UncertaintyPPM) || s.ValuePPM < 0 || s.UncertaintyPPM < 0 || s.ValuePPM > 1e6 || s.UncertaintyPPM > 1e6 {
			l.hold("Local gas measurement invalid or scope unknown; navigation held.")
			return
		}
		if s.Sequence < l.Last.Sequence {
			continue
		}
		if s.Sequence == l.Last.Sequence {
			if s != l.Last {
				l.Permanent = true
				l.hold("Gas sample identity changed; new mission required.")
				return
			}
			continue
		}
		if l.Last.Sequence > 0 && (s.SimulationNS <= l.Last.SimulationNS || s.CapturedUnixMS < l.Last.CapturedUnixMS) {
			l.hold("Original gas sample clock regressed; navigation held.")
			return
		}
		if l.Last.Sequence == 0 || s.Sequence != l.Last.Sequence+1 || s.SimulationNS-l.Last.SimulationNS > MaxGapNS {
			l.hold("Original gas sample gap; collecting a new clear interval.")
		}
		l.Last = s
		l.UpperPPM = s.ValuePPM + s.UncertaintyPPM
		if l.UpperPPM >= l.Policy.HoldPPM {
			l.hold("Local gas screening threshold reached; navigation held.")
			continue
		}
		if l.Held {
			if l.UpperPPM > l.Policy.ClearPPM {
				l.hold("Waiting for gas screening value at or below the signed clear threshold.")
				continue
			}
			if l.ClearSinceNS == 0 {
				l.ClearSinceNS = s.SimulationNS
			}
			l.ClearElapsedMS = (s.SimulationNS - l.ClearSinceNS) / 1000000
			l.Ready = l.ClearElapsedMS >= l.Policy.ClearForMS
			l.Reason = "Collecting original clear samples; navigation remains held."
			if l.Ready {
				l.Reason = "Clear interval observed; Resume explicitly. Other mission conditions still apply."
			}
		} else {
			l.Ready = true
			l.Reason = "Local environmental policy satisfied."
		}
	}
	if !l.fresh(now) {
		l.hold("Local gas sample stale; navigation held.")
	}
}
func (l *Ledger) Check(now int64) {
	if now < l.lastWallMS {
		l.Permanent = true
		l.hold("Local clock regressed; new mission required.")
	}
	l.lastWallMS = now
	if !l.fresh(now) {
		l.hold("Local gas sample stale; navigation held.")
	}
}
func (l *Ledger) Resume(now int64) error {
	l.Check(now)
	if l.Permanent || !l.Ready {
		return errors.New(l.Reason)
	}
	l.Held = false
	l.Reason = "Local environmental policy satisfied."
	return nil
}
