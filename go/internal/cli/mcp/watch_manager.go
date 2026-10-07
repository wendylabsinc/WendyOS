package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

const (
	maxActiveWatches     = 2
	watchEventBufferSize = 100
	watchEndedRetained   = 10
	watchStartWait       = 30 * time.Second
	// The MCP client gives the server 2 s after closing stdin before SIGTERM.
	watchShutdownTimeout = 1500 * time.Millisecond
	watchEventMethod     = "notifications/wendy/watch_event"
	watchStatusMethod    = "notifications/wendy/watch_status"
	watchStoppedReason   = "stopped"
)

var (
	errWatchLimit    = errors.New("two watches are already active; stop one first")
	errWatchNotFound = errors.New("no watch with that id in this session")
)

// watchEvent is one buffered event, numbered per watch.
type watchEvent struct {
	Sequence   uint64       `json:"sequence"`
	Kind       string       `json:"kind"`
	Classes    []watchClass `json:"classes"`
	OccurredAt string       `json:"occurred_at"`
}

// watchView is a watch as tools and clients see it.
type watchView struct {
	WatchID       string   `json:"watch_id"`
	Label         string   `json:"label"`
	Camera        string   `json:"camera"`
	CameraName    string   `json:"camera_name"`
	Classes       []string `json:"classes"`
	MinConfidence float64  `json:"min_confidence"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	StartedAt     string   `json:"started_at"`
	LastEventAt   string   `json:"last_event_at,omitempty"`
	LastSequence  uint64   `json:"last_sequence"`
}

// watchRecord is one watch. Its fields are guarded by the manager's mu.
type watchRecord struct {
	id          string
	spec        watchSpec
	revision    uint64 // the connection the watch was started on
	state       watchState
	reason      string
	startedAt   time.Time
	lastEventAt time.Time
	events      []watchEvent
	sequence    uint64
	missed      int // notifications that failed, reported with the next status
	handle      watchHandle
	changed     chan struct{} // closed and replaced on every change
}

// watchManager owns one MCP server's watches (design §6.1). It never holds mu
// while calling the backend or sending a notification, and it is never called
// with the server's mu held, so it cannot deadlock against connection changes.
type watchManager struct {
	backend   watchBackend
	notify    func(method string, params map[string]any) error
	revision  func() uint64
	serverID  string
	startWait time.Duration

	mu      sync.Mutex
	next    int
	watches map[string]*watchRecord
	order   []string
	closed  bool
}

func newWatchManager(backend watchBackend, notify func(method string, params map[string]any) error, revision func() uint64) *watchManager {
	var id [4]byte
	_, _ = rand.Read(id[:])
	return &watchManager{backend: backend, notify: notify, revision: revision, serverID: hex.EncodeToString(id[:]), startWait: watchStartWait, watches: map[string]*watchRecord{}}
}

func (m *watchManager) activeLocked() []*watchRecord {
	var active []*watchRecord
	for _, id := range m.order {
		if rec := m.watches[id]; rec.state != watchEnded {
			active = append(active, rec)
		}
	}
	return active
}

func (m *watchManager) freeSlots() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maxActiveWatches - len(m.activeLocked())
}

// start deploys a watch and waits up to startWait for it to leave PREPARING. A
// first watch on a fresh device installs the detector, which takes minutes;
// its READY then arrives as a notification.
func (m *watchManager) start(ctx context.Context, conn *grpcclient.AgentConnection, revision uint64, spec watchSpec) (watchView, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return watchView{}, errors.New("the watch manager has shut down")
	}
	if active := m.activeLocked(); len(active) >= maxActiveWatches {
		labels := make([]string, 0, len(active))
		for _, rec := range active {
			labels = append(labels, fmt.Sprintf("%s (%s)", rec.id, rec.spec.Label))
		}
		m.mu.Unlock()
		return watchView{}, fmt.Errorf("%w: %s", errWatchLimit, strings.Join(labels, ", "))
	}
	m.next++
	spec.Name = fmt.Sprintf("chat-%s-%d", m.serverID, m.next)
	// The record holds the slot while the campaign deploys.
	rec := &watchRecord{id: fmt.Sprintf("w%d", m.next), spec: spec, revision: revision, state: watchPreparing, startedAt: time.Now(), changed: make(chan struct{})}
	m.watches[rec.id] = rec
	m.order = append(m.order, rec.id)
	m.mu.Unlock()

	handle, err := m.backend.Start(ctx, conn, spec)
	m.mu.Lock()
	if err != nil {
		m.forgetLocked(rec.id)
		m.mu.Unlock()
		return watchView{}, err
	}
	rec.handle = handle
	if rec.state != watchEnded && m.revision() != rec.revision {
		// The device changed while this watch deployed: endStale ran before
		// the record had a handle to stop.
		m.endLocked(rec, "device changed")
	}
	ended := rec.state == watchEnded
	m.mu.Unlock()
	if ended {
		m.stopHandle(handle, time.Second)
		m.sendStatus(rec, "")
		return m.view(rec), nil
	}
	m.sendStatus(rec, "")
	go m.follow(rec, handle)
	return m.waitWhilePreparing(ctx, rec), nil
}

func (m *watchManager) forgetLocked(id string) {
	delete(m.watches, id)
	for i, existing := range m.order {
		if existing == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *watchManager) follow(rec *watchRecord, handle watchHandle) {
	for update := range handle.Updates() {
		m.apply(rec, update)
	}
}

func (m *watchManager) apply(rec *watchRecord, u watchUpdate) {
	m.mu.Lock()
	if rec.state == watchEnded {
		m.mu.Unlock()
		return
	}
	switch {
	case u.Event != nil:
		rec.sequence++
		event := watchEvent{Sequence: rec.sequence, Kind: u.Event.Kind, Classes: u.Event.Classes, OccurredAt: u.Event.OccurredAt.UTC().Format(time.RFC3339Nano)}
		if event.Classes == nil {
			event.Classes = []watchClass{}
		}
		rec.events = append(rec.events, event)
		if len(rec.events) > watchEventBufferSize {
			rec.events = rec.events[len(rec.events)-watchEventBufferSize:]
		}
		rec.lastEventAt = u.Event.OccurredAt
		m.signalLocked(rec)
		m.mu.Unlock()
		m.deliver(rec, watchEventMethod, map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "sequence": event.Sequence, "kind": event.Kind, "classes": event.Classes, "occurred_at": event.OccurredAt})
	case u.Status != nil && u.Status.State == watchEnded:
		handle := rec.handle
		m.endLocked(rec, u.Status.Reason)
		m.mu.Unlock()
		m.stopHandle(handle, 5*time.Second)
		m.sendStatus(rec, "")
	case u.Status != nil:
		changed := rec.state != u.Status.State || rec.reason != u.Status.Reason
		rec.state, rec.reason = u.Status.State, u.Status.Reason
		m.signalLocked(rec)
		m.mu.Unlock()
		if changed {
			m.sendStatus(rec, "")
		}
	case u.Gap != "":
		m.mu.Unlock()
		m.sendStatus(rec, u.Gap)
	default:
		m.mu.Unlock()
	}
}

func (m *watchManager) signalLocked(rec *watchRecord) {
	close(rec.changed)
	rec.changed = make(chan struct{})
}

func (m *watchManager) endLocked(rec *watchRecord, reason string) {
	rec.state, rec.reason = watchEnded, reason
	m.signalLocked(rec)
	// Keep a few ended watches, so a client can still learn how they ended.
	var ended []string
	for _, id := range m.order {
		if m.watches[id].state == watchEnded {
			ended = append(ended, id)
		}
	}
	for len(ended) > watchEndedRetained {
		m.forgetLocked(ended[0])
		ended = ended[1:]
	}
}

func (m *watchManager) stopHandle(handle watchHandle, timeout time.Duration) {
	if handle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = handle.Stop(ctx)
}

// stop ends a watch and removes it from the device. The record is ended first,
// so no event arrives after watch_stop returns.
func (m *watchManager) stop(ctx context.Context, id, reason string) (watchView, error) {
	m.mu.Lock()
	rec := m.watches[id]
	if rec == nil {
		m.mu.Unlock()
		return watchView{}, errWatchNotFound
	}
	if rec.state == watchEnded {
		view := m.viewLocked(rec)
		m.mu.Unlock()
		return view, nil
	}
	handle := rec.handle
	m.endLocked(rec, reason)
	m.mu.Unlock()
	var err error
	if handle != nil {
		err = handle.Stop(ctx)
	}
	m.sendStatus(rec, "")
	return m.view(rec), err
}

// endStale ends every watch started on another connection. That connection is
// already closed, so nothing reaches the old device: its campaigns lapse within
// lease + 5 s (design §6.1).
func (m *watchManager) endStale(revision uint64) {
	m.mu.Lock()
	var ended []*watchRecord
	for _, id := range m.order {
		rec := m.watches[id]
		if rec.state != watchEnded && rec.revision != revision && rec.handle != nil {
			m.endLocked(rec, "device changed")
			ended = append(ended, rec)
		}
	}
	m.mu.Unlock()
	for _, rec := range ended {
		go m.stopHandle(rec.handle, time.Second)
		m.sendStatus(rec, "")
	}
}

// shutdown removes every active watch when the server exits. Removals run
// concurrently under one short deadline; a lease covers anything the device
// did not receive.
func (m *watchManager) shutdown() {
	m.mu.Lock()
	m.closed = true
	var handles []watchHandle
	for _, rec := range m.activeLocked() {
		if rec.handle != nil {
			handles = append(handles, rec.handle)
		}
		m.endLocked(rec, "the session ended")
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, handle := range handles {
		wg.Add(1)
		go func(h watchHandle) {
			defer wg.Done()
			m.stopHandle(h, watchShutdownTimeout)
		}(handle)
	}
	wg.Wait()
}

func (m *watchManager) waitWhilePreparing(ctx context.Context, rec *watchRecord) watchView {
	timer := time.NewTimer(m.startWait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if rec.state != watchPreparing {
			view := m.viewLocked(rec)
			m.mu.Unlock()
			return view
		}
		changed := rec.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return m.view(rec)
		case <-ctx.Done():
			return m.view(rec)
		}
	}
}

// events returns buffered events after a sequence number, waiting up to wait
// for one to arrive. gap reports events that left the buffer unread.
func (m *watchManager) events(ctx context.Context, id string, after uint64, wait time.Duration) (watchView, []watchEvent, bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		rec := m.watches[id]
		if rec == nil {
			m.mu.Unlock()
			return watchView{}, nil, false, errWatchNotFound
		}
		events := []watchEvent{}
		for _, event := range rec.events {
			if event.Sequence > after {
				events = append(events, event)
			}
		}
		gap := len(rec.events) > 0 && rec.events[0].Sequence > after+1
		changed := rec.changed
		if len(events) > 0 || rec.state == watchEnded || wait <= 0 {
			view := m.viewLocked(rec)
			m.mu.Unlock()
			return view, events, gap, nil
		}
		m.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			wait = 0
		case <-ctx.Done():
			wait = 0
		}
	}
}

func (m *watchManager) list() []watchView {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]watchView, 0, len(m.order))
	for _, id := range m.order {
		views = append(views, m.viewLocked(m.watches[id]))
	}
	return views
}

func (m *watchManager) view(rec *watchRecord) watchView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(rec)
}

func (m *watchManager) viewLocked(rec *watchRecord) watchView {
	view := watchView{
		WatchID: rec.id, Label: rec.spec.Label, Camera: rec.spec.CameraID, CameraName: rec.spec.CameraName,
		Classes: rec.spec.Classes, MinConfidence: rec.spec.MinConfidence, State: string(rec.state), Reason: rec.reason,
		StartedAt: rec.startedAt.UTC().Format(time.RFC3339), LastSequence: rec.sequence,
	}
	if !rec.lastEventAt.IsZero() {
		view.LastEventAt = rec.lastEventAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

// sendStatus sends the watch's status. reason, when set, replaces the standing
// reason for this one notification (a gap does not change the watch's state).
func (m *watchManager) sendStatus(rec *watchRecord, reason string) {
	m.mu.Lock()
	if reason == "" {
		reason = rec.reason
	}
	params := map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "state": string(rec.state), "reason": reason, "camera": rec.spec.CameraName, "watching": rec.spec.Classes}
	if rec.missed > 0 {
		params["missed_notifications"] = rec.missed
		rec.missed = 0
	}
	m.mu.Unlock()
	m.deliver(rec, watchStatusMethod, params)
}

// deliver sends one notification. mcp-go's stdio session queues 100 and fails
// a send when full; the failure is reported with the watch's next status.
func (m *watchManager) deliver(rec *watchRecord, method string, params map[string]any) {
	if m.notify == nil {
		return
	}
	if err := m.notify(method, params); err != nil {
		m.mu.Lock()
		rec.missed++
		m.mu.Unlock()
	}
}
