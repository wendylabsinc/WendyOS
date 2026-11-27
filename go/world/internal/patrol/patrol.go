// Package patrol schedules passive room observations within one admitted mission.
// All positions here use the navigation map's X/Y floor axes, metres, Z up.
package patrol

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"math"
	"sort"
	"time"
)

var ErrInterrupted = errors.New("patrol control interrupted operation")

type Point struct {
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	Yaw float64 `json:"yaw"`
}
type Room struct {
	ID                     string       `json:"id"`
	Name                   string       `json:"name"`
	Polygon                [][2]float64 `json:"polygon"`
	Candidates             []Point      `json:"candidates,omitempty"`
	CandidateClearance     float64      `json:"candidate_clearance_m,omitempty"`
	CandidateBlockedReason string       `json:"candidate_blocked_reason,omitempty"`
	LastObservedS          *float64     `json:"last_observed_s,omitempty"`
	RevisitSeconds         int          `json:"revisit_seconds,omitempty"`
	NextDueMS              int64        `json:"next_due_unix_ms,omitempty"`
	Visits                 int          `json:"visits"`
	BlockedUntilMS         int64        `json:"blocked_until_unix_ms,omitempty"`
	BlockedReason          string       `json:"blocked_reason,omitempty"`
	Evidence               []string     `json:"evidence"`
}
type Finding struct {
	ID       string   `json:"id"`
	TimeS    float64  `json:"time_s"`
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"`
}
type State struct {
	Tour         *TourState                `json:"tour,omitempty"`
	WorldWatch   *patrolwatch.Ledger       `json:"world_watch,omitempty"`
	Environment  *patrolenvironment.Ledger `json:"environment,omitempty"`
	Work         *patrolwork.Ledger        `json:"work,omitempty"`
	Return       *ReturnState              `json:"return,omitempty"`
	Schema       string                    `json:"schema"`
	MissionID    string                    `json:"mission_id"`
	State        string                    `json:"state"`
	CurrentRoom  string                    `json:"current_room,omitempty"`
	Destination  *Point                    `json:"destination,omitempty"`
	ExpiresAtMS  int64                     `json:"expires_at_unix_ms"`
	NominalSpeed float64                   `json:"nominal_speed_mps"`
	Rooms        []Room                    `json:"rooms"`
	Findings     []Finding                 `json:"recent_findings"`
	Reason       string                    `json:"reason,omitempty"`
}
type Navigation struct {
	LocalizationHealth *LocalizationHealth `json:"localization_health,omitempty"`
	InspectionMode     string              `json:"inspection_mode,omitempty"`
	PolicyDigest       string              `json:"policy_digest"`
	PolicyFrozen       bool                `json:"policy_frozen"`
	Stamp              float64             `json:"stamp"`
	Ready              bool                `json:"ready"`
	Pose               Point               `json:"pose"`
	Covariance         []float64           `json:"covariance"`
	Route              [][]float64         `json:"route"`
	Speed              float64             `json:"speed_mps"`
	AngularSpeed       float64             `json:"angular_speed_rps"`
	SensorAge          float64             `json:"sensor_age_s"`
	Goal               struct {
		ID       string  `json:"id"`
		State    string  `json:"state"`
		Distance float64 `json:"distance_remaining"`
	} `json:"goal"`
}
type Route struct {
	Reachable bool        `json:"reachable"`
	Length    float64     `json:"length"`
	Route     [][]float64 `json:"route"`
}
type Capture struct {
	ManifestSHA256 string
	SampleCount    int
	TimeS          float64
	Evidence       []string
	Findings       []Finding
}
type Adapter interface {
	Plan(Point) (Route, error)
	Goal(string, Point) error
	Cancel() error
	Capture() (Capture, error)
}
type selection struct {
	order           []int
	room, candidate int
	bestIndex       int
	bestLength      float64
	best            Point
}

type Engine struct {
	tourFirstStamp, tourLastStamp     float64
	tourLastReceiptMS                 int64
	returnLastReceiptMS               int64
	returnFirstStamp, returnLastStamp float64
	returnSettledMS                   int64
	State                             State
	adapter                           Adapter
	progressAt, settledAt             int64
	bestDistance                      float64
	goalNumber                        uint64
	goalID                            string
	planning                          *selection
	scheduled                         bool
}

func New(id string, expiresMS int64, rooms []Room, adapter Adapter) (*Engine, error) {
	if id == "" || expiresMS <= 0 || len(rooms) == 0 || adapter == nil {
		return nil, errors.New("mission and permitted rooms required")
	}
	seen := map[string]bool{}
	for _, r := range rooms {
		if r.ID == "" || seen[r.ID] || len(r.Polygon) < 3 {
			return nil, errors.New("invalid room geometry")
		}
		seen[r.ID] = true
	}
	return &Engine{State: State{Schema: "wendy.nav.patrol.status.v1", MissionID: id, State: "waiting", ExpiresAtMS: expiresMS, NominalSpeed: .55, Rooms: rooms, Findings: []Finding{}}, adapter: adapter}, nil
}
func (e *Engine) Terminal() bool {
	return e.State.State == "stopped" || e.State.State == "expired" || e.State.State == "failed" || e.State.State == "completed"
}
func (e *Engine) Control(action string, nowMS int64) error {
	if e.Terminal() {
		return errors.New("mission has ended")
	}
	if nowMS >= e.State.ExpiresAtMS {
		e.State.State = "expired"
		return e.adapter.Cancel()
	}
	if action == "recall" {
		if !e.WorldMotionReady() {
			return errors.New("World evidence held; compatible evidence and explicit Resume required")
		}
		if !e.EnvironmentMotionReady() {
			return errors.New("local environment held; clear interval and explicit Resume required")
		}
		if e.State.Work != nil && (!e.State.Work.Valid || e.State.State == "work_held") {
			return errors.New("work allowance held; explicit recovery or a new mission required")
		}
		return e.recall(nowMS, "operator")
	}
	switch action {
	case "pause":
		e.State.State = "paused"
		e.planning = nil
		e.settledAt = 0
		return e.adapter.Cancel()
	case "resume":
		if l := e.State.WorldWatch; l != nil {
			l.Check(time.UnixMilli(nowMS))
			if !l.Ready || l.Permanent {
				return errors.New(l.Reason)
			}
		}
		if e.State.Environment != nil {
			e.State.Environment.Check(nowMS)
			if !e.State.Environment.Ready || e.State.Environment.Permanent {
				return errors.New(e.State.Environment.Reason)
			}
		}
		if e.State.Work != nil && !e.State.Work.Valid {
			return errors.New("work allowance unavailable or exhausted")
		}
		if e.State.State != "paused" && e.State.State != "return_held" && e.State.State != "work_held" && e.State.State != "environment_held" && e.State.State != "world_held" && e.State.State != "tour_held" {
			return errors.New("mission is not paused")
		}
		if e.State.Environment != nil {
			if err := e.State.Environment.Resume(nowMS); err != nil {
				return err
			}
		}
		if l := e.State.WorldWatch; l != nil {
			if err := l.Resume(time.UnixMilli(nowMS)); err != nil {
				return err
			}
		}
		e.State.State = "waiting"
		if e.State.Return != nil && e.State.Return.Requested {
			e.State.State = "return_planning"
		}
		e.State.Reason = ""
		return nil
	case "stop":
		e.State.State = "stopped"
		return e.adapter.Cancel()
	default:
		return errors.New("unknown patrol control")
	}
}
func (e *Engine) fail(err error) error {
	e.State.State = "failed"
	e.State.Reason = err.Error()
	_ = e.adapter.Cancel()
	return err
}
func (e *Engine) deferRoom(now int64, reason string) error {
	if e.State.Tour != nil {
		return e.holdTour("Inspection incomplete: " + reason + ". Resume to retry the same signed stop.")
	}
	for i := range e.State.Rooms {
		r := &e.State.Rooms[i]
		if r.ID == e.State.CurrentRoom {
			r.BlockedUntilMS = now + 30000
			r.BlockedReason = reason
		}
	}
	e.State.CurrentRoom = ""
	e.State.Destination = nil
	e.State.State = "waiting"
	e.settledAt = 0
	return e.adapter.Cancel()
}
func (e *Engine) Tick(nowMS int64, nav Navigation) error {
	if e.Terminal() {
		return nil
	}
	if nowMS >= e.State.ExpiresAtMS {
		e.State.State = "expired"
		return e.adapter.Cancel()
	}
	if e.tickWorld(nowMS) {
		return nil
	}
	if e.tickEnvironment(nowMS) {
		return nil
	}
	if held, err := e.tickWork(nowMS); held || err != nil {
		return err
	}
	if e.State.State == "paused" {
		return nil
	}
	if r := e.State.Return; r != nil {
		if !r.Requested && r.BeforeExpirySeconds > 0 && nowMS >= r.BeginAtMS {
			if err := e.recall(nowMS, "scheduled"); err != nil {
				return err
			}
		}
		if r.Requested {
			return e.tickReturn(nowMS, nav)
		}
	}
	if e.State.Tour != nil && e.State.State == "tour_held" {
		return nil
	}
	if !nav.Ready || !finite(nav.SensorAge) || nav.SensorAge > 1 {
		if e.State.Tour != nil {
			return e.holdTour("Inspection held: fresh LiDAR, odometry and localization required. Resume explicitly.")
		}
		if e.State.State != "waiting" {
			if err := e.adapter.Cancel(); err != nil {
				return e.fail(err)
			}
		}
		e.State.State = "waiting"
		e.planning = nil
		e.State.Reason = "waiting for fresh sensors and localization"
		e.settledAt = 0
		return nil
	}
	e.State.Reason = ""
	if e.State.State == "waiting" || e.State.State == "blocked" || e.State.State == "planning" || e.State.State == "scheduled" {
		if e.State.Tour != nil {
			return e.chooseTour(nowMS)
		}
		if e.State.Destination != nil {
			return e.dispatch(nowMS, *e.State.Destination)
		}
		return e.choose(nowMS)
	}
	if e.State.State == "navigating" {
		if nav.Goal.ID != e.goalID {
			if nowMS-e.progressAt >= 15000 {
				return e.deferRoom(nowMS, "navigation goal unavailable")
			}
			return nil
		}
		if e.State.Tour != nil && (nav.Goal.State == "policy_hold" || nav.Goal.State == "sensor_hold" || nav.Goal.State == "executor_lost") {
			return e.deferRoom(nowMS, "route blocked: "+nav.Goal.State)
		}
		switch nav.Goal.State {
		case "succeeded":
			if e.State.Destination == nil || math.Hypot(nav.Pose.X-e.State.Destination.X, nav.Pose.Y-e.State.Destination.Y) > .4 {
				return e.deferRoom(nowMS, "goal result outside observation tolerance")
			}
			if err := e.adapter.Cancel(); err != nil {
				return e.fail(err)
			}
			e.State.State = "observing"
			e.settledAt = 0
			e.progressAt = nowMS
		case "aborted", "canceled", "rejected", "failed":
			return e.deferRoom(nowMS, "route blocked: "+nav.Goal.State)
		default:
			// Nav2 can report zero before its first path-length feedback.
			// Only positive distances establish progress; arrival uses succeeded.
			if nav.Goal.State == "navigating" && finite(nav.Goal.Distance) && nav.Goal.Distance > 0 && nav.Goal.Distance < e.bestDistance-.08 {
				e.bestDistance = nav.Goal.Distance
				e.progressAt = nowMS
			}
			if nowMS-e.progressAt >= 15000 {
				return e.deferRoom(nowMS, "no navigation progress for 15 seconds")
			}
		}
	}
	if e.State.State == "observing" {
		if e.State.Tour != nil && (!finite(nav.Pose.X) || !finite(nav.Pose.Y) || !finite(nav.Pose.Yaw)) {
			return e.holdTour("Inspection held: invalid observation pose")
		}
		if e.State.Destination == nil || math.Hypot(nav.Pose.X-e.State.Destination.X, nav.Pose.Y-e.State.Destination.Y) > .4 {
			return e.deferRoom(nowMS, "moved outside observation tolerance")
		}
		for _, r := range e.State.Rooms {
			if r.ID == e.State.CurrentRoom && !inside(r.Polygon, nav.Pose) {
				return e.deferRoom(nowMS, "left observation room")
			}
		}
		if e.State.Tour != nil && (!finite(nav.Pose.Yaw) || math.Abs(math.Atan2(math.Sin(nav.Pose.Yaw-e.State.Destination.Yaw), math.Cos(nav.Pose.Yaw-e.State.Destination.Yaw))) > .15) {
			e.settledAt = 0
			if nowMS-e.progressAt >= 15000 {
				return e.holdTour("Inspection incomplete: signed observation yaw not reached")
			}
			return nil
		}
		if !finite(nav.Speed) || !finite(nav.AngularSpeed) || math.Abs(nav.Speed) > .12 || math.Abs(nav.AngularSpeed) > .15 {
			e.settledAt = 0
			if nowMS-e.progressAt >= 15000 {
				return e.deferRoom(nowMS, "robot did not settle for observation")
			}
			return nil
		}
		if e.State.Tour != nil && !e.tourSettled(nowMS, nav) {
			return nil
		}
		if e.settledAt == 0 {
			e.settledAt = nowMS
			return nil
		}
		if nowMS-e.settledAt < 2000 {
			return nil
		}
		capture, err := e.adapter.Capture()
		if errors.Is(err, ErrInterrupted) {
			return nil
		}
		if err != nil {
			return e.deferRoom(nowMS, "sensor capture failed: "+err.Error())
		}
		if !finite(capture.TimeS) || len(capture.Evidence) < 3 {
			return e.deferRoom(nowMS, "fresh evidence incomplete")
		}
		if e.State.Tour != nil {
			if err := e.recordTourCapture(capture); err != nil {
				return e.holdTour(err.Error())
			}
		}
		for i := range e.State.Rooms {
			r := &e.State.Rooms[i]
			if r.ID == e.State.CurrentRoom {
				if r.LastObservedS != nil && capture.TimeS <= *r.LastObservedS {
					return e.deferRoom(nowMS, "capture did not advance")
				}
				r.Visits++
				if e.scheduled {
					r.NextDueMS = nowMS + int64(r.RevisitSeconds)*1000
				}
				r.LastObservedS = &capture.TimeS
				r.Evidence = append([]string(nil), capture.Evidence...)
				r.BlockedUntilMS = 0
				r.BlockedReason = ""
			}
		}
		e.State.Findings = append(e.State.Findings, capture.Findings...)
		if len(e.State.Findings) > 40 {
			e.State.Findings = e.State.Findings[len(e.State.Findings)-40:]
		}
		e.State.State = "waiting"
		e.State.CurrentRoom = ""
		e.State.Destination = nil
		e.settledAt = 0
	}
	return nil
}
func (e *Engine) dispatch(now int64, p Point) error {
	e.goalNumber++
	e.goalID = e.State.MissionID + "/" + itoa(e.goalNumber)
	if err := e.adapter.Goal(e.goalID, p); err != nil {
		if errors.Is(err, ErrInterrupted) {
			e.State.Destination = &p
			return nil
		}
		return e.deferRoom(now, "goal refused: "+err.Error())
	}
	e.State.Destination = &p
	e.State.State = "navigating"
	e.progressAt = now
	e.bestDistance = math.Inf(1)
	return nil
}
func (e *Engine) choose(now int64) error {
	age := func(r Room) float64 {
		if e.scheduled {
			return float64(r.NextDueMS)
		}
		if r.LastObservedS == nil {
			return -1
		}
		return *r.LastObservedS
	}
	if e.planning == nil {
		order := []int{}
		for i, r := range e.State.Rooms {
			if r.BlockedUntilMS <= now && (!e.scheduled || r.NextDueMS <= now) {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(i, j int) bool { return age(e.State.Rooms[order[i]]) < age(e.State.Rooms[order[j]]) })
		e.planning = &selection{order: order, bestIndex: -1, bestLength: math.Inf(1)}
		e.State.State = "planning"
	}
	plan := e.planning
	// One bounded route request per tick keeps expiry and operator controls live.
	// Choose the first reachable observation position for each equally old room,
	// then break room-priority ties by their actual planned route lengths.
	if plan.room < len(plan.order) {
		i := plan.order[plan.room]
		r := &e.State.Rooms[i]
		if plan.bestIndex >= 0 && age(*r) > age(e.State.Rooms[plan.bestIndex]) {
			plan.room = len(plan.order)
		} else {
			if plan.candidate >= len(r.Candidates) {
				r.BlockedUntilMS = now + 30000
				r.BlockedReason = "no reachable observation position in current costmap"
				if len(r.Candidates) == 0 && r.CandidateBlockedReason != "" {
					r.BlockedReason = r.CandidateBlockedReason
				}
				plan.room++
				plan.candidate = 0
			} else {
				p := r.Candidates[plan.candidate]
				plan.candidate++
				if finite(p.X) && finite(p.Y) && finite(p.Yaw) && inside(r.Polygon, p) {
					route, err := e.adapter.Plan(p)
					if err != nil {
						r.BlockedUntilMS = now + 30000
						r.BlockedReason = "planner unavailable: " + err.Error()
						plan.room++
						plan.candidate = 0
						return nil
					}
					if route.Reachable && finite(route.Length) && route.Length >= 0 {
						if route.Length < plan.bestLength {
							plan.bestLength = route.Length
							plan.best = p
							plan.bestIndex = i
						}
						plan.room++
						plan.candidate = 0
					}
				}
			}
			return nil
		}
	}
	e.planning = nil
	if plan.bestIndex < 0 {
		e.State.State = "blocked"
		e.State.Reason = "all permitted rooms temporarily blocked"
		if e.scheduled {
			for _, r := range e.State.Rooms {
				if r.BlockedUntilMS <= now && r.NextDueMS > now {
					e.State.State = "scheduled"
					e.State.Reason = "waiting for the next room observation deadline"
					break
				}
			}
		}
		return nil
	}
	e.State.CurrentRoom = e.State.Rooms[plan.bestIndex].ID
	return e.dispatch(now, plan.best)
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func inside(poly [][2]float64, p Point) bool {
	yes := false
	j := len(poly) - 1
	for i, v := range poly {
		w := poly[j]
		if (v[1] > p.Y) != (w[1] > p.Y) && p.X < (w[0]-v[0])*(p.Y-v[1])/(w[1]-v[1])+v[0] {
			yes = !yes
		}
		j = i
	}
	return yes
}
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 20)
	for n > 0 {
		b = append(b, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
