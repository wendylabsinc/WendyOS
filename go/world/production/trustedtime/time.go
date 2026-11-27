// Package trustedtime defines authenticated physical time and bounded holdover.
// It deliberately provides no wall-clock or simulation-time fallback. Acquiring
// and verifying a fresh external time anchor is provided by authenticatedtime.
package trustedtime

import (
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrUntrusted = errors.New("authenticated physical time unavailable")

type Interval struct {
	Earliest      time.Time
	Latest        time.Time
	BootID        string
	Monotonic     time.Duration
	Domain        string
	Authenticated bool
	Source        string
}

func (w Interval) Validate() error {
	if !w.Authenticated || w.Domain != "physical" || w.Source == "" || len(w.Source) > 256 || strings.Contains(strings.ToLower(w.Source), "simulation") || strings.Contains(strings.ToLower(w.Source), "synthetic") || w.BootID == "" || len(w.BootID) > 256 || w.Monotonic < 0 || w.Earliest.IsZero() || w.Earliest.UnixMilli() <= 0 || w.Latest.Before(w.Earliest) || w.Latest.Sub(w.Earliest) > 24*time.Hour || w.Latest.Year() > 9999 {
		return ErrUntrusted
	}
	return nil
}

// Provider must return an interval from independently authenticated time, not
// from time.Now with an asserted error. Callers commission its implementation.
type Provider interface{ Now() (Interval, error) }

type Reading struct {
	BootID  string
	Elapsed time.Duration
}

// VerifiedAnchor is an explicit trust injection seam. The acquiring platform
// must verify its signed, nonce-bound evidence and associate its interval with
// the supplied suspend-inclusive boot-monotonic reading. This package cannot
// establish authenticity merely by constructing this Go value.
type VerifiedAnchor struct {
	Interval   Interval
	EvidenceID string
}
type HoldoverConfig struct {
	Anchor      VerifiedAnchor
	Read        func() (Reading, error)
	MaxHoldover time.Duration
	DriftPPM    uint64
	MaxWidth    time.Duration
}
type Holdover struct {
	mu     sync.Mutex
	cfg    HoldoverConfig
	last   time.Duration
	failed bool
}

func NewHoldover(cfg HoldoverConfig) (*Holdover, error) {
	if cfg.Anchor.Interval.Validate() != nil || cfg.Anchor.EvidenceID == "" || len(cfg.Anchor.EvidenceID) > 256 || cfg.Read == nil || cfg.MaxHoldover <= 0 || cfg.MaxHoldover > 7*24*time.Hour || cfg.DriftPPM == 0 || cfg.DriftPPM > 1_000_000 || cfg.MaxWidth <= 0 || cfg.MaxWidth > 24*time.Hour || cfg.Anchor.Interval.Latest.Sub(cfg.Anchor.Interval.Earliest) > cfg.MaxWidth {
		return nil, ErrUntrusted
	}
	h := &Holdover{cfg: cfg, last: cfg.Anchor.Interval.Monotonic}
	if _, err := h.Now(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Holdover) Now() (Interval, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		return Interval{}, ErrUntrusted
	}
	r, err := h.cfg.Read()
	a := h.cfg.Anchor.Interval
	if err != nil {
		return Interval{}, ErrUntrusted
	}
	if r.BootID != a.BootID || r.Elapsed < h.last || r.Elapsed < a.Monotonic || r.Elapsed-a.Monotonic > h.cfg.MaxHoldover {
		h.failed = true
		return Interval{}, ErrUntrusted
	}
	h.last = r.Elapsed
	elapsed := r.Elapsed - a.Monotonic
	// Ceiling division avoids understating drift, without overflowing elapsed*PPM.
	ns := uint64(elapsed)
	drift := time.Duration((ns/1_000_000)*h.cfg.DriftPPM + (ns%1_000_000*h.cfg.DriftPPM+999_999)/1_000_000)
	w := a
	w.Monotonic = r.Elapsed
	w.Earliest = a.Earliest.Add(elapsed - drift)
	w.Latest = a.Latest.Add(elapsed + drift)
	if w.Validate() != nil || w.Latest.Sub(w.Earliest) > h.cfg.MaxWidth {
		h.failed = true
		return Interval{}, ErrUntrusted
	}
	return w, nil
}
