// Package mission provides an in-memory policy prototype, not a signature
// verifier, durable executor, or physical safety controller.
package mission

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

type SafetyClass string

const (
	Observe SafetyClass = "OBSERVE"
	Move    SafetyClass = "MOVE"
	Actuate SafetyClass = "ACTUATE"
)

type Mode string

const (
	Teleop     Mode = "teleop"
	Supervised Mode = "supervised"
	Autonomous Mode = "autonomous"
)

type Domain string

const (
	Simulation Domain = "simulation"
	Physical   Domain = "physical"
)

type Action string

const (
	Continue Action = "continue"
	Hold     Action = "hold"
	Return   Action = "return"
	Stop     Action = "stop"
)

type Phase string

const (
	Accepted  Phase = "accepted"
	Running   Phase = "running"
	Held      Phase = "held"
	Returning Phase = "returning"
	Succeeded Phase = "succeeded"
	Failed    Phase = "failed"
	Canceled  Phase = "canceled"
	Rejected  Phase = "rejected"
)

func (p Phase) Terminal() bool {
	return p == Succeeded || p == Failed || p == Canceled || p == Rejected
}

// Envelope is the immutable mission-local authorization scope. The adapter
// evaluates geometry and provides InsideArea; this package does not plan routes.
type Envelope struct {
	AreaID          string
	RequiredSensors []string
	MinimumEnergy   float64
	Fallback        Action
}

type Task struct {
	ID       string
	Type     string
	Assignee string
	Site     string
	Domain   Domain
	Class    SafetyClass
	Mode     Mode
	// Deadline is in this Engine's monotonic clock domain, not Unix time.
	Deadline time.Duration
	Envelope Envelope
}

// Digest binds all fields, including ordered sensor requirements. It is a local
// prototype encoding and must not be adopted as a production signature format.
func Digest(t Task) [32]byte { b, _ := json.Marshal(t); return sha256.Sum256(b) }

// Authority is input from a trusted authorization adapter. A user-controlled
// network request must never be deserialized directly into this type.
// Signature, grants, revocation, nonce, and authenticated wall-time checks are
// the adapter's responsibility before it constructs this value.
type Authority struct {
	TaskDigest    [32]byte
	Principal     string
	Operator      bool
	AllowAutonomy bool
	Site          string
	Assignee      string
	Domain        Domain
	ValidUntil    time.Duration
}

// StopAuthority represents an already verified stop, including its immutable
// target. It deliberately does not require a currently valid role grant.
// The verification adapter must still reject revoked identities and replays.
type StopAuthority struct {
	TaskID     string
	Principal  string
	Site       string
	Assignee   string
	Domain     Domain
	ValidUntil time.Duration
}

type Status struct {
	Version uint64
	Phase   Phase
	Reason  string
}

type Decision struct {
	Action Action
	Status Status
}

// Conditions are trusted local adapter outputs, not claims supplied by task
// issuers. Lease deadlines must come from authenticated bidirectional exchanges.
// Connectivity is intentionally not an autonomous mission precondition.
type Conditions struct {
	// Negative-only local facts. Neither flag establishes geometry or authority.
	// They use existing fallback rules, including separately checked ReturnSafe.
	ObservationInvalid bool
	LocalFault         bool
	InsideArea         bool
	ObstacleClear      bool
	Energy             float64
	Sensors            map[string]bool
	SupervisionUntil   time.Duration
	CommandUntil       time.Duration
	// ReturnSafe must cover the return route, local sensors and energy separately.
	ReturnSafe bool
}

type Config struct {
	Assignee string
	Site     string
	Domain   Domain
	Catalog  map[string]SafetyClass
	Now      func() time.Duration
	MaxLease time.Duration
}

type record struct {
	task       Task
	digest     [32]byte
	status     Status
	grantUntil time.Duration
}

// Engine is serialized and in-memory. Entries, including stops and terminal
// tasks, are never evicted. Production use requires durable admission and quotas.
type Engine struct {
	mu         sync.Mutex
	cfg        Config
	last       time.Duration
	clockFault bool
	records    map[string]*record
	stops      map[string]struct{}
}

func New(c Config) (*Engine, error) {
	if c.Assignee == "" || c.Site == "" || c.Now == nil || c.MaxLease <= 0 || (c.Domain != Simulation && c.Domain != Physical) {
		return nil, errors.New("invalid engine configuration")
	}
	catalog := make(map[string]SafetyClass, len(c.Catalog))
	for k, v := range c.Catalog {
		if k == "" || (v != Observe && v != Move && v != Actuate) {
			return nil, errors.New("invalid catalog entry")
		}
		catalog[k] = v
	}
	c.Catalog = catalog
	return &Engine{cfg: c, records: make(map[string]*record), stops: make(map[string]struct{})}, nil
}

func (e *Engine) now() (time.Duration, error) {
	if e.clockFault {
		return 0, errors.New("monotonic clock fault is latched")
	}
	now := e.cfg.Now()
	if now < 0 || now < e.last {
		e.clockFault = true
		for _, r := range e.records {
			if !r.status.Phase.Terminal() {
				e.transition(r, Failed, Stop, "monotonic clock invalid; outcome unknown")
			}
		}
		return 0, errors.New("monotonic clock moved backwards")
	}
	e.last = now
	return now, nil
}

func (e *Engine) Accept(t Task, a Authority) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now, err := e.now()
	if err != nil {
		return Status{}, err
	}
	if err := e.validate(t, a, now); err != nil {
		return Status{}, err
	}
	digest := Digest(t)
	if r, ok := e.records[t.ID]; ok {
		if digest != r.digest {
			return Status{}, errors.New("task ID already bound to different content")
		}
		return r.status, nil // duplicates cannot renew the original grant or deadline
	}
	if _, ok := e.stops[t.ID]; ok {
		return Status{}, errors.New("task was stopped before delivery")
	}
	t.Envelope.RequiredSensors = append([]string(nil), t.Envelope.RequiredSensors...)
	r := &record{task: t, digest: digest, grantUntil: a.ValidUntil, status: Status{Version: 1, Phase: Accepted}}
	e.records[t.ID] = r
	return r.status, nil
}

func (e *Engine) validate(t Task, a Authority, now time.Duration) error {
	if t.ID == "" || t.Type == "" || t.Site != e.cfg.Site || t.Assignee != e.cfg.Assignee || t.Domain != e.cfg.Domain {
		return errors.New("invalid task identity or scope")
	}
	class, ok := e.cfg.Catalog[t.Type]
	if !ok || class != t.Class {
		return errors.New("task class must match trusted catalog")
	}
	if class == Actuate {
		return errors.New("ACTUATE is not implemented")
	}
	if t.Mode != Teleop && t.Mode != Supervised && t.Mode != Autonomous {
		return errors.New("invalid execution mode")
	}
	if t.Deadline <= now || a.ValidUntil <= now {
		return errors.New("expired task or authority")
	}
	if a.TaskDigest != Digest(t) || a.Principal == "" || a.Site != t.Site || a.Assignee != t.Assignee || a.Domain != t.Domain {
		return errors.New("authority does not bind task and scope")
	}
	if class == Move && !a.Operator {
		return errors.New("motion requires operator authority")
	}
	if t.Mode == Autonomous && !a.AllowAutonomy {
		return errors.New("autonomy not authorized")
	}
	if t.Envelope.AreaID == "" || !finite(t.Envelope.MinimumEnergy) || t.Envelope.MinimumEnergy < 0 || t.Envelope.MinimumEnergy > 1 {
		return errors.New("invalid operating envelope")
	}
	if t.Envelope.Fallback != Hold && t.Envelope.Fallback != Return && t.Envelope.Fallback != Stop {
		return errors.New("invalid fallback")
	}
	if t.Envelope.Fallback == Return && class != Move {
		return errors.New("return fallback requires motion authority")
	}
	seen := map[string]bool{}
	for _, s := range t.Envelope.RequiredSensors {
		if s == "" || seen[s] {
			return errors.New("invalid required sensor")
		}
		seen[s] = true
	}
	return nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func (e *Engine) Evaluate(id string, c Conditions) (Decision, error) {
	return e.evaluate(id, c, false)
}

// EvaluatePatrol explicitly applies the passive simulation patrol's recovery
// policy. Only observation/sensor/clearance holds can recover, and only after
// all current local preconditions and retained authority pass again. Other
// catalogs keep Evaluate's latched HOLD behavior. Restart and stop are terminal.
func (e *Engine) EvaluatePatrol(id string, c Conditions) (Decision, error) {
	return e.evaluate(id, c, true)
}

func (e *Engine) evaluate(id string, c Conditions, patrolRecovery bool) (Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now, err := e.now()
	if err != nil {
		if r, ok := e.records[id]; ok {
			return Decision{Stop, r.status}, err
		}
		return Decision{Action: Stop}, err
	}
	r, ok := e.records[id]
	if !ok {
		return Decision{Action: Stop}, errors.New("unknown task")
	}
	if patrolRecovery && (r.task.Type != "wendy.nav.patrol.v1" || r.task.Domain != Simulation || r.task.Class != Move || r.task.Mode != Autonomous || r.task.Envelope.Fallback != Hold) {
		return Decision{Stop, r.status}, errors.New("observation recovery requires passive simulation patrol")
	}
	if r.status.Phase.Terminal() {
		return Decision{Stop, r.status}, nil
	}
	if now >= r.task.Deadline || now >= r.grantUntil {
		return e.transition(r, Failed, Stop, "deadline or authority expired"), nil
	}
	if r.status.Phase == Held && (!patrolRecovery || !recoverablePatrolHold(r.status.Reason, r.task.Envelope.RequiredSensors)) {
		return Decision{Hold, r.status}, nil
	} // generic and non-observation holds remain latched
	if r.status.Phase == Returning {
		if !c.ReturnSafe {
			return e.transition(r, Failed, Stop, "return preconditions failed"), nil
		}
		return Decision{Return, r.status}, nil
	}
	// Initial supervision is a prerequisite to starting, not a lost-link event.
	// Waiting does not reserve motion, invoke RETURN, or latch HOLD. Once a task
	// has run, the ordinary fallback below remains latched after lease loss.
	if r.status.Phase == Accepted && r.task.Mode == Supervised && !e.validLease(c.SupervisionUntil, now) {
		return e.transition(r, Accepted, Hold, "waiting for initial supervision"), nil
	}
	// Reserve before any decision that could move, including a return fallback.
	if r.task.Class == Move && r.status.Phase == Accepted {
		for otherID, other := range e.records {
			if otherID != id && other.task.Class == Move && (other.status.Phase == Running || other.status.Phase == Held || other.status.Phase == Returning) {
				return Decision{Hold, r.status}, errors.New("actuator reserved by another task")
			}
		}
	}
	reason := e.precondition(r.task, c, now)
	if reason != "" {
		switch r.task.Envelope.Fallback {
		case Hold:
			return e.transition(r, Held, Hold, reason), nil
		case Return:
			if c.ReturnSafe {
				return e.transition(r, Returning, Return, reason), nil
			}
			return e.transition(r, Failed, Stop, reason+"; return unavailable"), nil
		default:
			return e.transition(r, Failed, Stop, reason), nil
		}
	}
	return e.transition(r, Running, Continue, "local conditions satisfied"), nil
}

func recoverablePatrolHold(reason string, sensors []string) bool {
	if reason == "local observations invalid or stale" || reason == "obstacle clearance unknown or blocked" {
		return true
	}
	for _, sensor := range sensors {
		if reason == "required sensor unavailable: "+sensor {
			return true
		}
	}
	return false
}

func (e *Engine) precondition(t Task, c Conditions, now time.Duration) string {
	if c.LocalFault {
		return "executor reported local fault"
	}
	if c.ObservationInvalid {
		return "local observations invalid or stale"
	}
	if !c.InsideArea {
		return "outside authorized area"
	}
	if t.Class == Move && !c.ObstacleClear {
		return "obstacle clearance unknown or blocked"
	}
	if !finite(c.Energy) || c.Energy < t.Envelope.MinimumEnergy || c.Energy > 1 {
		return "energy requirement not met"
	}
	for _, s := range t.Envelope.RequiredSensors {
		if !c.Sensors[s] {
			return "required sensor unavailable: " + s
		}
	}
	if t.Mode == Supervised && !e.validLease(c.SupervisionUntil, now) {
		return "supervision lease unavailable"
	}
	if t.Mode == Teleop && !e.validLease(c.CommandUntil, now) {
		return "command lease unavailable"
	}
	return ""
}

func (e *Engine) validLease(until, now time.Duration) bool {
	return until > now && until-now <= e.cfg.MaxLease
}

func (e *Engine) transition(r *record, phase Phase, action Action, reason string) Decision {
	if r.status.Phase != phase || r.status.Reason != reason {
		r.status = Status{Version: r.status.Version + 1, Phase: phase, Reason: reason}
	}
	return Decision{action, r.status}
}

// Stop remembers an authorized stop even when the task has not arrived. Canceled
// records a control decision; it is not confirmation that physical motion stopped.
func (e *Engine) Stop(a StopAuthority) (Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now, err := e.now()
	if err != nil {
		return Decision{Action: Stop}, err
	}
	if a.TaskID == "" || a.Principal == "" || a.Site != e.cfg.Site || a.Assignee != e.cfg.Assignee || a.Domain != e.cfg.Domain || a.ValidUntil <= now {
		return Decision{Action: Stop}, errors.New("invalid stop authority")
	}
	e.stops[a.TaskID] = struct{}{}
	r, ok := e.records[a.TaskID]
	if !ok {
		return Decision{Action: Stop}, nil
	}
	if r.status.Phase.Terminal() {
		return Decision{Stop, r.status}, nil
	}
	return e.transition(r, Canceled, Stop, "authorized stop"), nil
}

// Report accepts only assignee-originated terminal execution reports, after an
// actual start. Caller authentication is an upstream boundary. It cannot resume
// a held task, bypass evaluation, or overwrite a terminal decision.
func (e *Engine) Report(id, caller string, version uint64, phase Phase) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if caller != e.cfg.Assignee {
		return Status{}, errors.New("only assignee may report status")
	}
	r, ok := e.records[id]
	if !ok {
		return Status{}, errors.New("unknown task")
	}
	now, err := e.now()
	if err != nil {
		return r.status, err
	}
	if phase != Succeeded && phase != Failed {
		return Status{}, errors.New("only completion reports supported")
	}
	if r.status.Phase.Terminal() {
		return r.status, errors.New("terminal status is immutable")
	}
	if now >= r.task.Deadline || now >= r.grantUntil {
		return e.transition(r, Failed, Stop, "deadline or authority expired").Status, errors.New("completion arrived after expiry")
	}
	if version <= r.status.Version {
		return r.status, errors.New("status version must increase")
	}
	if r.status.Phase == Accepted {
		return r.status, errors.New("task has not started")
	}
	if r.status.Phase == Held && phase == Succeeded {
		return r.status, errors.New("held task cannot report success")
	}
	r.status = Status{Version: version, Phase: phase, Reason: "assignee reported completion"}
	return r.status, nil
}

// Restart models a restart over retained state. It is not a persistence layer.
// Every nonterminal motion task fails, including accepted tasks, and produces a
// stop decision. It never dispatches hardware commands itself.
func (e *Engine) Restart() map[string]Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	decisions := map[string]Decision{}
	for id, r := range e.records {
		if r.task.Class == Move && !r.status.Phase.Terminal() {
			decisions[id] = e.transition(r, Failed, Stop, "executor restarted; outcome unknown")
		}
	}
	return decisions
}

func (e *Engine) Status(id string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.records[id]
	if !ok {
		return Status{}, fmt.Errorf("unknown task %q", id)
	}
	return r.status, nil
}

// LoseSupervision applies a detected continuity failure without inventing local
// sensor conditions. It cannot authorize movement or resume a held task.
func (e *Engine) LoseSupervision(id string) (Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now, err := e.now()
	if err != nil {
		return Decision{Action: Stop}, err
	}
	r, ok := e.records[id]
	if !ok {
		return Decision{Action: Stop}, errors.New("unknown task")
	}
	if r.task.Mode != Supervised {
		return Decision{Action: Stop, Status: r.status}, errors.New("task is not supervised")
	}
	if r.status.Phase.Terminal() {
		return Decision{Action: Stop, Status: r.status}, nil
	}
	if now >= r.task.Deadline || now >= r.grantUntil {
		return e.transition(r, Failed, Stop, "deadline or authority expired"), nil
	}
	if r.status.Phase == Accepted || r.status.Phase == Held {
		return Decision{Action: Hold, Status: r.status}, nil
	}
	if r.task.Envelope.Fallback == Hold {
		return e.transition(r, Held, Hold, "supervision lease unavailable"), nil
	}
	// RETURN itself needs independently fresh local safety evidence. A lease
	// loss notification supplies none, so it can only stop here.
	return e.transition(r, Failed, Stop, "supervision lease unavailable; no authorized return evidence"), nil
}
