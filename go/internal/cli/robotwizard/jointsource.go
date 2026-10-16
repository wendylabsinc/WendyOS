package robotwizard

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// JointReading is one observation of where a robot's joints are.
//
// Order is carried beside Positions because it is the thing being checked. A
// policy that drives joints by index needs the robot's array to mean what it
// meant during training; a source that reports names in its own order is the
// only evidence available that it still does.
type JointReading struct {
	At        time.Time
	Positions map[string]float64
	Order     []string
}

// JointSource reads live joint positions. It is read-only by construction:
// there is no method here that could command anything, which is what makes a
// nothing-powered procedure nothing-powered.
type JointSource interface {
	// Read returns the most recent observation. It blocks until one is
	// available or ctx is done.
	Read(ctx context.Context) (JointReading, error)
	// Describe names the source, for preconditions and error messages.
	Describe() string
	Close() error
}

// JointSourceOpener resolves the backend a profile selected. The set of
// backends is the platform's, exactly as the set of methods is: a profile names
// one and parameterises it, and cannot describe a new one.
type JointSourceOpener func(ctx context.Context, spec robotcal.JointSourceSpec) (JointSource, error)

// UnsupportedBackendError is the refusal a profile gets when it selects a
// backend this build cannot open. It names what would be needed, because the
// operator's next question is always "so what do I do".
type UnsupportedBackendError struct {
	Backend   string
	Available []string
	Detail    string
}

func (e *UnsupportedBackendError) Error() string {
	msg := fmt.Sprintf("no joint source backend %q in this build", e.Backend)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if len(e.Available) > 0 {
		sorted := append([]string(nil), e.Available...)
		sort.Strings(sorted)
		msg += fmt.Sprintf(" (this build can open: %v)", sorted)
	}
	return msg
}

// SampleWindow collects readings for as long as an operator is moving a joint,
// and keeps the extremes.
//
// It tracks every joint the source reports, not only the one being swept. That
// is what makes the joint-map check free: if the operator was asked to move one
// joint and a different index moved, the sweep already has the evidence.
type SampleWindow struct {
	min      map[string]float64
	max      map[string]float64
	first    map[string]float64
	last     map[string]float64
	samples  int
	order    []string
	orderSet bool
}

// NewSampleWindow returns an empty window.
func NewSampleWindow() *SampleWindow {
	return &SampleWindow{
		min:   make(map[string]float64),
		max:   make(map[string]float64),
		first: make(map[string]float64),
		last:  make(map[string]float64),
	}
}

// Add folds one reading into the window.
func (w *SampleWindow) Add(r JointReading) {
	if !w.orderSet && len(r.Order) > 0 {
		w.order = append([]string(nil), r.Order...)
		w.orderSet = true
	}
	for name, v := range r.Positions {
		if _, seen := w.min[name]; !seen {
			w.min[name], w.max[name], w.first[name] = v, v, v
		}
		if v < w.min[name] {
			w.min[name] = v
		}
		if v > w.max[name] {
			w.max[name] = v
		}
		w.last[name] = v
	}
	w.samples++
}

// Samples is how many readings the window folded in. A step that collected one
// sample measured nothing, however wide its extremes look.
func (w *SampleWindow) Samples() int { return w.samples }

// Order is the joint order the source reported, if it reported one.
func (w *SampleWindow) Order() []string { return w.order }

// Span returns the extremes seen for a joint.
func (w *SampleWindow) Span(name string) (robotcal.Span, bool) {
	lo, ok := w.min[name]
	if !ok {
		return robotcal.Span{}, false
	}
	return robotcal.Span{Min: lo, Max: w.max[name]}, true
}

// Travel is how far a joint moved during the window.
func (w *SampleWindow) Travel(name string) float64 {
	span, ok := w.Span(name)
	if !ok {
		return 0
	}
	return span.Travel()
}

// DirectionSign is which way a joint's reported value went between the first
// extreme the operator held and the last: +1 when it increased, -1 when it
// decreased, 0 when it did not move.
//
// That is a deliberately modest claim. It says how this joint's number responds
// to the sweep the operator just performed, which is enough to catch a joint
// mounted reversed relative to the profile — the SO-101's drive_mode problem —
// without the platform pretending to know which physical direction "positive"
// is on a robot it has never seen.
func (w *SampleWindow) DirectionSign(name string) int {
	first, ok := w.first[name]
	if !ok {
		return 0
	}
	switch delta := w.last[name] - first; {
	case delta > 0:
		return 1
	case delta < 0:
		return -1
	default:
		return 0
	}
}

// MostMoved names the joint that travelled furthest in the window, and by how
// much. Used for the joint-map check: the operator was asked to move exactly
// one joint, so the answer should be that joint.
func (w *SampleWindow) MostMoved() (string, float64) {
	var best string
	var bestTravel float64
	names := make([]string, 0, len(w.min))
	for name := range w.min {
		names = append(names, name)
	}
	// Sorted so a tie resolves the same way every run, rather than by map order.
	sort.Strings(names)
	for _, name := range names {
		if t := w.Travel(name); t > bestTravel {
			best, bestTravel = name, t
		}
	}
	return best, bestTravel
}
