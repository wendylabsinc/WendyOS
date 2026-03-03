package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// ProgressUpdateMsg updates the progress bar percentage.
type ProgressUpdateMsg struct {
	Percent float64
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
	done     bool
	err      error
}

// NewProgress creates a new ProgressModel with the given title.
func NewProgress(title string) ProgressModel {
	p := progress.New(progress.WithDefaultGradient())
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
			return m, tea.Quit
		}

	case ProgressUpdateMsg:
		m.percent = msg.Percent
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
	return fmt.Sprintf("%s\n%s\n", m.title, m.progress.View())
}

// Err returns any error from the completed progress.
func (m ProgressModel) Err() error {
	return m.err
}
