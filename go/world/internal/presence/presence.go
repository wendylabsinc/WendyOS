// Package presence interprets anonymous planar tracks in configured site zones.
// Events are historical inferences, never evidence of clearance or person identity.
package presence

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Retention = time.Minute
const MaxEvents = 128

type Zone struct {
	ID           string     `json:"id"`
	Label        string     `json:"label"`
	MinXZ        [2]float64 `json:"min_xz"`
	MaxXZ        [2]float64 `json:"max_xz"`
	DwellSeconds float64    `json:"dwell_seconds"`
}
type Occupant struct {
	TrackID      string               `json:"track_id"`
	State        string               `json:"state"`
	LastObserved time.Time            `json:"last_observed"`
	Since        time.Time            `json:"since"`
	DwellSeconds float64              `json:"dwell_seconds"`
	ValidUntil   time.Time            `json:"valid_until"`
	Inputs       []tracking.Detection `json:"inputs"`
}
type ZoneState struct {
	Zone      Zone       `json:"zone"`
	State     string     `json:"state"`    // observed, uncertain or unknown; never empty/clear
	Observed  int        `json:"observed"` // Lower bound from current unambiguous tracks.
	Possible  int        `json:"possible"`
	Occupants []Occupant `json:"occupants"`
}
type Event struct {
	Sequence     uint64               `json:"sequence"`
	Kind         string               `json:"kind"` // entered, dwell, uncertain, exited, lost
	ZoneID       string               `json:"zone_id"`
	TrackID      string               `json:"track_id"`
	At           time.Time            `json:"at"`
	DwellSeconds float64              `json:"dwell_seconds"`
	Reason       string               `json:"reason"`
	ValidUntil   time.Time            `json:"valid_until"`
	Inputs       []tracking.Detection `json:"inputs"`
}
type Output struct {
	Zones            []ZoneState `json:"zones"`
	Events           []Event     `json:"events"`
	RetentionSeconds float64     `json:"retention_seconds"`
}
type visit struct {
	since, last, until time.Time
	dwell, paused      bool
	inputs             []tracking.Detection
}
type Engine struct {
	zones      []Zone
	run, clock string
	frame      world.FrameRef
	last       time.Time
	visits     map[string]map[string]visit
	events     []Event
	sequence   uint64
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func New(zones []Zone, run, clock string, frame world.FrameRef) (*Engine, error) {
	if len(zones) == 0 || len(zones) > 16 || run == "" || clock == "" || frame.Validate() != nil {
		return nil, errors.New("presence requires bounded zones and explicit run, clock and frame")
	}
	copyZones := append([]Zone(nil), zones...)
	sort.Slice(copyZones, func(i, j int) bool { return copyZones[i].ID < copyZones[j].ID })
	for i, z := range copyZones {
		if z.ID == "" || len(z.ID) > 80 || len(z.Label) > 120 || i > 0 && copyZones[i-1].ID == z.ID || !finite(z.DwellSeconds) || z.DwellSeconds <= 0 || z.DwellSeconds > 3600 {
			return nil, errors.New("invalid zone identity or dwell threshold")
		}
		for a := 0; a < 2; a++ {
			if !finite(z.MinXZ[a]) || !finite(z.MaxXZ[a]) || z.MinXZ[a] >= z.MaxXZ[a] || math.Abs(z.MinXZ[a]) > 10000 || math.Abs(z.MaxXZ[a]) > 10000 {
				return nil, errors.New("invalid zone rectangle")
			}
		}
	}
	return &Engine{zones: copyZones, run: run, clock: clock, frame: frame, visits: map[string]map[string]visit{}, events: []Event{}}, nil
}

// Matches allows omission after configuration; an existing session never changes zones.
func (e *Engine) Matches(zones []Zone) bool {
	if len(zones) == 0 {
		return true
	}
	other, err := New(zones, e.run, e.clock, e.frame)
	return err == nil && reflect.DeepEqual(other.zones, e.zones)
}
func inputs(in []tracking.Detection) []tracking.Detection {
	out := append([]tracking.Detection(nil), in...)
	for i := range out {
		out[i].Evidence = append(out[i].Evidence[:0:0], out[i].Evidence...)
	}
	return out
}

// Classify uses the axis projections of a 2-sigma covariance ellipse. This is
// deliberately conservative at rectangle edges; it is not a calibrated confidence.
func classify(z Zone, t tracking.Track) string {
	inside := true
	for a := 0; a < 2; a++ {
		radius := 2 * math.Sqrt(t.Covariance[a*3])
		low, high := t.PositionXZ[a]-radius, t.PositionXZ[a]+radius
		if high < z.MinXZ[a] || low > z.MaxXZ[a] {
			return "outside"
		}
		inside = inside && low > z.MinXZ[a] && high < z.MaxXZ[a]
	}
	if inside && !t.Ambiguous {
		return "inside"
	}
	return "uncertain"
}

// addInside distinguishes historical inside evidence from its extrapolated
// current estimate. Prediction disagreement cannot establish current occupancy.
func addInside(state *ZoneState, t tracking.Track, v visit, currentRelation string) {
	kind := "inside"
	if currentRelation == "inside" {
		state.Observed++
	} else {
		state.Possible++
		kind = "uncertain"
	}
	state.Occupants = append(state.Occupants, Occupant{TrackID: t.ID, State: kind, LastObserved: v.last, Since: v.since, DwellSeconds: v.last.Sub(v.since).Seconds(), ValidUntil: v.until, Inputs: inputs(v.inputs)})
}
func validEstimate(p [2]float64, c [4]float64) bool {
	determinant := c[0]*c[3] - c[1]*c[2]
	return finite(p[0]) && finite(p[1]) && finite(c[0]) && finite(c[1]) && finite(c[2]) && finite(c[3]) && c[0] > 0 && c[3] > 0 && math.Abs(c[1]-c[2]) <= 1e-9 && finite(determinant) && determinant > 0
}
func (e *Engine) emit(kind string, z Zone, id string, at time.Time, v visit, reason string) {
	e.sequence++
	e.events = append(e.events, Event{Sequence: e.sequence, Kind: kind, ZoneID: z.ID, TrackID: id, At: at, DwellSeconds: math.Max(0, v.last.Sub(v.since).Seconds()), Reason: reason, ValidUntil: v.until, Inputs: inputs(v.inputs)})
}

// Step requires the complete current tracker output. Invalid batches do not
// mutate state. Missing, coasting, ambiguous or boundary-overlapping tracks break
// continuous dwell. Historical transitions use the measurement-updated estimate at
// LastObserved, never the extrapolated current position at Time. Only fresh,
// same-track outside evidence establishes exit; prediction alone remains uncertain.
func (e *Engine) Step(now time.Time, tracks []tracking.Track) (Output, error) {
	out := Output{Zones: []ZoneState{}, Events: []Event{}, RetentionSeconds: Retention.Seconds()}
	if now.IsZero() || !e.last.IsZero() && now.Before(e.last) || len(tracks) > 256 {
		return out, errors.New("invalid presence time or track bound")
	}
	seen := map[string]bool{}
	for _, t := range tracks {
		if t.ID == "" || len(t.ID) > 256 || seen[t.ID] || t.RunID != e.run || t.ClockDomain != e.clock || t.Frame != e.frame || t.LastObserved.IsZero() || t.LastObserved.After(now) || !t.ValidUntil.After(t.LastObserved) || !t.Time.Equal(now) || len(t.Members) == 0 || len(t.Members) > 64 || !validEstimate(t.PositionXZ, t.Covariance) || !validEstimate(t.ObservedPositionXZ, t.ObservedCovariance) || t.State != "observed" && t.State != "coasting" {
			return out, errors.New("invalid track or presence scope")
		}
		seen[t.ID] = true
		for _, m := range t.Members {
			if m.RunID != e.run || m.ClockDomain != e.clock || m.Frame != e.frame || m.SampleID == "" || m.SampleTime.After(t.LastObserved) || t.ValidUntil.After(m.ValidUntil) {
				return out, errors.New("unbound presence evidence")
			}
		}
	}
	ordered := append([]tracking.Track(nil), tracks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, z := range e.zones {
		previous := e.visits[z.ID]
		current := map[string]visit{}
		handled := map[string]bool{}
		state := ZoneState{Zone: z, State: "unknown", Occupants: []Occupant{}}
		for _, t := range ordered {
			old, had := previous[t.ID]
			fresh := t.State == "observed" && now.Before(t.ValidUntil)
			currentRelation := classify(z, t)
			observation := t
			observation.PositionXZ, observation.Covariance = t.ObservedPositionXZ, t.ObservedCovariance
			relation := classify(z, observation)
			if !fresh {
				relation = currentRelation
			}
			if relation == "inside" && (!fresh || t.Ambiguous) {
				relation = "uncertain"
			}
			if relation == "outside" {
				if had && fresh && !t.Ambiguous && t.LastObserved.After(old.last) && t.LastObserved.Before(old.until) {
					v := old
					v.inputs = inputs(t.Members)
					v.until = t.ValidUntil
					e.emit("exited", z, t.ID, t.LastObserved, v, "fresh same-track uncertainty lies outside zone")
					handled[t.ID] = true
				}
				if fresh && now.Before(t.ValidUntil) && currentRelation != "outside" {
					state.Possible++
					state.Occupants = append(state.Occupants, Occupant{TrackID: t.ID, State: "uncertain", LastObserved: t.LastObserved, ValidUntil: t.ValidUntil, Inputs: inputs(t.Members)})
				}
				continue
			}
			if relation == "inside" {
				continuous := had && !old.paused && t.LastObserved.After(old.last) && t.LastObserved.Before(old.until)
				// A repeated observed batch cannot extend dwell or re-enter a visit.
				if had && !old.paused && t.LastObserved.Equal(old.last) {
					current[t.ID] = old
					addInside(&state, t, old, currentRelation)
					handled[t.ID] = true
					continue
				}
				v := visit{since: t.LastObserved, last: t.LastObserved, until: t.ValidUntil, inputs: inputs(t.Members)}
				if continuous {
					v.since = old.since
					v.dwell = old.dwell
				} else {
					if had && !old.paused {
						e.emit("lost", z, t.ID, now, old, "observation continuity ended")
					}
					e.emit("entered", z, t.ID, t.LastObserved, v, "fresh unambiguous uncertainty lies inside zone")
				}
				if !v.dwell && v.last.Sub(v.since).Seconds() >= z.DwellSeconds {
					v.dwell = true
					e.emit("dwell", z, t.ID, t.LastObserved, v, "continuous observed dwell reached configured threshold")
				}
				current[t.ID] = v
				handled[t.ID] = true
				addInside(&state, t, v, currentRelation)
			} else if now.Before(t.ValidUntil) {
				// Boundary uncertainty pauses dwell but preserves a fresh,
				// unambiguous same-track transition for a later observed exit.
				if had && fresh && !t.Ambiguous && t.LastObserved.Before(old.until) {
					old.until = t.ValidUntil
					old.inputs = inputs(t.Members)
					if !old.paused {
						e.emit("uncertain", z, t.ID, t.LastObserved, old, "uncertainty overlaps zone boundary; dwell paused")
					}
					old.paused = true
					current[t.ID] = old
					handled[t.ID] = true
				}
				state.Possible++
				state.Occupants = append(state.Occupants, Occupant{TrackID: t.ID, State: "uncertain", LastObserved: t.LastObserved, ValidUntil: t.ValidUntil, Inputs: inputs(t.Members)})
			}
		}
		ids := []string{}
		for id := range previous {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !handled[id] {
				e.emit("lost", z, id, now, previous[id], "missing, expired, coasting or uncertain evidence; no exit inferred")
			}
		}
		e.visits[z.ID] = current
		if state.Observed > 0 {
			state.State = "observed"
		}
		if state.Possible > 0 {
			state.State = "uncertain"
		}
		out.Zones = append(out.Zones, state)
	}
	retained := []Event{}
	for _, event := range e.events {
		if now.Sub(event.At) < Retention {
			retained = append(retained, event)
		}
	}
	if len(retained) > MaxEvents {
		retained = retained[len(retained)-MaxEvents:]
	}
	e.events = retained
	e.last = now
	for _, event := range retained {
		event.Inputs = inputs(event.Inputs)
		out.Events = append(out.Events, event)
	}
	return out, nil
}
