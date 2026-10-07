package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// WatchControl is the session's camera watches as the TUI uses them.
type WatchControl interface {
	WatchNotices() <-chan WatchNotice
	ListWatches(ctx context.Context) ([]WatchInfo, error)
	StopAllWatches(ctx context.Context) (int, error)
}

// watchDisplay is what the TUI knows about one watch.
type watchDisplay struct {
	label, camera string
	classes       []string
	state         string
	startState    string // what watch_start returned, from its tool result
	errorTurned   bool   // a turn already reported this watch's ERROR
	readyTurned   bool   // a turn already reported its READY
}

type watchNoticeMessage struct{ notice WatchNotice }
type watchPaceMessage struct{}
type watchListMessage struct {
	watches []WatchInfo
	err     error
}
type watchStopMessage struct {
	stopped int
	err     error
}

// waitForWatchNotice is re-armed after every notice, as the voice wait is.
func (m *chatModel) waitForWatchNotice() tea.Cmd {
	notices, ctx := m.watchNotices, m.ctx
	if notices == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case notice := <-notices:
			return watchNoticeMessage{notice}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *chatModel) watchFor(n WatchNotice) *watchDisplay {
	d := m.watches[n.WatchID]
	if d == nil {
		d = &watchDisplay{}
		m.watches[n.WatchID] = d
	}
	if n.Label != "" {
		d.label = n.Label
	}
	if n.Camera != "" {
		d.camera = n.Camera
	}
	if len(n.Watching) > 0 {
		d.classes = n.Watching
	}
	if n.Method == watchStatusMethod {
		d.state = n.State
	}
	return d
}

// handleWatchNotice shows a notice as one event line.
func (m *chatModel) handleWatchNotice(n WatchNotice) tea.Cmd {
	d := m.watchFor(n)
	if missed := n.Missed + n.MissedByServer; missed > 0 {
		m.appendEntry("event", "", fmt.Sprintf("· %d watch notification(s) were missed", missed))
	}
	m.appendEntry("event", "", watchNoticeLine(n, d, m.now()))
	m.watchQueue = append(m.watchQueue, n)
	return m.maybeStartWatchTurn()
}

// An event turn starts at most this often; later reports merge into it.
const watchTurnSpacing = 10 * time.Second

// watchTurnRefusal is the tool result for a call that needs approval in an
// event turn. Nobody asked for the turn, so nobody is asked to approve it,
// and --yes does not cover it (design §7.3).
const watchTurnRefusal = "Not run: a watch report started this turn, not the user. Tell the user what you would do; they can ask you to."

// watchTriggers reports whether n starts an event turn (design §7.2). Other
// notices are shown and folded into the next event turn. It is evaluated when
// a turn could start, after watch_start's own result has been seen.
func (m *chatModel) watchTriggers(n WatchNotice) bool {
	d := m.watches[n.WatchID]
	switch {
	case n.Method == watchEventMethod:
		return n.Kind == "entered"
	case n.State == "ENDED":
		return n.Reason != watchStoppedReason
	case n.State == "ERROR":
		return d != nil && !d.errorTurned && d.startState != "ERROR"
	case n.State == "READY":
		return d != nil && !d.readyTurned && d.startState == "PREPARING"
	}
	return false
}

// maybeStartWatchTurn starts an event turn when chat is idle, nothing typed is
// waiting, the queue holds a trigger, and the last event turn was at least
// watchTurnSpacing ago. Otherwise a pacing tick comes back here later.
func (m *chatModel) maybeStartWatchTurn() tea.Cmd {
	if m.active || m.quitting || m.clearAfterTurn || len(m.queuedPrompts) > 0 || m.pendingDelegation != nil {
		return nil
	}
	triggered := false
	for _, n := range m.watchQueue {
		triggered = triggered || m.watchTriggers(n)
	}
	if !triggered {
		return nil
	}
	if wait := watchTurnSpacing - m.now().Sub(m.lastWatchTurn); wait > 0 {
		if m.watchPacing {
			return nil
		}
		m.watchPacing = true
		return tea.Tick(wait, func(time.Time) tea.Msg { return watchPaceMessage{} })
	}
	items := m.watchQueue
	m.watchQueue = nil
	for _, n := range items {
		if d := m.watches[n.WatchID]; d != nil {
			d.errorTurned = d.errorTurned || n.State == "ERROR"
			d.readyTurned = d.readyTurned || n.State == "READY"
		}
	}
	m.lastWatchTurn = m.now()
	return m.startEventTurn(m.watchEventPrompt(items))
}

// startEventTurn runs a turn for watch reports. The event lines already in the
// transcript stand in for a "You" entry, and the turn skips memory: its text is
// not a statement from the user (design §7.3). It is not an interruption
// either, so the previous reply keeps speaking.
func (m *chatModel) startEventTurn(prompt string) tea.Cmd {
	cmd := m.beginTurn(prompt, true)
	m.viewport.GotoBottom()
	return cmd
}

// watchEventPrompt is an event turn's prompt (design §7.3). The label and the
// camera name come from the model, the user or the device, so they are
// JSON-quoted like the event itself.
func (m *chatModel) watchEventPrompt(items []WatchNotice) string {
	const ask = "\nTell the user if this is what they asked to be alerted about.\nTools that need approval are not available in this turn."
	if len(items) == 1 {
		d := m.watches[items[0].WatchID]
		if d == nil {
			d = &watchDisplay{label: items[0].Label}
		}
		header := "Your watch " + jsonQuote(d.label) + " (" + strings.Join(d.classes, ", ") + ", camera " + jsonQuote(d.camera) + ") reported:\n"
		return header + UntrustedJSONBlock(watchNoticeData(items[0])) + ask
	}
	payload := make([]map[string]any, 0, len(items))
	for _, n := range items {
		payload = append(payload, watchNoticeData(n))
	}
	return "Your watches reported:\n" + UntrustedJSONBlock(payload) + ask
}

func watchNoticeData(n WatchNotice) map[string]any {
	data := map[string]any{"watch_id": n.WatchID, "watch": n.Label}
	if n.Method == watchEventMethod {
		data["kind"], data["classes"], data["occurred_at"] = n.Kind, n.Classes, n.OccurredAt
	} else {
		data["state"], data["reason"] = n.State, n.Reason
	}
	return data
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// noteWatchStart records what watch_start returned, from its tool result, so
// a later READY or ERROR is judged against it.
func (m *chatModel) noteWatchStart(text string) {
	var result struct {
		WatchID string `json:"watch_id"`
		Label   string `json:"label"`
		State   string `json:"state"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &result) != nil || result.WatchID == "" {
		return
	}
	d := m.watches[result.WatchID]
	if d == nil {
		d = &watchDisplay{label: result.Label}
		m.watches[result.WatchID] = d
	}
	d.startState = result.State
}

// watchNoticeLine formats a notice: "· 14:02:11  front door  person 0.91 entered".
func watchNoticeLine(n WatchNotice, d *watchDisplay, now time.Time) string {
	at := now
	if t, err := time.Parse(time.RFC3339Nano, n.OccurredAt); err == nil {
		at = t
	}
	prefix := "· " + at.Local().Format("15:04:05") + "  " + chatSingleLine(d.label) + "  "
	if n.Method == watchEventMethod {
		parts := make([]string, 0, len(n.Classes))
		for _, class := range n.Classes {
			parts = append(parts, fmt.Sprintf("%s %.2f", chatSingleLine(class.Label), class.Score))
		}
		what := strings.Join(parts, ", ")
		if what == "" {
			what = "detection"
		}
		return prefix + what + " " + chatSingleLine(n.Kind)
	}
	line := prefix + strings.ToLower(chatSingleLine(n.State))
	if n.Reason != "" {
		line += ": " + chatSingleLine(n.Reason)
	}
	return line
}

func (m *chatModel) activeWatchCount() int {
	count := 0
	for _, d := range m.watches {
		if d.state != "" && d.state != "ENDED" {
			count++
		}
	}
	return count
}

func (m *chatModel) listWatches() tea.Cmd {
	control, ctx := m.opts.Watches, m.ctx
	if control == nil {
		m.appendEntry("notice", "Watches", "Camera watches are not available in this session.")
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		watches, err := control.ListWatches(ctx)
		return watchListMessage{watches, err}
	}
}

func (m *chatModel) stopAllWatches() tea.Cmd {
	control, ctx := m.opts.Watches, m.ctx
	if control == nil {
		m.appendEntry("notice", "Watches", "Camera watches are not available in this session.")
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		stopped, err := control.StopAllWatches(ctx)
		return watchStopMessage{stopped, err}
	}
}

func (m *chatModel) showWatchList(msg watchListMessage) {
	if msg.err != nil {
		m.appendEntry("error", "Could not list watches", msg.err.Error())
		return
	}
	if len(msg.watches) == 0 {
		m.appendEntry("notice", "Watches", "No camera watches in this session. Ask Wendy to watch a camera, for example: tell me when someone comes to the door.")
		return
	}
	var lines []string
	for _, w := range msg.watches {
		line := fmt.Sprintf("%s  %s  %s · %s  %s", w.WatchID, w.Label, strings.Join(w.Classes, ", "), w.CameraName, w.State)
		if w.Reason != "" {
			line += ": " + w.Reason
		}
		if t, err := time.Parse(time.RFC3339Nano, w.LastEventAt); err == nil {
			line += "  last event " + t.Local().Format("15:04:05")
		}
		lines = append(lines, line)
	}
	m.appendEntry("notice", "Watches", strings.Join(lines, "\n"))
}

func (m *chatModel) showWatchStop(msg watchStopMessage) {
	text := fmt.Sprintf("Stopped %d watch(es).", msg.stopped)
	if msg.err != nil {
		m.appendEntry("error", "Could not stop every watch", text+" "+msg.err.Error())
		return
	}
	m.appendEntry("notice", "Watches", text)
}
