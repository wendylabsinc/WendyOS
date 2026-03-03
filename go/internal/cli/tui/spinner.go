// Package tui provides reusable Bubble Tea models for the Wendy CLI.
package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SpinnerDoneMsg signals that the async work is complete.
type SpinnerDoneMsg struct {
	Result interface{}
	Err    error
}

// SpinnerModel is a reusable Bubble Tea spinner that runs until it receives a SpinnerDoneMsg.
type SpinnerModel struct {
	spinner  spinner.Model
	title    string
	done     bool
	err      error
	result   interface{}
	quitting bool
}

// NewSpinner creates a new SpinnerModel with the given title.
func NewSpinner(title string) SpinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	return SpinnerModel{
		spinner: s,
		title:   title,
	}
}

// Init implements tea.Model.
func (m SpinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
}

// Update implements tea.Model.
func (m SpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}

	case SpinnerDoneMsg:
		m.done = true
		m.result = msg.Result
		m.err = msg.Err
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

// View implements tea.Model.
func (m SpinnerModel) View() string {
	if m.quitting {
		return ""
	}
	if m.done {
		if m.err != nil {
			return fmt.Sprintf("Error: %v\n", m.err)
		}
		return ""
	}
	return fmt.Sprintf("%s %s\n", m.spinner.View(), m.title)
}

// Result returns the result and error from the completed spinner.
func (m SpinnerModel) Result() (interface{}, error) {
	return m.result, m.err
}

// Done returns whether the spinner has completed.
func (m SpinnerModel) Done() bool {
	return m.done
}
