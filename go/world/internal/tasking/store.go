package tasking

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const maxBytes = 64 << 20
const taskStoreVersion = 3

type savedRecord struct {
	Record Record   `json:"record"`
	Digest [32]byte `json:"digest"`
}
type state struct {
	AuditID        string `json:"audit_id"`
	RunID          string `json:"run_id"`
	Version        int    `json:"version"`
	Site, Assignee string
	Domain         mission.Domain
	LastUnixMS     int64                  `json:"last_unix_ms"`
	ExecutorEpoch  uint64                 `json:"executor_epoch"`
	Records        map[string]savedRecord `json:"records"`
	Stops          map[string]Record      `json:"stops"`
	Rejected       map[string]savedRecord `json:"rejected,omitempty"`
	Audit          []Audit                `json:"audit"`
	Engine         mission.Checkpoint     `json:"engine"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}
type Store struct {
	beforeCommit    func()
	mu              sync.Mutex
	cfg             Config
	path            string
	lock            *os.File
	closed          bool
	fault           error
	state           state
	engine          *mission.Engine
	write           func(string, []byte) error
	lastMono        time.Duration
	lastWall        int64
	supervisionBoot string
	supervision     map[supervisionKey]*supervisionSlot
}

func (s *Store) engineConfig() mission.Config {
	return mission.Config{Assignee: s.cfg.Assignee, Site: s.cfg.Site, Domain: s.cfg.Domain, Catalog: s.cfg.Catalog, Now: s.cfg.Monotonic, MaxLease: s.cfg.MaxLease}
}
func Open(path string, cfg Config) (*Store, error) {
	catalogCopy := make(map[string]mission.SafetyClass, len(cfg.Catalog))
	for typ, class := range cfg.Catalog {
		catalogCopy[typ] = class
	}
	cfg.Catalog = catalogCopy
	if cfg.Go2Binding != nil {
		binding := *cfg.Go2Binding
		cfg.Go2Binding = &binding
	}
	if cfg.ValidateAuthority == nil && cfg.ValidateAuthorityContext != nil {
		cfg.ValidateAuthority = func(r Record) error { return cfg.ValidateAuthorityContext(context.Background(), r) }
	}
	if cfg.ValidateSupervisor == nil && cfg.ValidateSupervisorContext != nil {
		cfg.ValidateSupervisor = func(p json.RawMessage) (supervision.Ack, supervision.Operator, error) {
			return cfg.ValidateSupervisorContext(context.Background(), p)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Monotonic == nil {
		start := time.Now()
		cfg.Monotonic = func() time.Duration { return time.Since(start) }
	}
	if cfg.MaxLease == 0 {
		cfg.MaxLease = time.Second
	}
	if cfg.MaxTelemetryAge == 0 {
		cfg.MaxTelemetryAge = time.Second
	}
	if cfg.MaxTasks == 0 {
		cfg.MaxTasks = 10000
	}
	if cfg.ExecutorPrincipal == "" || cfg.MaxTelemetryAge <= 0 || cfg.MaxTasks < 1 || path == "" {
		return nil, errors.New("invalid task store configuration")
	}
	if err := validateControlConfiguration(&cfg); err != nil {
		return nil, err
	}
	if err := validateGo2Configuration(cfg); err != nil {
		return nil, err
	}
	if err := configureSupervision(&cfg); err != nil {
		return nil, err
	}
	boot := make([]byte, 32)
	if _, err := rand.Read(boot); err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, write: writeAtomic, supervisionBoot: hex.EncodeToString(boot), supervision: map[supervisionKey]*supervisionSlot{}}
	var err error
	s.engine, err = mission.New(s.engineConfig())
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	s.path = filepath.Join(dir, filepath.Base(path))
	for _, p := range []string{s.path, s.path + ".lock"} {
		if st, e := os.Lstat(p); e == nil && st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("task store symlinks forbidden")
		}
	}
	s.lock, err = os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		s.lock.Close()
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	s.state = state{RunID: cfg.RunID, Version: taskStoreVersion, Site: cfg.Site, Assignee: cfg.Assignee, Domain: cfg.Domain, Records: map[string]savedRecord{}, Stops: map[string]Record{}}
	f, err := os.Open(s.path)
	if err == nil {
		b, e := io.ReadAll(io.LimitReader(f, maxBytes+1))
		f.Close()
		if e != nil {
			return nil, e
		}
		if len(b) > maxBytes {
			return nil, errors.New("task state too large")
		}
		var env envelope
		if err = json.Unmarshal(b, &env); err != nil {
			return nil, err
		}
		hash := sha256.Sum256(env.State)
		if hex.EncodeToString(hash[:]) != env.SHA256 {
			return nil, errors.New("task state checksum mismatch")
		}
		s.state = state{}
		if err = json.Unmarshal(env.State, &s.state); err != nil {
			return nil, err
		}
		if (s.state.Version != 1 && s.state.Version != 2 && s.state.Version != taskStoreVersion) || s.state.RunID != cfg.RunID || s.state.Site != cfg.Site || s.state.Assignee != cfg.Assignee || s.state.Domain != cfg.Domain || s.state.Records == nil || s.state.Stops == nil {
			return nil, errors.New("task state scope/version mismatch")
		}
		if s.state.Version == 1 && len(s.state.Rejected) != 0 {
			return nil, errors.New("legacy task state cannot contain durable refusals")
		}
		if s.state.Version == taskStoreVersion && len(s.state.AuditID) != 64 {
			return nil, errors.New("invalid audit incarnation")
		}
		for i, event := range s.state.Audit {
			if event.Sequence != uint64(i+1) || event.UnixMS <= 0 || event.UnixMS > s.state.LastUnixMS {
				return nil, errors.New("invalid audit sequence/time")
			}
		}
		// Version three preserves both durable refusal IDs and the audit
		// incarnation. Older readers must fail instead of discarding either.
		s.state.Version = taskStoreVersion
		for id, r := range s.state.Rejected {
			d, e := digest(r.Record.Spec)
			if e != nil || d != r.Digest || r.Record.Spec.ID != id || r.Record.Spec.Site != cfg.Site || r.Record.Spec.Assignee != cfg.Assignee || r.Record.Spec.Domain != cfg.Domain || r.Record.Status.Phase != mission.Rejected || r.Record.Status.Version != 1 || r.Record.Result != nil {
				return nil, errors.New("invalid persisted rejection")
			}
			if _, exists := s.state.Records[id]; exists {
				return nil, errors.New("rejected task also admitted")
			}
			if _, exists := s.state.Stops[id]; exists {
				return nil, errors.New("rejected task also stopped")
			}
		}
		if cfg.Now().UnixMilli() < s.state.LastUnixMS {
			return nil, errors.New("wall clock behind durable task checkpoint")
		}
		for id, r := range s.state.Records {
			d, e := digest(r.Record.Spec)
			if e != nil || d != r.Digest || r.Record.Spec.ID != id {
				return nil, errors.New("invalid persisted task digest")
			}
			if !r.Record.Status.Phase.Terminal() {
				if e := s.CheckControlContext(r.Record.Spec); e != nil {
					return nil, errors.New("active task differs from commissioned control context")
				}
			}
			if r.Record.Spec.Type == Go2GoToType && !r.Record.Status.Phase.Terminal() {
				if e := ValidateGo2Spec(r.Record.Spec, cfg.Go2Binding, cfg.RunID); e != nil {
					return nil, errors.New("active Go2 task differs from configured binding; preserve its original run")
				}
			}
			if _, ok := s.state.Engine.Records[id]; !ok {
				return nil, errors.New("missing policy checkpoint")
			}
		}
		s.engine, err = mission.Restore(s.engineConfig(), s.state.Engine)
		if err != nil {
			return nil, err
		}
		for id, r := range s.state.Records {
			st, _ := s.engine.Status(id)
			if st != r.Record.Status {
				r.Record.Status = st
				s.state.Records[id] = r
				s.appendAudit(&s.state, id, "system", "restart", st, nil)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.state.AuditID == "" {
		s.state.AuditID = hex.EncodeToString(boot)
	}
	if _, _, err = s.clock(); err != nil {
		return nil, err
	}
	if err = s.commit(s.engine, s.state); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}
func digest(spec Spec) ([32]byte, error) { b, e := json.Marshal(spec); return sha256.Sum256(b), e }
func cloneState(x state) state {
	b, _ := json.Marshal(x)
	var out state
	_ = json.Unmarshal(b, &out)
	return out
}
func copyRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Store) ready() error {
	if s.closed {
		return errors.New("task store closed")
	}
	if s.fault != nil {
		return fmt.Errorf("task store fault; reopen required: %w", s.fault)
	}
	return nil
}
func (s *Store) clock() (int64, time.Duration, error) {
	if e := s.ready(); e != nil {
		return 0, 0, e
	}
	wall := s.cfg.Now().UnixMilli()
	mono := s.cfg.Monotonic()
	if wall <= 0 || wall < s.state.LastUnixMS || wall < s.lastWall || mono < 0 || mono < s.lastMono {
		s.fault = errors.New("clock moved backwards")
		return 0, 0, s.fault
	}
	s.lastMono = mono
	s.lastWall = wall
	return wall, mono, nil
}
func (s *Store) appendAudit(st *state, id, actor, event string, status mission.Status, proof json.RawMessage) {
	st.Audit = append(st.Audit, Audit{uint64(len(st.Audit) + 1), s.cfg.Now().UnixMilli(), id, actor, event, status, append(json.RawMessage(nil), proof...)})
}
func (s *Store) commit(engine *mission.Engine, st state) (resultErr error) {
	return s.commitContext(context.Background(), engine, st)
}
func (s *Store) commitContext(ctx context.Context, engine *mission.Engine, st state) (resultErr error) {
	phase := diagnostics.Begin(ctx, diagnostics.TaskCommit)
	defer func() { phase.End(resultErr) }()
	var started time.Time
	if s.cfg.Metrics != nil {
		started = time.Now()
	}
	wall, _, clockErr := s.clock()
	if clockErr != nil {
		return clockErr
	}
	st.LastUnixMS = wall
	st.Engine = engine.Checkpoint()
	for id, saved := range st.Records {
		status := st.Engine.Records[id].Status
		if saved.Record.Status != status {
			saved.Record.Status = status
			st.Records[id] = saved
			s.appendAudit(&st, id, "system", "policy failure", status, nil)
		}
	}
	b, e := json.Marshal(st)
	if e != nil {
		return e
	}
	h := sha256.Sum256(b)
	data, e := json.Marshal(envelope{hex.EncodeToString(h[:]), b})
	if e != nil {
		return e
	}
	if len(data) > maxBytes {
		return errors.New("task storage quota exceeded")
	}
	if s.cfg.Metrics != nil {
		defer func() {
			s.cfg.Metrics.Observe(runtimeobs.TaskStoreCommit, time.Since(started))
			if resultErr != nil {
				s.cfg.Metrics.Increment(runtimeobs.TaskStoreCommitFailures)
			}
		}()
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if e = s.write(s.path, data); e != nil {
		s.fault = e
		return e
	}
	s.state = st
	s.engine = engine
	return nil
}
func deadline(ms, wall int64, mono time.Duration) (time.Duration, error) {
	if ms <= wall || ms-wall > int64((365*24*time.Hour)/time.Millisecond) {
		return 0, errors.New("deadline expired or beyond one year")
	}
	d := time.Duration(ms-wall) * time.Millisecond
	if mono > time.Duration(math.MaxInt64)-d {
		return 0, errors.New("deadline overflow")
	}
	return mono + d, nil
}
func (s *Store) Accept(spec Spec, a Authorization) (Record, error) {
	return s.AcceptContext(context.Background(), spec, a)
}
func (s *Store) AcceptContext(ctx context.Context, spec Spec, a Authorization) (Record, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	wall, mono, e := s.clock()
	if e != nil {
		return Record{}, e
	}
	if a.Principal == "" || !json.Valid(a.Proof) || a.ValidUntil.UnixMilli() <= wall {
		return Record{}, errors.New("invalid verified authorization")
	}
	d, e := digest(spec)
	if e != nil {
		return Record{}, e
	}
	if r, ok := s.state.Records[spec.ID]; ok {
		if r.Digest != d {
			return Record{}, errors.New("task ID bound to different wire spec")
		}
		return copyRecord(r.Record), nil
	}
	if r, ok := s.state.Rejected[spec.ID]; ok {
		if r.Digest != d {
			return Record{}, errors.New("rejected task ID bound to different wire spec")
		}
		return copyRecord(r.Record), nil
	}
	if _, ok := s.state.Stops[spec.ID]; ok {
		return Record{}, errors.New("task stopped before delivery")
	}
	if len(s.state.Records)+len(s.state.Stops)+len(s.state.Rejected) >= s.cfg.MaxTasks {
		return Record{}, errors.New("task quota exceeded")
	}
	if spec.Mode == mission.Teleop {
		return Record{}, errors.New("teleoperation command protocol unavailable")
	}
	if spec.Mode == mission.Supervised && !s.supervisionEnabled() {
		return Record{}, errors.New("verified supervision is not configured")
	}
	if e = s.CheckControlContext(spec); e != nil {
		return Record{}, e
	}
	if e = validateSpec(spec); e != nil {
		return Record{}, e
	}
	if spec.Type == PatrolType && (s.cfg.RunID == "" || spec.NotAfterUnixMS-wall > int64(spec.Patrol.DurationSeconds)*1000) {
		return Record{}, errors.New("patrol duration exceeds its declared simulation mission")
	}
	if spec.Type == Go2GoToType {
		if e = ValidateGo2Spec(spec, s.cfg.Go2Binding, s.cfg.RunID); e != nil {
			return Record{}, e
		}
	}
	if spec.Type == InspectType && (spec.Domain != mission.Simulation || s.cfg.RunID == "" || (spec.Trigger != nil && spec.Trigger.Component.Timing.ClockDomain != "simulation:"+s.cfg.RunID)) {
		return Record{}, errors.New("inspection requires the configured simulation run")
	}
	until, e := deadline(spec.NotAfterUnixMS, wall, mono)
	if e != nil {
		return Record{}, e
	}
	grant, e := deadline(a.ValidUntil.UnixMilli(), wall, mono)
	if e != nil {
		return Record{}, e
	}
	t := mission.Task{ID: spec.ID, Type: spec.Type, Site: spec.Site, Assignee: spec.Assignee, Domain: spec.Domain, Class: spec.Class, Mode: spec.Mode, Deadline: until, Envelope: mission.Envelope{AreaID: spec.Frame.ID + "@" + spec.Frame.Revision, RequiredSensors: spec.RequiredSensors, MinimumEnergy: spec.MinimumEnergy, Fallback: spec.Fallback}}
	en := s.engine.Clone()
	status, e := en.Accept(t, mission.Authority{TaskDigest: mission.Digest(t), Principal: a.Principal, Operator: a.Operator, AllowAutonomy: a.AllowAutonomy, Site: spec.Site, Assignee: spec.Assignee, Domain: spec.Domain, ValidUntil: grant})
	if e != nil {
		return Record{}, e
	}
	if a.ValidateAdmission != nil {
		if e = a.ValidateAdmission(); e != nil {
			return Record{}, e
		}
	}
	r := copyRecord(Record{Spec: spec, Status: status, Proof: append(json.RawMessage(nil), a.Proof...), AuthorizationUntilUnixMS: a.ValidUntil.UnixMilli()})
	st := cloneState(s.state)
	st.Records[spec.ID] = savedRecord{r, d}
	s.appendAudit(&st, spec.ID, a.Principal, "accepted", status, a.Proof)
	if e = s.commitContext(ctx, en, st); e != nil {
		return Record{}, e
	}
	// First admission cannot succeed if its context/proposal/authority expired
	// while durability was pending. No executor can observe the record until this
	// lock is released. Retain the immutable ID and fail it before returning.
	if spec.ControlContext != nil {
		wall, _, e = s.clock()
		if e == nil && (wall >= spec.NotAfterUnixMS || wall >= a.ValidUntil.UnixMilli()) {
			e = errors.New("mission expired during admission")
		}
		if e == nil && a.ValidateAdmission != nil {
			e = a.ValidateAdmission()
		}
		if e == nil && s.cfg.ValidateAuthority != nil {
			e = s.validateAuthority(ctx, copyRecord(r))
		}
		if e == nil {
			wall, _, e = s.clock()
			if e == nil && (wall >= spec.NotAfterUnixMS || wall >= a.ValidUntil.UnixMilli()) {
				e = errors.New("mission expired during authority fence")
			}
		}
		if e != nil {
			reason := e
			failed := s.engine.Clone()
			_, _ = failed.Fail(spec.ID, "admission authority expired during durable write")
			if commitErr := s.commitContext(ctx, failed, cloneState(s.state)); commitErr != nil {
				return Record{}, commitErr
			}
			return copyRecord(s.state.Records[spec.ID].Record), reason
		}
	}
	return copyRecord(r), nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func inside(b Bounds, p [3]float64) bool {
	for i, v := range p {
		if !finite(v) || v < b.Min[i] || v > b.Max[i] {
			return false
		}
	}
	return true
}
func validateSpec(s Spec) error {
	if e := validatePatrolShape(s); e != nil {
		return e
	}
	if e := validateGo2Shape(s); e != nil {
		return e
	}
	if s.InspectionDestination != nil && (s.ControlContext == nil || s.Trigger == nil || !missioncontrol.IsTypedInspectionTrigger(s.Trigger.Component.Key.Type) || s.Type != InspectType || s.Domain != mission.Simulation || s.Mode != mission.Autonomous) {
		return errors.New("inspection destination requires commissioned native inspection")
	}
	if s.Trigger != nil {
		if err := s.Trigger.Validate(s.Site, s.Frame); err != nil {
			return err
		}
		if missioncontrol.IsTypedInspectionTrigger(s.Trigger.Component.Key.Type) {
			if err := missioncontrol.ValidateNativeInspectionTarget(s.Trigger.Component, s.Target, s.InspectionDestination); err != nil {
				return err
			}
		}
	}
	if e := s.Frame.Validate(); e != nil {
		return e
	}
	for i := 0; i < 3; i++ {
		if !finite(s.Area.Min[i]) || !finite(s.Area.Max[i]) || s.Area.Min[i] >= s.Area.Max[i] {
			return errors.New("invalid bounded area")
		}
	}
	if !inside(s.Area, s.Target) || !finite(s.SpeedMPS) || !finite(s.ToleranceM) || !finite(s.StopSpeedMPS) || s.SpeedMPS <= 0 || s.SpeedMPS > 10 || s.ToleranceM <= 0 || s.ToleranceM > 10 || s.StopSpeedMPS < 0 || s.StopSpeedMPS > s.SpeedMPS {
		return errors.New("invalid navigation target or limits")
	}
	// Return needs an independently validated return route, not a boolean claim.
	if s.Fallback == mission.Return {
		return errors.New("return adapter not implemented")
	}
	return nil
}
func (s *Store) Stop(id string, a Authorization) (Record, error) {
	return s.StopContext(context.Background(), id, a)
}
func (s *Store) StopContext(ctx context.Context, id string, a Authorization) (Record, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	wall, mono, e := s.clock()
	if e != nil {
		return Record{}, e
	}
	if id == "" || a.Principal == "" || !a.Operator || !json.Valid(a.Proof) {
		return Record{}, errors.New("invalid stop authorization")
	}
	until, e := deadline(a.ValidUntil.UnixMilli(), wall, mono)
	if e != nil {
		return Record{}, e
	}
	if r, ok := s.state.Stops[id]; ok {
		return copyRecord(r), nil
	}
	if r, ok := s.state.Rejected[id]; ok {
		return copyRecord(r.Record), nil
	}
	if _, ok := s.state.Records[id]; !ok && len(s.state.Records)+len(s.state.Stops)+len(s.state.Rejected) >= s.cfg.MaxTasks {
		return Record{}, errors.New("task quota exceeded")
	}
	en := s.engine.Clone()
	d, e := en.Stop(mission.StopAuthority{TaskID: id, Principal: a.Principal, Site: s.cfg.Site, Assignee: s.cfg.Assignee, Domain: s.cfg.Domain, ValidUntil: until})
	if e != nil {
		return Record{}, e
	}
	st := cloneState(s.state)
	r := Record{Spec: Spec{ID: id, Site: s.cfg.Site, Assignee: s.cfg.Assignee, Domain: s.cfg.Domain}, Status: mission.Status{Version: 1, Phase: mission.Canceled, Reason: "stop before delivery"}, Proof: a.Proof, AuthorizationUntilUnixMS: a.ValidUntil.UnixMilli()}
	if old, ok := st.Records[id]; ok {
		old.Record.Status = d.Status
		st.Records[id] = old
		r = old.Record
	}
	st.Stops[id] = copyRecord(r)
	s.appendAudit(&st, id, a.Principal, "stop", r.Status, a.Proof)
	if e = s.commitContext(ctx, en, st); e != nil {
		return Record{}, e
	}
	return copyRecord(r), nil
}
func (s *Store) BeginExecutor(actor string) (uint64, error) {
	return s.BeginExecutorContext(context.Background(), actor)
}
func (s *Store) BeginExecutorContext(ctx context.Context, actor string) (uint64, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	if _, _, e := s.clock(); e != nil {
		return 0, e
	}
	if actor != s.cfg.ExecutorPrincipal {
		return 0, errors.New("wrong executor principal")
	}
	if s.state.ExecutorEpoch == math.MaxUint64 {
		return 0, errors.New("executor epoch exhausted")
	}
	en := s.engine.Clone()
	st := cloneState(s.state)
	st.ExecutorEpoch++
	for id, d := range en.Restart() {
		r := st.Records[id]
		r.Record.Status = d.Status
		st.Records[id] = r
		s.appendAudit(&st, id, actor, "executor restart", d.Status, nil)
	}
	s.appendAudit(&st, "", actor, "executor started", mission.Status{}, nil)
	if e := s.commitContext(ctx, en, st); e != nil {
		return 0, e
	}
	s.supervision = map[supervisionKey]*supervisionSlot{}
	return st.ExecutorEpoch, nil
}
func (s *Store) executor(actor string, epoch uint64) error {
	if actor != s.cfg.ExecutorPrincipal || epoch == 0 || epoch != s.state.ExecutorEpoch {
		return errors.New("stale or unauthorized executor")
	}
	return nil
}
func (s *Store) Evaluate(id, actor string, epoch uint64, t Telemetry) (Decision, error) {
	return s.EvaluateContext(context.Background(), id, actor, epoch, t)
}
func (s *Store) EvaluateContext(ctx context.Context, id, actor string, epoch uint64, t Telemetry) (Decision, error) {
	return s.evaluateContext(ctx, id, actor, epoch, t, false, false)
}

// EvaluateLiveContext evaluates one live executor delivery. Cancellation can
// abandon a candidate decision at a safe boundary, but never interrupts or
// rolls back a durable commit. Abandoned deliveries return Stop; a later fresh
// delivery must pass every authority and observation check again. Diagnostic
// callers retain EvaluateContext's existing completion semantics.
func (s *Store) EvaluateLiveContext(ctx context.Context, id, actor string, epoch uint64, t Telemetry) (Decision, error) {
	return s.evaluateContext(ctx, id, actor, epoch, t, true, false)
}

func (s *Store) evaluateContext(ctx context.Context, id, actor string, epoch uint64, t Telemetry, live, patrolRecovery bool) (Decision, error) {
	canceled := func() error {
		if live {
			return ctx.Err()
		}
		return nil
	}
	if err := canceled(); err != nil {
		return Decision{Action: mission.Stop}, err
	}
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	if err := canceled(); err != nil {
		return Decision{Action: mission.Stop}, err
	}
	wall, _, e := s.clock()
	if e != nil {
		return Decision{Action: mission.Stop}, e
	}
	if e = s.executor(actor, epoch); e != nil {
		return Decision{Action: mission.Stop}, e
	}
	r, ok := s.state.Records[id]
	if !ok {
		return Decision{Action: mission.Stop}, errors.New("unknown task")
	}
	if patrolRecovery && (r.Record.Spec.Type != PatrolType || validatePatrolShape(r.Record.Spec) != nil) {
		return Decision{Action: mission.Stop}, errors.New("recovery requires an admitted passive simulation patrol")
	}
	en := s.engine.Clone()
	evaluateMission := func(c mission.Conditions) (mission.Decision, error) {
		if patrolRecovery {
			return en.EvaluatePatrol(id, c)
		}
		return en.Evaluate(id, c)
	}
	st := cloneState(s.state)
	var d mission.Decision
	var budget *SupervisionBudget
	if !r.Record.Status.Phase.Terminal() && (wall >= r.Record.Spec.NotAfterUnixMS || wall >= r.Record.AuthorizationUntilUnixMS) {
		status, _ := en.Fail(id, "absolute deadline or authority expired")
		d = mission.Decision{Action: mission.Stop, Status: status}
		e = errors.New("absolute deadline or authority expired")
	}
	if e == nil && s.cfg.ValidateAuthority != nil && !r.Record.Status.Phase.Terminal() {
		e = s.validateAuthority(ctx, copyRecord(r.Record))
		if err := canceled(); err != nil {
			return Decision{Action: mission.Stop}, err
		}
		if e != nil {
			status, _ := en.Fail(id, "authority no longer valid")
			d = mission.Decision{Action: mission.Stop, Status: status}
		}
	}
	if e == nil {
		good := !t.LocalFault && t.Frame == r.Record.Spec.Frame && t.SampleUnixMS > 0 && t.SampleUnixMS <= wall && wall-t.SampleUnixMS <= s.cfg.MaxTelemetryAge.Milliseconds() && finite(t.Energy) && t.Energy >= 0 && t.Energy <= 1
		for i := range t.Velocity {
			good = good && finite(t.Velocity[i]) && finite(t.Position[i])
		}
		sensors := t.Sensors
		if !good {
			sensors = nil
		}
		// Executor telemetry cannot establish operator presence or command authority.
		supervisionUntil, checkErr := s.supervisionDeadlineContext(ctx, id)
		if err := canceled(); err != nil {
			return Decision{Action: mission.Stop}, err
		}
		if checkErr != nil {
			return Decision{Action: mission.Stop}, checkErr
		}
		wall, _, checkErr = s.clock()
		if checkErr != nil {
			return Decision{Action: mission.Stop}, checkErr
		}
		// Proof verification may take time. Recompute freshness at this boundary.
		good = good && t.SampleUnixMS <= wall && wall-t.SampleUnixMS <= s.cfg.MaxTelemetryAge.Milliseconds()
		if !good {
			sensors = nil
		}
		if !r.Record.Status.Phase.Terminal() && (wall >= r.Record.Spec.NotAfterUnixMS || wall >= r.Record.AuthorizationUntilUnixMS) {
			status, _ := en.Fail(id, "absolute deadline or authority expired")
			d = mission.Decision{Action: mission.Stop, Status: status}
			e = errors.New("absolute deadline or authority expired")
		} else {
			d, e = evaluateMission(mission.Conditions{ObservationInvalid: !good, LocalFault: t.LocalFault, InsideArea: good && inside(r.Record.Spec.Area, t.Position), ObstacleClear: good && t.ObstacleClear && patrolPositionClear(r.Record.Spec, t), Energy: t.Energy, Sensors: sensors, SupervisionUntil: supervisionUntil, CommandUntil: 0, ReturnSafe: false})
		}
		if e == nil && d.Action == mission.Continue && r.Record.Spec.Class == mission.Move && ((r.Record.Spec.Type != PatrolType && reached(r.Record.Spec, t)) || (patrolRecovery && patrolArrived(r.Record.Spec, t, wall))) {
			complete := r.Record.Spec.Type != InspectType
			if r.Record.Spec.Type == InspectType && t.Inspection != nil {
				e = validateInspection(r.Record.Spec, t, wall, s.cfg.MaxTelemetryAge)
				if e == nil && (s.cfg.RunID == "" || t.Inspection.RunID != s.cfg.RunID) {
					e = errors.New("inspection run differs from local simulation")
				}
				if e == nil {
					if s.cfg.ValidateInspection == nil {
						e = errors.New("inspection evidence validator required")
					} else {
						e = s.cfg.ValidateInspection(*t.Inspection)
					}
				}
				if err := canceled(); err != nil {
					return Decision{Action: mission.Stop}, err
				}
				if e == nil {
					var current int64
					current, _, e = s.clock()
					if e == nil {
						e = validateInspection(r.Record.Spec, t, current, s.cfg.MaxTelemetryAge)
					}
					if e == nil && (current >= r.Record.Spec.NotAfterUnixMS || current >= r.Record.AuthorizationUntilUnixMS) {
						e = errors.New("inspection deadline elapsed during verification")
					}
					complete = e == nil
				}
			}
			if e != nil {
				d.Action = mission.Stop
			}
			if complete {
				status, err := en.Report(id, s.cfg.Assignee, d.Status.Version+1, mission.Succeeded)
				if err != nil {
					e = err
					d.Action = mission.Stop
					if status.Version != 0 {
						d.Status = status
					}
				} else {
					d = mission.Decision{Action: mission.Stop, Status: status}
					if r.Record.Spec.Type == PatrolType && t.PatrolArrival != nil {
						r.Record.PatrolReturnResult = &PatrolReturnResult{Arrival: *t.PatrolArrival, Position: t.Position, Velocity: t.Velocity, Frame: t.Frame}
						// Own nested tour receipts; caller mutation must not rewrite a terminal result.
						r.Record = copyRecord(r.Record)
					}
					if r.Record.Spec.Type == InspectType {
						copy := copyRecord(Record{Result: t.Inspection})
						r.Record.Result = copy.Result
					}
				}
			}
		}
	}
	r.Record.Status = d.Status
	st.Records[id] = r
	if d.Status != s.state.Records[id].Record.Status {
		s.appendAudit(&st, id, actor, "evaluated", d.Status, nil)
	}
	if err := canceled(); err != nil {
		return Decision{Action: mission.Stop}, err
	}
	if err := s.commitContext(ctx, en, st); err != nil {
		return Decision{Action: mission.Stop}, err
	}
	// Once a commit starts its result is retained, even if its caller leaves.
	// Do not turn an abandoned post-write freshness check into another state
	// transition. No continuing permission is returned for this delivery.
	if err := canceled(); err != nil {
		return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, err
	}
	// A durable write can outlast either a short lease or a sensor sample.
	// Recheck before returning any decision that permits continuing motion.
	if e == nil && d.Action == mission.Continue {
		var until time.Duration
		var err error
		if r.Record.Spec.Mode == mission.Supervised {
			until, err = s.supervisionDeadlineContext(ctx, id)
			if abandoned := canceled(); abandoned != nil {
				return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, abandoned
			}
			if err != nil {
				return Decision{Action: mission.Stop}, err
			}
		}
		// Both task persistence and supervisor verification can block on durable
		// I/O. Recheck the retained mission last, then resample every local
		// deadline below so this check cannot renew a lease or stale telemetry.
		if s.cfg.ValidateAuthority != nil {
			authorityErr := s.validateAuthority(ctx, copyRecord(r.Record))
			if err := canceled(); err != nil {
				return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, err
			}
			if authorityErr != nil {
				en = s.engine.Clone()
				status, failErr := en.Fail(id, "authority no longer valid after evaluation")
				if failErr != nil {
					return Decision{Action: mission.Stop}, failErr
				}
				st = cloneState(s.state)
				r.Record.Status = status
				st.Records[id] = r
				s.appendAudit(&st, id, actor, "authority changed during evaluation", status, nil)
				if err := canceled(); err != nil {
					return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, err
				}
				if err := s.commitContext(ctx, en, st); err != nil {
					return Decision{Action: mission.Stop}, err
				}
				return Decision{Action: mission.Stop, Record: copyRecord(r.Record)}, authorityErr
			}
		}
		current, currentMono, err := s.clock()
		if err != nil {
			return Decision{Action: mission.Stop}, err
		}
		en = s.engine.Clone()
		var final mission.Decision
		if current >= r.Record.Spec.NotAfterUnixMS || current >= r.Record.AuthorizationUntilUnixMS {
			status, _ := en.Fail(id, "absolute deadline or authority expired")
			final = mission.Decision{Action: mission.Stop, Status: status}
		} else {
			good := !t.LocalFault && t.Frame == r.Record.Spec.Frame && t.SampleUnixMS > 0 && t.SampleUnixMS <= current && current-t.SampleUnixMS <= s.cfg.MaxTelemetryAge.Milliseconds() && finite(t.Energy) && t.Energy >= 0 && t.Energy <= 1
			for i := range t.Velocity {
				good = good && finite(t.Velocity[i]) && finite(t.Position[i])
			}
			sensors := t.Sensors
			if !good {
				sensors = nil
			}
			final, err = evaluateMission(mission.Conditions{ObservationInvalid: !good, LocalFault: t.LocalFault, InsideArea: good && inside(r.Record.Spec.Area, t.Position), ObstacleClear: good && t.ObstacleClear && patrolPositionClear(r.Record.Spec, t), Energy: t.Energy, Sensors: sensors, SupervisionUntil: until})
			if err != nil {
				return Decision{Action: mission.Stop}, err
			}
		}
		if final.Action == mission.Continue && r.Record.Spec.Mode == mission.Supervised {
			// The executor subtracts its full Evaluate round trip and processing
			// from this budget, then passes the fixed expiry to the actuator.
			current, currentMono, err = s.clock()
			if err != nil {
				return Decision{Action: mission.Stop}, err
			}
			budget = s.currentSupervisionBudget(id, current, currentMono)
			stale := t.SampleUnixMS > current || current-t.SampleUnixMS > s.cfg.MaxTelemetryAge.Milliseconds()
			if budget == nil || stale {
				sensors := t.Sensors
				if stale {
					sensors = nil
				}
				final, err = evaluateMission(mission.Conditions{ObservationInvalid: stale, LocalFault: t.LocalFault, InsideArea: !stale && inside(r.Record.Spec.Area, t.Position), ObstacleClear: !stale && t.ObstacleClear, Energy: t.Energy, Sensors: sensors})
				budget = nil
				if err != nil {
					return Decision{Action: mission.Stop}, err
				}
			}
		}
		if final.Status != r.Record.Status || final.Action != d.Action {
			st = cloneState(s.state)
			r.Record.Status = final.Status
			st.Records[id] = r
			s.appendAudit(&st, id, actor, "conditions changed during evaluation", final.Status, nil)
			if abandoned := canceled(); abandoned != nil {
				return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, abandoned
			}
			if err = s.commitContext(ctx, en, st); err != nil {
				return Decision{Action: mission.Stop}, err
			}
		}
		d = final
	}
	if err := canceled(); err != nil {
		return Decision{Action: mission.Stop, Record: copyRecord(s.state.Records[id].Record)}, err
	}

	return Decision{Action: d.Action, Record: copyRecord(r.Record), Supervision: budget}, e
}
func reached(s Spec, t Telemetry) bool {
	var distance, speed float64
	for i := 0; i < 3; i++ {
		distance = math.Hypot(distance, t.Position[i]-s.Target[i])
		speed = math.Hypot(speed, t.Velocity[i])
	}
	return distance <= s.ToleranceM && speed <= s.StopSpeedMPS
}
func (s *Store) Report(id, actor string, epoch, version uint64, phase mission.Phase) (Record, error) {
	return s.ReportContext(context.Background(), id, actor, epoch, version, phase)
}
func (s *Store) ReportContext(ctx context.Context, id, actor string, epoch, version uint64, phase mission.Phase) (Record, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	wall, _, clockErr := s.clock()
	if clockErr != nil {
		return Record{}, clockErr
	}
	if e := s.executor(actor, epoch); e != nil {
		return Record{}, e
	}
	r, ok := s.state.Records[id]
	if !ok {
		return Record{}, errors.New("unknown task")
	}
	if phase == mission.Succeeded && r.Record.Spec.Class == mission.Move {
		return Record{}, errors.New("motion success requires measured target completion")
	}
	en := s.engine.Clone()
	st := cloneState(s.state)
	var status mission.Status
	var e error
	if !r.Record.Status.Phase.Terminal() && (wall >= r.Record.Spec.NotAfterUnixMS || wall >= r.Record.AuthorizationUntilUnixMS) {
		e = errors.New("absolute deadline or authority expired")
	}
	if e == nil && s.cfg.ValidateAuthority != nil {
		e = s.validateAuthority(ctx, copyRecord(r.Record))
	}
	if e != nil {
		status, _ = en.Fail(id, "authority no longer valid")
	} else {
		status, e = en.Report(id, s.cfg.Assignee, version, phase)
	}
	if status.Version == 0 {
		return Record{}, e
	}
	r.Record.Status = status
	st.Records[id] = r
	if status != s.state.Records[id].Record.Status {
		s.appendAudit(&st, id, actor, "reported", status, nil)
	}
	if err := s.commitContext(ctx, en, st); err != nil {
		return Record{}, err
	}
	return copyRecord(r.Record), e
}
func (s *Store) Get(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return Record{}, e
	}
	if r, ok := s.state.Records[id]; ok {
		return copyRecord(r.Record), nil
	}
	if r, ok := s.state.Stops[id]; ok {
		return copyRecord(r), nil
	}
	if r, ok := s.state.Rejected[id]; ok {
		return copyRecord(r.Record), nil
	}
	return Record{}, errors.New("unknown task")
}
func (s *Store) List() ([]Record, error) {
	return s.ListContext(context.Background())
}
func (s *Store) ListContext(ctx context.Context) ([]Record, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	if e := s.ready(); e != nil {
		return nil, e
	}
	out := make([]Record, 0, len(s.state.Records))
	for _, r := range s.state.Records {
		out = append(out, copyRecord(r.Record))
	}
	for _, r := range s.state.Rejected {
		out = append(out, copyRecord(r.Record))
	}
	for id, r := range s.state.Stops {
		if _, exists := s.state.Records[id]; !exists {
			out = append(out, copyRecord(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.ID < out[j].Spec.ID })
	return out, nil
}
func (s *Store) AuditLog() ([]Audit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return nil, e
	}
	return cloneState(s.state).Audit, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		return s.lock.Close()
	}
	return nil
}
func writeAtomic(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".task-state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
