package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// ProgressUpdateMsg updates the progress bar percentage.
// Written and Total are optional; when both are non-zero the view renders
// a byte counter like "4.00%  (420.0 MB / 10.5 GB)".
type ProgressUpdateMsg struct {
	Percent float64
	Written int64
	Total   int64
}

// ProgressDoneMsg signals that the progress operation is complete.
type ProgressDoneMsg struct {
	Err error
}

// ProgressModel is a reusable Bubble Tea progress bar.
type ProgressModel struct {
	progress progress.Model
	title    string
	percent  float64
	written  int64
	total    int64
	done     bool
	err      error
}

// NewProgress creates a new ProgressModel with the given title.
func NewProgress(title string) ProgressModel {
	p := progress.New(progress.WithGradient(string(Emerald400), string(Emerald700)))
	p.PercentFormat = " %5.2f%%"
	return ProgressModel{
		progress: p,
		title:    title,
	}
}

// Init implements tea.Model.
func (m ProgressModel) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (m ProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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
		cmd := m.progress.SetPercent(msg.Percent)
		return m, cmd

	case ProgressDoneMsg:
		m.done = true
		m.err = msg.Err
		m.percent = 1.0
		return m, tea.Quit

	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		return m, cmd
	}

	return m, nil
}

// View implements tea.Model.
func (m ProgressModel) View() string {
	if m.done && m.err != nil {
		return fmt.Sprintf("Error: %v\n", m.err)
	}

	byteInfo := ""
	if m.written > 0 && m.total > 0 {
		byteInfo = fmt.Sprintf("  (%s / %s)", formatBytes(m.written), formatBytes(m.total))
	}

	if m.done {
		// Render at 100% directly — the animation may not have caught up
		// before tea.Quit was processed.
		if m.total > 0 {
			byteInfo = fmt.Sprintf("  (%s / %s)", formatBytes(m.total), formatBytes(m.total))
		}
		return fmt.Sprintf("%s\n%s%s\n", m.title, m.progress.ViewAs(1.0), byteInfo)
	}
	return fmt.Sprintf("%s\n%s%s\n", m.title, m.progress.ViewAs(m.percent), byteInfo)
}

// formatBytes returns a human-readable byte string.
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Err returns any error from the completed progress.
func (m ProgressModel) Err() error {
	return m.err
}
