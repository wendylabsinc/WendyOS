package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ValidateFunc validates user input. Return nil on success, or an error
// whose message will be shown inline so the user can retry.
type ValidateFunc func(value string) error

// TextPromptModel is a Bubble Tea model for a single-line text input with
// inline validation. Invalid input shows an error and lets the user edit
// and resubmit. Press q or ctrl+c to quit.
type TextPromptModel struct {
	Prompt   string
	Hint     string // shown dimmed after the prompt (e.g. "e.g. /dev/i2c-1")
	Validate ValidateFunc

	input    textinput.Model
	err      string // current validation error, empty when valid
	done     bool
	quitting bool
}

// NewTextPrompt creates a new text prompt with an optional validation function.
func NewTextPrompt(prompt, hint string, validate ValidateFunc) TextPromptModel {
	ti := textinput.New()
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 50
	ti.PromptStyle = lipgloss.NewStyle().Foreground(ColorPrimary)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	return TextPromptModel{
		Prompt:   prompt,
		Hint:     hint,
		Validate: validate,
		input:    ti,
	}
}

func (m TextPromptModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m TextPromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			value := strings.TrimSpace(m.input.Value())
			if m.Validate != nil {
				if err := m.Validate(value); err != nil {
					m.err = err.Error()
					return m, nil
				}
			}
			m.err = ""
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		default:
			// Clear error when user starts editing.
			m.err = ""
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

var (
	tpLabel = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	tpHint  = lipgloss.NewStyle().Foreground(ColorDim)
	tpError = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")) // red-500
)

func (m TextPromptModel) View() string {
	if m.done || m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(tpLabel.Render(m.Prompt))
	if m.Hint != "" {
		sb.WriteString("  " + tpHint.Render(m.Hint))
	}
	sb.WriteString("\n")
	sb.WriteString(m.input.View())
	sb.WriteString("\n")

	if m.err != "" {
		sb.WriteString(tpError.Render("Error: "+m.err) + "\n")
	}

	return sb.String()
}

// Value returns the trimmed text the user entered.
func (m TextPromptModel) Value() string {
	return strings.TrimSpace(m.input.Value())
}

// Cancelled returns true if the user quit without submitting.
func (m TextPromptModel) Cancelled() bool {
	return m.quitting
}

// PromptText runs an interactive text prompt with validation. Returns the
// validated value or an error on cancellation / Bubble Tea failure.
func PromptText(prompt, hint string, validate ValidateFunc, programOpts ...tea.ProgramOption) (string, error) {
	m := NewTextPrompt(prompt, hint, validate)
	p := tea.NewProgram(m, programOpts...)
	result, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("text prompt: %w", err)
	}
	model, ok := result.(TextPromptModel)
	if !ok {
		return "", fmt.Errorf("text prompt: unexpected model type %T", result)
	}
	if model.Cancelled() {
		return "", fmt.Errorf("text prompt: cancelled")
	}
	return model.Value(), nil
}
