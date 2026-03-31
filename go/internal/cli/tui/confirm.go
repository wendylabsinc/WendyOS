package tui

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ConfirmModel is a Bubble Tea model for styled yes/no prompts.
type ConfirmModel struct {
	Question string
	choice   bool // true = Yes, false = No
	answered bool
	quitting bool
}

// NewConfirm creates a new confirm model defaulting to No.
func NewConfirm(question string) ConfirmModel {
	return ConfirmModel{Question: question}
}

func (m ConfirmModel) Init() tea.Cmd { return nil }

func (m ConfirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			m.choice = true
			m.answered = true
			return m, tea.Quit
		case "n", "N":
			m.choice = false
			m.answered = true
			return m, tea.Quit
		case "enter":
			m.answered = true
			return m, tea.Quit
		case "left", "h":
			m.choice = true
			return m, nil
		case "right", "l":
			m.choice = false
			return m, nil
		case "tab":
			m.choice = !m.choice
			return m, nil
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		}
	}
	return m, nil
}

var (
	confirmQuestion = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	confirmActive   = lipgloss.NewStyle().Bold(true).Foreground(ColorSelectedFg).Background(ColorSelectedBg).Padding(0, 1)
	confirmInactive = lipgloss.NewStyle().Foreground(ColorDim).Padding(0, 1)
	confirmHint     = lipgloss.NewStyle().Foreground(ColorDim)
)

func (m ConfirmModel) View() string {
	if m.answered || m.quitting {
		return ""
	}

	yes := confirmInactive.Render("Yes")
	no := confirmInactive.Render("No")
	if m.choice {
		yes = confirmActive.Render("Yes")
	} else {
		no = confirmActive.Render("No")
	}

	return fmt.Sprintf(
		"%s  %s %s  %s\n",
		confirmQuestion.Render(m.Question),
		yes, no,
		confirmHint.Render("(y/n)"),
	)
}

// Confirmed returns true if the user selected Yes.
func (m ConfirmModel) Confirmed() bool {
	return m.answered && m.choice
}

// Cancelled returns true if the user quit without answering (Ctrl+C / q).
func (m ConfirmModel) Cancelled() bool {
	return m.quitting
}

// Confirm runs a styled yes/no prompt and returns the user's choice.
// Optional programOpts are passed to tea.NewProgram (useful for testing with
// tea.WithInput / tea.WithOutput).
func Confirm(question string, programOpts ...tea.ProgramOption) (bool, error) {
	m := NewConfirm(question)
	p := tea.NewProgram(m, programOpts...)
	result, err := p.Run()
	if err != nil {
		return false, fmt.Errorf("confirm prompt: %w", err)
	}
	model, ok := result.(ConfirmModel)
	if !ok {
		return false, fmt.Errorf("confirm prompt: unexpected model type %T", result)
	}
	if model.Cancelled() {
		return false, ErrCancelled
	}
	return model.Confirmed(), nil
}

// ConfirmWithIO runs a styled yes/no prompt reading from r and discarding
// output. This is useful for non-TTY environments such as tests.
func ConfirmWithIO(question string, r io.Reader) (bool, error) {
	return Confirm(question, tea.WithInput(r), tea.WithOutput(io.Discard))
}
