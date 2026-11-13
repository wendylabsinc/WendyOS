package tui

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ConfirmModel is a Bubble Tea model for styled yes/no prompts.
type ConfirmModel struct {
	Question string
	choice   bool // true = Yes, false = No
	answered bool
	quitting bool
	// requireExplicit disables a default answer: the prompt resolves only on a
	// y/n keypress. Enter, arrows, and tab are ignored, and no option is
	// pre-highlighted. Ctrl+C / q still cancels.
	requireExplicit bool
	// danger renders the question in the error (red) style to flag a
	// security-sensitive decision the user must look at carefully.
	danger bool
	// compact uses a single [y/N] hint and ignores option navigation.
	compact bool
}

func NewConfirm(question string) ConfirmModel {
	return ConfirmModel{Question: question}
}

// NewConfirmDefaultNo builds a compact prompt defaulting to No.
func NewConfirmDefaultNo(question string) ConfirmModel {
	return ConfirmModel{Question: question, compact: true}
}

func NewConfirmDefaultYes(question string) ConfirmModel {
	return ConfirmModel{Question: question, choice: true}
}

// NewConfirmNoDefaultDanger builds a no-default prompt whose question is
// rendered in red, for security-sensitive decisions that must catch attention.
func NewConfirmNoDefaultDanger(question string) ConfirmModel {
	return ConfirmModel{Question: question, requireExplicit: true, danger: true}
}

// NewConfirmNoDefault builds a prompt with no default answer. The user must
// press y or n; Enter does nothing.
func NewConfirmNoDefault(question string) ConfirmModel {
	return ConfirmModel{Question: question, requireExplicit: true}
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
			if m.requireExplicit {
				return m, nil // no default; must press y or n
			}
			m.answered = true
			return m, tea.Quit
		case "left", "h":
			if m.requireExplicit || m.compact {
				return m, nil
			}
			m.choice = true
			return m, nil
		case "right", "l":
			if m.requireExplicit || m.compact {
				return m, nil
			}
			m.choice = false
			return m, nil
		case "tab":
			if m.requireExplicit || m.compact {
				return m, nil
			}
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
	confirmDanger   = lipgloss.NewStyle().Bold(true).Foreground(ColorError)
	confirmActive   = lipgloss.NewStyle().Bold(true).Foreground(ColorSelectedFg).Background(ColorSelectedBg).Padding(0, 1)
	confirmInactive = lipgloss.NewStyle().Foreground(ColorDim).Padding(0, 1)
	confirmHint     = lipgloss.NewStyle().Foreground(ColorDim)
)

func (m ConfirmModel) View() string {
	if m.answered || m.quitting {
		return ""
	}

	if m.compact {
		return fmt.Sprintf("%s %s\n", confirmQuestion.Render(m.Question), confirmHint.Render("[y/N]"))
	}

	if m.requireExplicit {
		// No default: render the question with a neutral hint and no
		// pre-highlighted option. Danger prompts render the question in red.
		qStyle := confirmQuestion
		if m.danger {
			qStyle = confirmDanger
		}
		return fmt.Sprintf(
			"%s  %s\n",
			qStyle.Render(m.Question),
			confirmHint.Render("[y/n]"),
		)
	}

	yes, no := confirmChoice("Yes", m.choice), confirmChoice("No", !m.choice)

	return fmt.Sprintf(
		"%s  %s %s  %s\n",
		confirmQuestion.Render(m.Question),
		yes, no,
		confirmHint.Render("(y/n)"),
	)
}

// confirmChoice renders one option. Without colour the highlight is plain
// text, so brackets take the place of its padding.
func confirmChoice(label string, active bool) string {
	switch {
	case !active:
		return confirmInactive.Render(label)
	case lipgloss.ColorProfile() == termenv.Ascii:
		return "[" + label + "]"
	}
	return confirmActive.Render(label)
}

// Confirmed returns true if the user selected Yes.
func (m ConfirmModel) Confirmed() bool {
	return m.answered && m.choice
}

// Cancelled returns true if the user quit without answering (Ctrl+C / q).
func (m ConfirmModel) Cancelled() bool {
	return m.quitting
}

func runConfirm(m ConfirmModel, programOpts []tea.ProgramOption) (bool, error) {
	p := NewProgram(m, programOpts...)
	result, err := RunProgram(p)
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

// Confirm runs a styled yes/no prompt defaulting to No.
// Optional programOpts are passed to tea.NewProgram (useful for testing with
// tea.WithInput / tea.WithOutput).
func Confirm(question string, programOpts ...tea.ProgramOption) (bool, error) {
	return runConfirm(NewConfirm(question), programOpts)
}

// ConfirmDefaultNo runs a compact [y/N] prompt defaulting to No.
// Pass the question without the [y/N] suffix. Returns ErrCancelled on Ctrl+C / q.
func ConfirmDefaultNo(question string, programOpts ...tea.ProgramOption) (bool, error) {
	return runConfirm(NewConfirmDefaultNo(question), programOpts)
}

// ConfirmDefaultYes runs a styled yes/no prompt defaulting to Yes.
func ConfirmDefaultYes(question string, programOpts ...tea.ProgramOption) (bool, error) {
	return runConfirm(NewConfirmDefaultYes(question), programOpts)
}

// ConfirmNoDefault runs a styled yes/no prompt with no default answer: the user
// must press y or n. Returns ErrCancelled on Ctrl+C / q.
func ConfirmNoDefault(question string, programOpts ...tea.ProgramOption) (bool, error) {
	return runConfirm(NewConfirmNoDefault(question), programOpts)
}

// ConfirmNoDefaultDanger is ConfirmNoDefault with the question rendered in red,
// for security-sensitive decisions that must catch the user's attention.
func ConfirmNoDefaultDanger(question string, programOpts ...tea.ProgramOption) (bool, error) {
	return runConfirm(NewConfirmNoDefaultDanger(question), programOpts)
}

// ConfirmWithIO runs a styled yes/no prompt reading from r and discarding
// output. This is useful for non-TTY environments such as tests.
func ConfirmWithIO(question string, r io.Reader) (bool, error) {
	return Confirm(question, tea.WithInput(r), tea.WithOutput(io.Discard))
}
