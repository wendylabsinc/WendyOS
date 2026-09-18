package robotwizard

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	// Status is what the source can say about each joint beside where it is:
	// whether a person could move it by hand right now, and whatever it
	// measured while deciding. A joint with no entry is one the source had
	// nothing to say about, which is the ordinary case for a source that reads
	// positions and nothing else.
	Status map[string]JointStatus
}

// Mobility is a source's answer to one question: can a person move this joint
// by hand right now?
//
// It is deliberately not a vendor state word. A unitree_hg robot answers with
// motor mode 1; a Feetech servo bus answers with torque-enable register 40; a
// brake answers with a solenoid. The wizard needs the answer, not the encoding,
// so each backend translates its own robot into this and keeps its vocabulary
// to itself. A wizard that learned "mode" would be a wizard that knows which
// robot it is driving.
//
// The zero value is MobilityUnknown on purpose. A source that cannot tell says
// nothing, and a hand sweep then proceeds exactly as it did before this
// existed: the checks that use this refuse on evidence, never on the absence of
// it.
type Mobility int

const (
	// MobilityUnknown is a source that cannot tell. sensor_msgs/JointState
	// carries positions and no torque state, so this is what that reports.
	MobilityUnknown Mobility = iota
	// MobilityFree is a joint a person can move: nothing is driving it.
	MobilityFree
	// MobilityHeld is a joint something is holding against the operator. It is
	// not "absent" — a joint that is not fitted at all is missing from the
	// reading entirely, and the two failures read very differently to whoever
	// is standing in front of the robot.
	MobilityHeld
)

func (m Mobility) String() string {
	switch m {
	case MobilityFree:
		return "free"
	case MobilityHeld:
		return "held"
	default:
		return "unknown"
	}
}

// MarshalJSON writes the word rather than the number, because a stored
// calibration record is read by people and an integer here would need this
// file to decode.
func (m Mobility) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON reads a record an earlier run wrote, so a resumed sweep
// restores what it knew. An unrecognised word is unknown rather than an error:
// a record written by a later version must not make this one unable to read its
// own session.
func (m *Mobility) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"free"`:
		*m = MobilityFree
	case `"held"`:
		*m = MobilityHeld
	default:
		*m = MobilityUnknown
	}
	return nil
}

// JointStatus is what a source can say about one joint beside where it is.
//
// Every field is optional, and a source fills in what it actually measured.
// Nothing here is derived: a backend that does not know a joint's temperature
// leaves it out rather than reporting a zero, for the same reason an unfitted
// joint is absent rather than at zero radians.
type JointStatus struct {
	Mobility Mobility `json:"mobility,omitempty"`
	// HoldReason is why the joint is not free, in the source's own words — a
	// phrase that completes "<joint> is …". "still energised (motor mode 1)"
	// on a unitree_hg robot; "torque-enabled" on a servo bus. The wizard prints
	// it and never parses it.
	HoldReason string `json:"hold_reason,omitempty"`
	// HoldRemedy is what the operator should do about it, again in the
	// source's words, because freeing a robot's joints is a per-robot
	// procedure and the wizard core knows no robot.
	HoldRemedy string `json:"hold_remedy,omitempty"`
	// TemperaturesC is every temperature sensor this joint reports, in the
	// source's own order. More than one because a motor that reports its
	// winding and its driver board separately is hiding whichever runs hotter
	// the moment something averages them.
	TemperaturesC []float64 `json:"temperatures_c,omitempty"`
	// Volts is the joint's supply, nil when the source reports none. A pointer
	// because zero volts is a reading — it is what an unfitted slot reports —
	// and "not measured" must not arrive looking like it.
	Volts *float64 `json:"volts,omitempty"`
	// Vendor carries the source's own state words verbatim, uninterpreted:
	// unitree_hg's motor mode is the one this exists for. Nothing switches on
	// them. They are here so that a person diagnosing a robot can see what the
	// robot actually said — an hour of live-robot time went into re-deriving
	// exactly these numbers once, because the joint source had dropped them.
	Vendor map[string]string `json:"vendor,omitempty"`
}

// Summary is the one line a person sees about a joint's condition, or "" when
// the source measured nothing worth showing.
func (s JointStatus) Summary() string {
	var parts []string
	switch s.Mobility {
	case MobilityFree:
		parts = append(parts, "free to move")
	case MobilityHeld:
		held := "held"
		if s.HoldReason != "" {
			held += " — " + s.HoldReason
		}
		parts = append(parts, held)
	}
	if len(s.TemperaturesC) > 0 {
		temps := make([]string, 0, len(s.TemperaturesC))
		for _, c := range s.TemperaturesC {
			temps = append(temps, fmt.Sprintf("%.4g", c))
		}
		parts = append(parts, strings.Join(temps, "/")+" °C")
	}
	if s.Volts != nil {
		parts = append(parts, fmt.Sprintf("%.4g V", *s.Volts))
	}
	for _, key := range sortedKeys(s.Vendor) {
		parts = append(parts, fmt.Sprintf("%s %s", key, s.Vendor[key]))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
//
// It takes the whole joint contract rather than only the source spec because
// not every robot's wire format carries joint names. A Unitree humanoid
// publishes a positionally indexed array and nothing else, so the only thing
// that can say which joint index 22 is, is the profile's own order — which is
// exactly why the sweep checks that order rather than trusting it.
type JointSourceOpener func(ctx context.Context, joints robotcal.Joints) (JointSource, error)

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
	min   map[string]float64
	max   map[string]float64
	first map[string]float64
	last  map[string]float64
	// status is the newest condition the source reported for each joint. The
	// newest rather than the first, because a motor that warmed up or was
	// energised part-way through a sweep is exactly what somebody reading the
	// record afterwards needs to see.
	status   map[string]JointStatus
	samples  int
	order    []string
	orderSet bool
}

// NewSampleWindow returns an empty window.
func NewSampleWindow() *SampleWindow {
	return &SampleWindow{
		min:    make(map[string]float64),
		max:    make(map[string]float64),
		first:  make(map[string]float64),
		last:   make(map[string]float64),
		status: make(map[string]JointStatus),
	}
}

// Add folds one reading into the window.
func (w *SampleWindow) Add(r JointReading) {
	if !w.orderSet && len(r.Order) > 0 {
		w.order = append([]string(nil), r.Order...)
		w.orderSet = true
	}
	for name, st := range r.Status {
		w.status[name] = st
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

// Status is the last condition the source reported for a joint during the
// window, and whether it reported one at all.
func (w *SampleWindow) Status(name string) (JointStatus, bool) {
	st, ok := w.status[name]
	return st, ok
}

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
