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
	// watch_stop and a watch the device ended wait this long for the removal;
	// the lease removes a campaign the device never hears about.
	watchStopTimeout = 5 * time.Second
	// The MCP client gives the server 2 s after closing stdin before SIGTERM.
	watchShutdownTimeout = 1500 * time.Millisecond
	watchEventMethod     = "notifications/wendy/watch_event"
	watchStatusMethod    = "notifications/wendy/watch_status"
	watchStoppedReason   = "stopped"
)

var (
	errWatchLimit    = errors.New("two watches are already active; stop one first")
	errWatchNotFound = errors.New("no watch with that id in this session")
	// errWatchStillDeploying is a watch_stop whose watch was still deploying
	// when the stop's bound ran out; start removes it once the deploy returns.
	errWatchStillDeploying = errors.New("the device was still setting up the watch")
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
	// settled is closed once the deploy has returned and, for a watch that
	// ended during it, start has removed it. removeErr, the removal's result,
	// is written before settled closes.
	settled   chan struct{}
	removeErr error
	endSeq    uint64 // order in which watches ended; 0 while active

	// notifyMu serializes this watch's notifications. Lock order is
	// notifyMu then mu, never the reverse.
	notifyMu  sync.Mutex
	endedSent bool // guarded by notifyMu: the ENDED status has gone out
}

// watchManager owns one MCP server's watches (design §6.1). It never holds mu
// while calling the backend or sending a notification, and it is never called
// with the server's mu held, so it cannot deadlock against connection changes.
type watchManager struct {
	backend     watchBackend
	notify      func(method string, params map[string]any) error
	revision    func() uint64
	serverID    string
	startWait   time.Duration
	stopTimeout time.Duration

	mu       sync.Mutex
	next     int
	watches  map[string]*watchRecord
	order    []string
	closed   bool
	endCount uint64
}

func newWatchManager(backend watchBackend, notify func(method string, params map[string]any) error, revision func() uint64) *watchManager {
	var id [4]byte
	_, _ = rand.Read(id[:])
	return &watchManager{backend: backend, notify: notify, revision: revision, serverID: hex.EncodeToString(id[:]), startWait: watchStartWait, stopTimeout: watchStopTimeout, watches: map[string]*watchRecord{}}
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
	rec := &watchRecord{id: fmt.Sprintf("w%d", m.next), spec: spec, revision: revision, state: watchPreparing, startedAt: time.Now(), changed: make(chan struct{}), settled: make(chan struct{})}
	m.watches[rec.id] = rec
	m.order = append(m.order, rec.id)
	m.mu.Unlock()

	handle, err := m.backend.Start(ctx, conn, spec)
	m.mu.Lock()
	if err != nil {
		m.forgetLocked(rec.id)
		if errors.As(err, new(watchUnconfirmedError)) {
			// A stop that ended this watch during the deploy must not report
			// it removed: the device may still create it.
			rec.removeErr = err
		}
		close(rec.settled)
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
	if !ended {
		close(rec.settled)
	}
	m.mu.Unlock()
	if ended {
		// The watch ended during the deploy, by watch_stop, a device change or
		// shutdown. None of them had the handle, so the removal is done here.
		m.sendStatus(rec, "")
		rec.removeErr = m.stopHandle(handle, m.stopTimeout)
		close(rec.settled)
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
		m.sendEvent(rec, event)
	case u.Status != nil && u.Status.State == watchEnded:
		handle := rec.handle
		m.endLocked(rec, u.Status.Reason)
		m.mu.Unlock()
		m.sendStatus(rec, "")
		m.stopHandle(handle, m.stopTimeout)
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
	m.endCount++
	rec.endSeq = m.endCount
	// Keep the most recently ended watches, so a client can still learn how
	// they ended; forget the earliest-ended first.
	for {
		var oldest *watchRecord
		count := 0
		for _, id := range m.order {
			if r := m.watches[id]; r.state == watchEnded {
				count++
				if oldest == nil || r.endSeq < oldest.endSeq {
					oldest = r
				}
			}
		}
		if count <= watchEndedRetained {
			return
		}
		m.forgetLocked(oldest.id)
	}
}

func (m *watchManager) stopHandle(handle watchHandle, timeout time.Duration) error {
	if handle == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return handle.Stop(ctx)
}

// stop ends a watch and removes it from the device. The record is ended and
// ENDED sent first, so no event arrives after watch_stop returns and a device
// that stopped answering cannot delay ENDED. The removal gets stopTimeout; the
// lease removes a campaign the device never hears about. A watch still
// deploying is removed by start once the deploy returns, and stop waits for
// that within the same bound.
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
	m.sendStatus(rec, "")
	stopCtx, cancel := context.WithTimeout(ctx, m.stopTimeout)
	defer cancel()
	if handle == nil {
		select {
		case <-rec.settled:
			return m.view(rec), rec.removeErr
		case <-stopCtx.Done():
			return m.view(rec), errWatchStillDeploying
		}
	}
	return m.view(rec), handle.Stop(stopCtx)
}

// endStale ends every watch started on an earlier connection. That connection
// is already closed, so nothing reaches the old device: its campaigns lapse
// within lease + 5 s (design §6.1). A late call for an older revision leaves
// watches started on newer connections alone.
func (m *watchManager) endStale(revision uint64) {
	m.mu.Lock()
	var ended []*watchRecord
	for _, id := range m.order {
		rec := m.watches[id]
		if rec.state != watchEnded && rec.revision < revision && rec.handle != nil {
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
// ENDED goes out exactly once and nothing follows it.
func (m *watchManager) sendStatus(rec *watchRecord, reason string) {
	rec.notifyMu.Lock()
	defer rec.notifyMu.Unlock()
	if rec.endedSent {
		return
	}
	m.mu.Lock()
	if rec.state == watchEnded {
		rec.endedSent = true
		reason = rec.reason
	} else if reason == "" {
		reason = rec.reason
	}
	params := map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "state": string(rec.state), "reason": reason, "camera": rec.spec.CameraName, "watching": rec.spec.Classes}
	missed := rec.missed
	if missed > 0 {
		params["missed_notifications"] = missed
		rec.missed = 0
	}
	m.mu.Unlock()
	m.deliverLocked(rec, watchStatusMethod, params, missed)
}

// sendEvent sends one event unless the watch has ended; the event stays in the
// buffer either way.
func (m *watchManager) sendEvent(rec *watchRecord, event watchEvent) {
	rec.notifyMu.Lock()
	defer rec.notifyMu.Unlock()
	m.mu.Lock()
	ended := rec.endedSent || rec.state == watchEnded
	m.mu.Unlock()
	if ended {
		return
	}
	m.deliverLocked(rec, watchEventMethod, map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "sequence": event.Sequence, "kind": event.Kind, "classes": event.Classes, "occurred_at": event.OccurredAt}, 0)
}

// deliverLocked sends one notification with rec.notifyMu held. mcp-go's stdio
// session queues 100 and fails a send when full, so this never blocks; the
// failure is reported with the watch's next status. carried is the missed
// count this notification reports: if it fails, that count is kept too.
func (m *watchManager) deliverLocked(rec *watchRecord, method string, params map[string]any, carried int) {
	if m.notify == nil {
		return
	}
	if err := m.notify(method, params); err != nil {
		m.mu.Lock()
		rec.missed += carried + 1
		m.mu.Unlock()
	}
}
