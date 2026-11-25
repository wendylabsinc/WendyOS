// Package diagnostics provides bounded, explicit request-correlated phase traces.
// It never changes cancellation, authorization, persistence or execution policy.
package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

type Class uint8

const (
	Other Class = iota
	Challenge
	Ack
	Evaluate
	Stop
	TaskList
	Publication
	Repair
	TaskRelay
	Query
	RepairRound
	TaskRelayRound
)

var classes = []string{"other", "challenge", "ack", "evaluate", "stop", "task_list", "publication", "repair", "task_relay", "query", "repair_round", "task_relay_round"}

type Phase uint8

const (
	OuterAuthorization Phase = iota
	VerifierLock
	VerifierHold
	ReplayLock
	ReplayCommit
	ReplayAppend
	ReplayReplace
	ReplaySync
	TaskLock
	TaskHold
	MissionCheck
	SupervisorCheck
	TaskCommit
	Response
)

var phases = []string{"outer_authorization", "verifier_lock", "verifier_hold", "replay_lock", "replay_commit", "replay_append", "replay_replace", "replay_sync", "task_lock", "task_hold", "mission_check", "supervisor_check", "task_commit", "response"}

type Limits struct {
	WindowMS    int `json:"window_ms"`
	GraceMS     int `json:"grace_ms"`
	MaxRequests int `json:"max_requests"`
	MaxEvents   int `json:"max_events"`
}
type RequestRecord struct {
	ID         uint64 `json:"id"`
	Class      string `json:"class"`
	StartNS    int64  `json:"start_ns"`
	EndNS      int64  `json:"end_ns"`
	DeadlineNS int64  `json:"deadline_ns"`
	Status     int    `json:"status"`
	Canceled   bool   `json:"canceled"`
}
type Event struct {
	RequestID       uint64 `json:"request_id"`
	Phase           string `json:"phase"`
	StartNS         int64  `json:"start_ns"`
	EndNS           int64  `json:"end_ns"`
	CanceledAtStart bool   `json:"canceled_at_start"`
	Canceled        bool   `json:"canceled"`
	Failed          bool   `json:"failed"`
}
type Counters struct {
	DroppedRequests    uint64 `json:"dropped_requests"`
	DroppedEvents      uint64 `json:"dropped_events"`
	OutsideWindow      uint64 `json:"outside_window"`
	LateCompletions    uint64 `json:"late_completions"`
	UnfinishedRequests uint64 `json:"unfinished_requests"`
	UnfinishedEvents   uint64 `json:"unfinished_events"`
}
type Snapshot struct {
	Schema           string          `json:"schema"`
	InstanceID       string          `json:"instance_id"`
	RunNonce         string          `json:"run_nonce"`
	State            string          `json:"state"`
	ArmedAtUnixMS    int64           `json:"armed_at_unix_ms"`
	CapturedAtUnixMS int64           `json:"captured_at_unix_ms"`
	ElapsedNS        int64           `json:"elapsed_ns"`
	Limits           Limits          `json:"limits"`
	Requests         []RequestRecord `json:"requests"`
	Events           []Event         `json:"events"`
	Counters         Counters        `json:"counters"`
}
type Collector struct {
	mu              sync.Mutex
	now             func() time.Time
	instance, nonce string
	limits          Limits
	armed           time.Time
	requests        []RequestRecord
	events          []Event
	counters        Counters
}
type tokenKey struct{}
type Request struct {
	collector *Collector
	index     int
	ctx       context.Context
}
type Span struct {
	request *Request
	index   int
}

func New(nonce string, limits Limits) (*Collector, error) {
	b, e := hex.DecodeString(nonce)
	if e != nil || len(b) != 16 || hex.EncodeToString(b) != nonce || limits.WindowMS < 1 || limits.WindowMS > 3000 || limits.GraceMS < 1 || limits.GraceMS > 3000 || limits.MaxRequests < 1 || limits.MaxRequests > 2048 || limits.MaxEvents < 1 || limits.MaxEvents > 32768 {
		return nil, errors.New("invalid request trace limits")
	}
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		return nil, e
	}
	return &Collector{instance: hex.EncodeToString(id[:]), nonce: nonce, limits: limits, now: time.Now}, nil
}
func (c *Collector) Arm(nonce string) error {
	_, err := c.armSnapshot(nonce)
	return err
}
func (c *Collector) armSnapshot(nonce string) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if nonce != c.nonce || !c.armed.IsZero() {
		return Snapshot{}, errors.New("request trace trigger mismatch or already armed")
	}
	c.armed = c.now()
	return c.snapshotLocked(c.armed), nil
}
func (c *Collector) closed(now time.Time) bool {
	return !c.armed.IsZero() && now.Sub(c.armed) >= time.Duration(c.limits.WindowMS+c.limits.GraceMS)*time.Millisecond
}
func (c *Collector) Start(ctx context.Context, class Class) (context.Context, *Request) {
	if c == nil || int(class) >= len(classes) {
		return ctx, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.armed.IsZero() {
		return ctx, nil
	}
	if now.Sub(c.armed) >= time.Duration(c.limits.WindowMS)*time.Millisecond {
		c.counters.OutsideWindow++
		return ctx, nil
	}
	if len(c.requests) >= c.limits.MaxRequests {
		c.counters.DroppedRequests++
		return ctx, nil
	}
	r := &Request{collector: c, index: len(c.requests), ctx: ctx}
	record := RequestRecord{ID: uint64(r.index + 1), Class: classes[class], StartNS: int64(now.Sub(c.armed)), EndNS: -1, DeadlineNS: -1}
	if d, ok := ctx.Deadline(); ok {
		record.DeadlineNS = int64(d.Sub(c.armed))
	}
	c.requests = append(c.requests, record)
	return context.WithValue(ctx, tokenKey{}, r), r
}
func (r *Request) Finish(status int) {
	if r == nil {
		return
	}
	c := r.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.closed(now) {
		c.counters.LateCompletions++
		return
	}
	record := &c.requests[r.index]
	if record.EndNS >= 0 {
		return
	}
	record.EndNS = int64(now.Sub(c.armed))
	if status >= 100 && status <= 599 {
		record.Status = status
	}
	record.Canceled = r.ctx.Err() != nil
}
func Begin(ctx context.Context, phase Phase) *Span {
	if ctx == nil || int(phase) >= len(phases) {
		return nil
	}
	r, _ := ctx.Value(tokenKey{}).(*Request)
	if r == nil {
		return nil
	}
	c := r.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.closed(now) || len(c.events) >= c.limits.MaxEvents {
		c.counters.DroppedEvents++
		return nil
	}
	index := len(c.events)
	c.events = append(c.events, Event{RequestID: uint64(r.index + 1), Phase: phases[phase], StartNS: int64(now.Sub(c.armed)), EndNS: -1, CanceledAtStart: ctx.Err() != nil})
	return &Span{request: r, index: index}
}
func (s *Span) End(failure ...error) {
	if s == nil {
		return
	}
	c := s.request.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.closed(now) {
		c.counters.LateCompletions++
		return
	}
	e := &c.events[s.index]
	if e.EndNS >= 0 {
		return
	}
	e.EndNS = int64(now.Sub(c.armed))
	e.Canceled = s.request.ctx.Err() != nil
	for _, err := range failure {
		if err != nil {
			e.Failed = true
		}
	}
}
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked(c.now())
}
func (c *Collector) snapshotLocked(now time.Time) Snapshot {
	s := Snapshot{Schema: "wendy.world.request-trace.v1", InstanceID: c.instance, RunNonce: c.nonce, State: "waiting", CapturedAtUnixMS: now.UnixMilli(), Limits: c.limits, Requests: append([]RequestRecord{}, c.requests...), Events: append([]Event{}, c.events...), Counters: c.counters}
	if !c.armed.IsZero() {
		s.ArmedAtUnixMS = c.armed.UnixMilli()
		s.ElapsedNS = int64(now.Sub(c.armed))
		s.State = "recording"
		if now.Sub(c.armed) >= time.Duration(c.limits.WindowMS)*time.Millisecond {
			s.State = "draining"
		}
		if c.closed(now) {
			s.State = "complete"
		}
	}
	for _, r := range s.Requests {
		if r.EndNS < 0 {
			s.Counters.UnfinishedRequests++
		}
	}
	for _, e := range s.Events {
		if e.EndNS < 0 {
			s.Counters.UnfinishedEvents++
		}
	}
	return s
}

// Enabled reports an explicitly attached diagnostic request; it grants no authority.
func Enabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	r, _ := ctx.Value(tokenKey{}).(*Request)
	return r != nil
}
