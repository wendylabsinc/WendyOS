// Package patrolwatch applies a signed synthetic source-region condition to a
// patrol. A region outside the watch rectangle is not route or gas clearance.
package patrolwatch

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/gasview"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatchpolicy"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Method = patrolwatchpolicy.Method

// Policy is shared with signed task definitions without importing this runtime.
type Policy = patrolwatchpolicy.Policy

type Ledger struct {
	Policy          Policy            `json:"policy"`
	Ready           bool              `json:"ready"`
	Held            bool              `json:"held"`
	Permanent       bool              `json:"permanent"`
	Reason          string            `json:"reason"`
	Transport       string            `json:"transport"`
	Selection       gasview.Selection `json:"selection"`
	RemainingMS     int64             `json:"remaining_ms"`
	OriginalUnixMS  int64             `json:"original_unix_ms"`
	selector        *gasview.Selector
	records         []world.Component
	last            time.Time
	authorizedUntil time.Time
}

func New(p Policy, run string, frame world.FrameRef) (*Ledger, error) {
	if e := p.Validate(); e != nil {
		return nil, e
	}
	if p.Contract.Field.Simulation.RunID != run || p.Contract.Field.Frame != frame {
		return nil, errors.New("world watch contract differs from active physics run/frame")
	}
	raw, _ := json.Marshal(p)
	var owned Policy
	if e := json.Unmarshal(raw, &owned); e != nil {
		return nil, e
	}
	s, e := gasview.New(owned.Contract)
	if e != nil {
		return nil, e
	}
	return &Ledger{Policy: owned, Held: true, Reason: "Waiting for compatible World evidence; Resume explicitly.", Transport: "unavailable", selector: s}, nil
}
func (l *Ledger) hold(reason string) {
	l.Ready = false
	l.Held = true
	l.Reason = reason
}

// Observe accepts only a completed authenticated node query. Empty snapshots
// replace the cache. A refused query invalidates it immediately. Transport loss
// retains the previous originals only until their existing deadlines.
func (l *Ledger) Observe(records []world.Component, transport string, authorizedUntil, timeNow time.Time) {
	l.Transport = transport
	l.authorizedUntil = authorizedUntil
	switch transport {
	case "connected":
		raw, e := json.Marshal(records)
		if e != nil || len(raw) > 1<<20 || len(records) > 256 {
			l.records = nil
			l.hold("World snapshot exceeds bounds.")
			return
		}
		// A replacement query must start from zero-valued records. Reusing a
		// struct slice preserves omitted provenance fields when record order changes.
		var replacement []world.Component
		if err := json.Unmarshal(raw, &replacement); err != nil {
			l.records = nil
			l.hold("World snapshot cannot be decoded.")
			return
		}
		l.records = replacement
	case "refused":
		l.records = nil
		l.hold("World reader authorization refused; navigation held.")
	case "unavailable":
	default:
		l.records = nil
		l.hold("World reader state invalid.")
	}
	l.Check(timeNow)
}

// CheckTick receives the engine's captured integer tick, not a new clock read.
// A later pre-motion observation may already have checked a newer instant. Do
// not roll that decision backwards or turn scheduling latency into clock failure.
func (l *Ledger) CheckTick(nowMS int64) {
	now := time.UnixMilli(nowMS)
	if now.Before(l.last) {
		now = l.last
	}
	l.Check(now)
}
func (l *Ledger) Check(now time.Time) {
	now = time.UnixMilli(now.UnixMilli())
	l.RemainingMS = 0
	if l.Permanent {
		return
	}
	if !l.last.IsZero() && now.Before(l.last) {
		l.Permanent = true
		l.hold("World watch clock regressed; new mission required.")
		return
	}
	l.last = now
	if !now.Before(l.authorizedUntil) {
		l.Selection = gasview.Selection{}
		l.hold("World reader grant unavailable or expired.")
		return
	}
	usable := []world.Component{}
	for _, c := range l.records {
		if c.Key.Type == gasreplica.ClaimType && now.Sub(c.Timing.SampleTime) <= time.Duration(l.Policy.MaxAgeMS)*time.Millisecond {
			usable = append(usable, c)
		}
	}
	selection, e := l.selector.Step(usable, now)
	l.Selection = selection
	if e != nil || selection.State != "localized" || selection.Result.Estimate == nil {
		l.hold("Required World evidence missing, expired, ambiguous or disputed; navigation held.")
		return
	}
	estimate := selection.Result.Estimate
	l.RemainingMS = int64(math.Floor((estimate.ValidUntil - selection.Result.Now) * 1000))
	for _, c := range usable {
		if gasreplica.ComponentHash(c) == selection.Hash {
			l.OriginalUnixMS = c.Timing.SampleTime.UnixMilli()
			l.RemainingMS = min(l.RemainingMS, l.OriginalUnixMS+l.Policy.MaxAgeMS-now.UnixMilli())
		}
	}
	l.RemainingMS = min(l.RemainingMS, l.authorizedUntil.Sub(now).Milliseconds())
	if l.RemainingMS <= 0 {
		l.hold("Original World evidence deadline elapsed.")
		return
	}
	// Every supported grid cell is tested with its half-cell extent. The selected
	// region may be disconnected; a centroid or radius must not hide an overlap.
	half := l.Policy.Contract.Field.Step / 2
	for _, cell := range estimate.Cells {
		p := cell.Position
		if p[0]+half >= l.Policy.MinXZ[0] && p[0]-half <= l.Policy.MaxXZ[0] && p[1]+half >= l.Policy.MinXZ[1] && p[1]-half <= l.Policy.MaxXZ[1] {
			l.hold("Supported source region overlaps the signed watch rectangle; navigation held.")
			return
		}
	}
	if len(estimate.Cells) == 0 {
		l.hold("Source region has no supported cells.")
		return
	}
	l.Ready = true
	l.Reason = "Synthetic source-region condition satisfied; local motion conditions still apply."
	if l.Held {
		l.Reason = "Compatible World evidence restored; Resume explicitly."
	}
}
func (l *Ledger) Resume(now time.Time) error {
	l.Check(now)
	if !l.Ready || l.Permanent {
		return errors.New(l.Reason)
	}
	l.Held = false
	return nil
}
