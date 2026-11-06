package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

const maxProgressDetailLines = 4

// ProgressUpdateMsg updates the progress bar percentage.
// Written and Total are optional; when both are non-zero the view renders
// a byte counter like "4.00%  (420.0 MiB / 10.5 GiB)". When only Written is
// non-zero (total unknown, e.g. gzip streams) it renders "(420.0 MiB)".
type ProgressUpdateMsg struct {
	Percent float64
	Written int64
	Total   int64
	Title   string
	Detail  string
}

// ProgressDoneMsg signals that the progress operation is complete.
type ProgressDoneMsg struct {
	Err error
}

// ProgressModel is a reusable Bubble Tea progress bar.
type ProgressModel struct {
	progress progress.Model
	title    string
	details  []string
	hints    hintRotator
	width    int
	percent  float64
	written  int64
	total    int64
	done     bool
	err      error
	showErr  bool
}

func NewProgress(title string) ProgressModel {
	p := progress.New(progress.WithGradient(string(Emerald400), string(Emerald700)))
	p.PercentFormat = " %5.2f%%"
	return ProgressModel{
		progress: p,
		title:    title,
		hints:    newHintRotator(),
		showErr:  true,
	}
}

// WithoutErrorView suppresses inline error rendering in View. This is useful
// when the caller will surface the returned error separately and would
// otherwise print it twice.
func (m ProgressModel) WithoutErrorView() ProgressModel {
	m.showErr = false
	return m
}

// WithoutHints drops the rotating "Tip:" line, for progress whose context
// makes the tips noise, such as setting up the simulator.
func (m ProgressModel) WithoutHints() ProgressModel {
	m.hints = hintRotator{}
	return m
}

// Init implements tea.Model.
func (m ProgressModel) Init() tea.Cmd {
	return m.hints.tick()
}

// Update implements tea.Model.
func (m ProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.done = true
			m.err = context.Canceled
			return m, tea.Quit
		}

	case ProgressUpdateMsg:
		m.percent = msg.Percent
		m.written = msg.Written
		m.total = msg.Total
		if msg.Title != "" {
			m.title = msg.Title
		}
		if msg.Detail != "" {
			m.details = appendProgressDetail(m.details, msg.Detail)
		}
		cmd := m.progress.SetPercent(msg.Percent)
		return m, cmd

	case ProgressDoneMsg:
		m.done = true
		m.err = msg.Err
		m.percent = 1.0
		return m, tea.Quit

	case hintTickMsg:
		m.hints.next()
		return m, m.hints.tick()

	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		return m, cmd
	}

	return m, nil
}

// View implements tea.Model.
func (m ProgressModel) View() string {
	byteInfo := ""
	if m.written > 0 && m.total > 0 {
		byteInfo = fmt.Sprintf("  (%s / %s)", FormatBytes(m.written), FormatBytes(m.total))
	} else if m.written > 0 {
		byteInfo = fmt.Sprintf("  (%s)", FormatBytes(m.written))
	}
	details := m.detailView()

	if m.done && m.err != nil {
		if !m.showErr {
			percent := m.percent
			if percent >= 1.0 {
				percent = 0.99
			}
			return fmt.Sprintf("%s (failed)\n%s%s%s\n", m.title, details, m.progress.ViewAs(percent), byteInfo)
		}
		return fmt.Sprintf("Error: %v\n", m.err)
	}

	if m.done {
		// Render at 100% directly — the animation may not have caught up
		// before tea.Quit was processed.
		if m.total > 0 {
			byteInfo = fmt.Sprintf("  (%s / %s)", FormatBytes(m.total), FormatBytes(m.total))
		}
		return fmt.Sprintf("%s\n%s%s%s\n", m.title, details, m.progress.ViewAs(1.0), byteInfo)
	}
	out := fmt.Sprintf("%s\n%s%s%s\n", m.title, details, m.progress.ViewAs(m.percent), byteInfo)
	if hint := m.hints.view(m.width); hint != "" {
		out += hint + "\n"
	}
	return out
}

func appendProgressDetail(details []string, detail string) []string {
	if len(details) > 0 && details[len(details)-1] == detail {
		return details
	}
	details = append(details, detail)
	if len(details) > maxProgressDetailLines {
		details = details[len(details)-maxProgressDetailLines:]
	}
	return details
}

func (m ProgressModel) detailView() string {
	if len(m.details) == 0 {
		return ""
	}
	return strings.Join(m.details, "\n") + "\n"
}

// FormatBytes formats a byte count using binary units for progress displays.
func FormatBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Err returns any error from the completed progress.
func (m ProgressModel) Err() error {
	return m.err
}
