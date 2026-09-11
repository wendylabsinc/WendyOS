package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Each step uses the normal screen, leaving a short confirmation in the
// terminal when complete. API keys never appear in those confirmations.
type terminalSetupUI struct {
	input  io.Reader
	output io.Writer
}

func (ui terminalSetupUI) options(ctx context.Context) []tea.ProgramOption {
	options := []tea.ProgramOption{tea.WithContext(ctx)}
	if ui.input != nil {
		options = append(options, tea.WithInput(ui.input))
	}
	if ui.output != nil {
		options = append(options, tea.WithOutput(ui.output))
	}
	return options
}

func (ui terminalSetupUI) Choose(ctx context.Context, title, description string, choices []setupChoice) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(choices) == 0 {
		return "", fmt.Errorf("no choices available for %s", chatSingleLine(title))
	}
	m := newSetupChoiceModel(title, description, choices)
	_, err := tea.NewProgram(m, ui.options(ctx)...).Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if m.cancelled || !m.done {
		return "", tui.ErrCancelled
	}
	return m.choices[m.filtered[m.selected]].ID, nil
}

func (ui terminalSetupUI) Text(ctx context.Context, title, description, initial string, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m := newSetupTextModel(title, description, initial, secret)
	_, err := tea.NewProgram(m, ui.options(ctx)...).Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if m.cancelled || !m.done {
		return "", tui.ErrCancelled
	}
	return strings.TrimSpace(m.input.Value()), nil
}

func (ui terminalSetupUI) Work(ctx context.Context, title string, fn func(context.Context, func(string)) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workCtx, cancel := context.WithCancel(ctx)
	progress := make(chan string, 1)
	done := make(chan error, 1)
	exited := make(chan struct{})
	var workerErr error
	go func() {
		defer close(exited)
		workerErr = fn(workCtx, func(message string) {
			// Progress is advisory. Keep the worker responsive even if rendering
			// is slower than a download, or the program is shutting down.
			select {
			case progress <- message:
			default:
			}
		})
		done <- workerErr
	}()
	defer func() {
		cancel()
		<-exited
	}()
	m := newSetupWorkModel(title, cancel, progress, done)
	_, err := tea.NewProgram(m, ui.options(ctx)...).Run()
	// Always join the worker before reporting a result or starting a retry.
	cancel()
	<-exited
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.cancelled {
		return tui.ErrCancelled
	}
	if err != nil {
		return err
	}
	return workerErr
}

func (ui terminalSetupUI) Note(message string) {
	output := ui.output
	if output == nil {
		output = os.Stdout
	}
	fmt.Fprintln(output, chatSanitize(message))
}

type setupChoiceModel struct {
	title, description string
	choices            []setupChoice
	filtered           []int
	selected           int
	search             textinput.Model
	searchable         bool
	viewport           viewport.Model
	width, height      int
	done, cancelled    bool
}

func newSetupChoiceModel(title, description string, choices []setupChoice) *setupChoiceModel {
	search := textinput.New()
	search.Prompt = "Search: "
	search.Placeholder = "type to filter models"
	search.PromptStyle = chatTitle
	search.Cursor.Style = chatTitle
	m := &setupChoiceModel{
		title: title, description: description, choices: choices,
		search: search, searchable: len(choices) > 8, viewport: viewport.New(1, 1),
		width: 80, height: 24,
	}
	m.viewport.KeyMap = viewport.KeyMap{}
	if m.searchable {
		m.search.Focus()
	}
	m.refilter()
	return m
}

func (m *setupChoiceModel) Init() tea.Cmd {
	if m.searchable {
		return textinput.Blink
	}
	return nil
}

func (m *setupChoiceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.refresh()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			if len(m.filtered) > 0 {
				m.done = true
				return m, tea.Quit
			}
			return m, nil
		case "up", "down", "pgup", "pgdown", "ctrl+home", "ctrl+end":
			switch msg.String() {
			case "up":
				m.selected--
			case "down":
				m.selected++
			case "pgup":
				m.selected -= max(1, m.viewport.Height/2)
			case "pgdown":
				m.selected += max(1, m.viewport.Height/2)
			case "ctrl+home":
				m.selected = 0
			case "ctrl+end":
				m.selected = len(m.filtered) - 1
			}
			m.selected = max(0, min(m.selected, len(m.filtered)-1))
			m.refresh()
			return m, nil
		}
		if msg.Type == tea.KeyRunes {
			msg.Runes = []rune(tui.StripControl(ansi.Strip(string(msg.Runes))))
		}
		if m.searchable {
			previous := m.search.Value()
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			if previous != m.search.Value() {
				m.refilter()
			}
			return m, cmd
		}
	}
	if m.searchable {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *setupChoiceModel) refilter() {
	m.filtered = nil
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	for i, choice := range m.choices {
		if strings.Contains(strings.ToLower(chatSingleLine(choice.ID+" "+choice.Label+" "+choice.Detail)), query) {
			m.filtered = append(m.filtered, i)
		}
	}
	m.selected = 0
	m.viewport.GotoTop()
	m.refresh()
}

func (m *setupChoiceModel) header() string {
	width := setupContentWidth(m.width)
	result := setupHeader(m.title, m.description, width)
	if m.searchable {
		result += "\n" + m.search.View()
	}
	return result + "\n"
}

func (m *setupChoiceModel) refresh() {
	width := setupContentWidth(m.width)
	m.search.Width = max(1, width-ansi.StringWidth(m.search.Prompt)-1)
	m.viewport.Width = width
	m.viewport.Height = max(1, min(16, m.height-strings.Count(m.header(), "\n")-3))
	var rows []string
	selectedStart, selectedEnd := 0, 0
	for row, index := range m.filtered {
		choice := m.choices[index]
		label := "  " + chatSingleLine(choice.Label)
		if row == m.selected {
			selectedStart = len(rows)
			label = chatTitle.Render("› " + chatSingleLine(choice.Label))
		}
		rows = append(rows, ansi.Truncate(label, width, "…"))
		if detail := chatSanitize(choice.Detail); detail != "" {
			for _, line := range strings.Split(ansi.Wrap(detail, max(1, width-2), ""), "\n") {
				rows = append(rows, chatDim.Render("  "+line))
			}
		}
		if row == m.selected {
			selectedEnd = len(rows)
		}
	}
	if len(rows) == 0 {
		rows = strings.Split(ansi.Wrap("No matching models. Edit the search to try again.", width, ""), "\n")
	}
	m.viewport.Height = min(m.viewport.Height, len(rows))
	m.viewport.SetContent(strings.Join(rows, "\n"))
	if selectedStart < m.viewport.YOffset || selectedEnd-selectedStart >= m.viewport.Height {
		m.viewport.SetYOffset(selectedStart)
	} else if selectedEnd > m.viewport.YOffset+m.viewport.Height {
		m.viewport.SetYOffset(selectedEnd - m.viewport.Height)
	}
}

func (m *setupChoiceModel) View() string {
	if m.cancelled {
		return ""
	}
	if m.done {
		return setupFrame(chatTitle.Render("✓ ")+chatSingleLine(m.choices[m.filtered[m.selected]].Label), m.width, m.height) + "\n"
	}
	hints := "↑/↓ choose · Enter continue · Esc cancel"
	if m.searchable {
		hints = fmt.Sprintf("%d of %d · type to search · ↑/↓ choose · Enter continue · Esc cancel", len(m.filtered), len(m.choices))
	}
	return setupFrame(m.header()+m.viewport.View()+"\n"+chatDim.Render(ansi.Wrap(hints, setupContentWidth(m.width), "")), m.width, m.height)
}

type setupTextModel struct {
	title, description string
	input              textinput.Model
	secret             bool
	width, height      int
	done, cancelled    bool
}

func newSetupTextModel(title, description, initial string, secret bool) *setupTextModel {
	input := textinput.New()
	input.Prompt = "› "
	input.PromptStyle = chatTitle
	input.Cursor.Style = chatTitle
	input.Focus()
	input.Width = 74
	if secret {
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '•'
	} else {
		input.SetValue(chatSingleLine(initial))
		input.CursorEnd()
	}
	return &setupTextModel{title: title, description: description, input: input, secret: secret, width: 80, height: 24}
}

func (m *setupTextModel) Init() tea.Cmd { return textinput.Blink }

func (m *setupTextModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.input.Width = max(1, setupContentWidth(m.width)-3)
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelled = true
			m.input.Reset()
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		}
		if msg.Type == tea.KeyRunes {
			msg.Runes = []rune(tui.StripControl(ansi.Strip(string(msg.Runes))))
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *setupTextModel) View() string {
	if m.cancelled {
		return ""
	}
	if m.done {
		confirmation := chatSingleLine(m.title)
		if !m.secret && m.input.Value() != "" {
			confirmation += ": " + chatSingleLine(m.input.Value())
		}
		return setupFrame(chatTitle.Render("✓ ")+confirmation, m.width, m.height) + "\n"
	}
	width := setupContentWidth(m.width)
	return setupFrame(setupHeader(m.title, m.description, width)+"\n"+m.input.View()+"\n"+
		chatDim.Render(ansi.Wrap("Enter continue · Esc cancel", width, "")), m.width, m.height)
}

type setupProgressMsg string
type setupWorkDoneMsg struct{ err error }

type setupWorkModel struct {
	title, progress string
	spinner         spinner.Model
	cancel          context.CancelFunc
	updates         <-chan string
	finished        <-chan error
	width, height   int
	done, cancelled bool
}

func newSetupWorkModel(title string, cancel context.CancelFunc, progress <-chan string, done <-chan error) *setupWorkModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = chatTitle
	return &setupWorkModel{title: title, cancel: cancel, updates: progress, finished: done, spinner: s, width: 80, height: 24}
}

func (m *setupWorkModel) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case progress := <-m.updates:
			return setupProgressMsg(progress)
		case err := <-m.finished:
			return setupWorkDoneMsg{err: err}
		}
	}
}

func (m *setupWorkModel) Init() tea.Cmd { return tea.Batch(m.spinner.Tick, m.next()) }

func (m *setupWorkModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "esc" || msg.String() == "ctrl+c" {
			m.cancelled = true
			m.cancel()
			return m, nil
		}
	case setupProgressMsg:
		m.progress = chatSanitize(string(msg))
		return m, m.next()
	case setupWorkDoneMsg:
		m.done = true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *setupWorkModel) View() string {
	if m.done {
		return ""
	}
	title, progress := m.title, m.progress
	if m.cancelled {
		title, progress = "Canceling…", ""
	}
	width := setupContentWidth(m.width)
	view := m.spinner.View() + " " + chatTitle.Render(ansi.Wrap(chatSingleLine(title), max(1, width-2), ""))
	if progress != "" {
		view += "\n" + chatDim.Render(ansi.Wrap(progress, width, ""))
	}
	if !m.cancelled {
		view += "\n" + chatDim.Render("Esc cancel")
	}
	return setupFrame(view, m.width, m.height)
}

func setupContentWidth(width int) int { return max(1, min(88, width-2)) }

func setupHeader(title, description string, width int) string {
	header := chatTitle.Render(ansi.Wrap(chatSingleLine(title), width, ""))
	if description != "" {
		header += "\n" + chatDim.Render(ansi.Wrap(chatSanitize(description), width, ""))
	}
	return header
}

func setupFrame(frame string, width, height int) string {
	lines := strings.Split(frame, "\n")
	if len(lines) > max(1, height) {
		lines = lines[:max(1, height)]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, min(88, width)), "")
	}
	return strings.Join(lines, "\n")
}
