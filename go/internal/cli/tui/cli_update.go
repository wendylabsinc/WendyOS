package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// cliUpdateModel keeps the update decision visible until the user responds.
// The alternate screen preserves the completed command's output.
type cliUpdateModel struct {
	current, latest string
	width, height   int
	later           bool
	answered        bool
	cancelled       bool
}

func (m cliUpdateModel) Init() tea.Cmd { return nil }

func (m cliUpdateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "shift+tab", "left", "right", "up", "down":
			m.later = !m.later
		case "enter":
			m.answered = true
			return m, tea.Quit
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m cliUpdateModel) View() string {
	if m.answered || m.cancelled {
		return ""
	}
	width := 56
	if m.width > 0 {
		width = min(width, max(1, m.width-6))
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorNotice).Render("UPDATE YOUR WENDY CLI")
	download, later := "> Download now", "  Ask again later..."
	if m.later {
		download, later = "  Download now", "> Ask again later..."
		download, later = confirmInactive.Render(download), confirmActive.Render(later)
	} else {
		download, later = confirmActive.Render(download), confirmInactive.Render(later)
	}
	hint := confirmHint.Render("Arrow keys / Tab to choose. Enter to confirm.")
	body := fmt.Sprintf("%s\n\nA new release is ready to download.\n\nInstalled: %s\nAvailable: %s\n\n%s\n%s\n\nAsk again later reminds you in 24 hours.\n\n%s", title, m.current, m.latest, download, later, hint)
	style := lipgloss.NewStyle().Width(width).Padding(1, 2).
		Border(lipgloss.ThickBorder()).BorderForeground(ColorNotice)
	dialog := style.Render(body)
	if m.height > 0 && lipgloss.Height(dialog) > m.height {
		// Bubble Tea drops the top lines of a view taller than the terminal,
		// so short terminals get a compact dialog that keeps the versions.
		dialog = style.Padding(0, 2).Render(fmt.Sprintf("%s\nInstalled: %s\nAvailable: %s\n%s\n%s\n%s", title, m.current, m.latest, download, later, hint))
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

// PromptCLIUpdate offers an immediate update or a one-day reminder.
func PromptCLIUpdate(current, latest string, opts ...tea.ProgramOption) (bool, error) {
	options := append([]tea.ProgramOption{tea.WithAltScreen()}, opts...)
	result, err := tea.NewProgram(cliUpdateModel{current: current, latest: latest}, options...).Run()
	if err != nil {
		return false, fmt.Errorf("CLI update prompt: %w", err)
	}
	model, ok := result.(cliUpdateModel)
	if !ok {
		return false, fmt.Errorf("CLI update prompt: unexpected model type %T", result)
	}
	if model.cancelled || !model.answered {
		return false, ErrCancelled
	}
	return !model.later, nil
}
