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

// SpinnerUpdateMsg updates the label shown next to the spinner.
type SpinnerUpdateMsg struct {
	Label string
}

// SpinnerModel is a reusable Bubble Tea spinner that runs until it receives a SpinnerDoneMsg.
type SpinnerModel struct {
	spinner  spinner.Model
	title    string
	hints    hintRotator
	width    int
	done     bool
	err      error
	result   interface{}
	quitting bool
}

func NewSpinner(title string) SpinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)
	return SpinnerModel{
		spinner: s,
		title:   title,
		hints:   newHintRotator(),
	}
}

// WithoutHints drops the rotating "Tip:" line; see ProgressModel.WithoutHints.
func (m SpinnerModel) WithoutHints() SpinnerModel {
	m.hints = hintRotator{}
	return m
}

// Init implements tea.Model.
func (m SpinnerModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.hints.tick())
}

// Update implements tea.Model.
func (m SpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

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

	case SpinnerUpdateMsg:
		m.title = msg.Label
		return m, nil

	case hintTickMsg:
		m.hints.next()
		return m, m.hints.tick()

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
	out := fmt.Sprintf("%s %s\n", m.spinner.View(), m.title)
	if hint := m.hints.view(m.width); hint != "" {
		out += hint + "\n"
	}
	return out
}

func (m SpinnerModel) Result() (interface{}, error) {
	return m.result, m.err
}

// Done returns whether the spinner has completed.
func (m SpinnerModel) Done() bool {
	return m.done
}
