package chat

import (
	"context"
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
	return nil
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
