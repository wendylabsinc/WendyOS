package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// UIOptions supplies the agent and the session information displayed by Run.
type UIOptions struct {
	Engine        *Engine
	State         *UIState
	Model         string
	Provider      string
	Workspace     string
	Device        string
	InitialPrompt string
	AutoApprove   bool
	Input         io.Reader
	Output        io.Writer
}

// UIState keeps the visible transcript in memory when connection setup is
// canceled and the same engine resumes. A new connection uses a fresh state.
type UIState struct {
	transcript []chatEntry
}

// ErrReconfigure asks the command to reopen private connection setup.
var ErrReconfigure = errors.New("chat setup requested")

// Run opens a full-screen chat session, restoring the original terminal on exit.
// All agent work and pending approvals are canceled before Run returns.
func Run(ctx context.Context, opts UIOptions) error {
	if opts.Engine == nil {
		return errors.New("chat requires an engine")
	}
	ctx, cancel := context.WithCancel(ctx)
	m := newChatModel(ctx, opts)
	defer func() {
		cancel()
		m.workers.Wait()
		if opts.State != nil {
			opts.State.transcript = append([]chatEntry(nil), m.transcript...)
		}
	}()
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion()}
	if opts.Input != nil {
		options = append(options, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		options = append(options, tea.WithOutput(opts.Output))
	}
	_, err := tea.NewProgram(m, options...).Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && m.reconfigure {
		return ErrReconfigure
	}
	return err
}

type chatEntry struct {
	kind  string
	title string
	text  string
}

type approvalRequest struct {
	call  ToolCall
	reply chan bool // Buffered so a decision never blocks the UI after cancellation.
}

type turnMessage struct {
	id       uint64
	event    *Event
	approval *approvalRequest
	done     bool
	err      error
}

type initialPromptMessage struct{}

type chatModel struct {
	ctx     context.Context
	opts    UIOptions
	workers sync.WaitGroup

	width, height int
	compact       bool
	transcript    []chatEntry
	viewport      viewport.Model
	composer      textarea.Model
	spinner       spinner.Model
	status        string

	turnID      uint64
	events      <-chan turnMessage
	cancelTurn  context.CancelFunc
	active      bool
	canceling   bool
	quitting    bool
	reconfigure bool
	approval    *approvalRequest
	preview     viewport.Model
}

var (
	chatTitle = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	chatDim   = lipgloss.NewStyle().Foreground(tui.ColorDim)
	chatUser  = lipgloss.NewStyle().Bold(true).Foreground(tui.Emerald200)
	chatTool  = lipgloss.NewStyle().Foreground(tui.ColorInfo).Bold(true)
	chatWarn  = lipgloss.NewStyle().Foreground(tui.ColorNotice).Bold(true)
	chatError = lipgloss.NewStyle().Foreground(tui.ColorError)
	chatBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(tui.ColorBorder).Padding(0, 1)
)

func newChatModel(ctx context.Context, opts UIOptions) *chatModel {
	input := textarea.New()
	input.Placeholder = "Ask Wendy to build, deploy, or inspect your hardware…"
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.MaxHeight = 0
	input.MaxWidth = 0
	input.EndOfBufferCharacter = ' '
	input.FocusedStyle.Prompt = chatTitle
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.BlurredStyle.Prompt = chatDim
	input.Cursor.Style = chatTitle
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	input.Focus()
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = chatTitle
	m := &chatModel{
		ctx: ctx, opts: opts, composer: input, spinner: s,
		viewport: viewport.New(0, 0), preview: viewport.New(0, 0),
		status: "Ready",
	}
	m.viewport.KeyMap = viewport.KeyMap{} // Composer owns ordinary cursor keys.
	m.transcript = []chatEntry{{kind: "welcome", title: "Build something with Wendy", text: "Develop in your workspace and work with Wendy devices using natural language.\nTry: inspect my device, explain this project, or build and deploy an app.\n\nType /help for commands. Tool actions that change files or devices ask for approval."}}
	if opts.AutoApprove {
		m.transcript[0].text += "\nAutomatic tool approval is enabled for this session."
	}
	if opts.State != nil && len(opts.State.transcript) > 0 {
		m.transcript = append([]chatEntry(nil), opts.State.transcript...)
	}
	m.resize(80, 24)
	if m.removeStandaloneCredential(m.opts.InitialPrompt) {
		m.opts.InitialPrompt = ""
	}
	return m
}

func (m *chatModel) Init() tea.Cmd {
	if strings.TrimSpace(m.opts.InitialPrompt) != "" {
		return tea.Batch(textarea.Blink, func() tea.Msg { return initialPromptMessage{} })
	}
	return textarea.Blink
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case initialPromptMessage:
		if !m.active && !m.quitting {
			prompt := m.opts.InitialPrompt
			m.opts.InitialPrompt = ""
			return m, m.submit(prompt)
		}
		return m, nil
	case turnMessage:
		if msg.id != m.turnID || !m.active {
			return m, nil
		}
		if msg.done {
			m.active = false
			m.approval = nil
			m.cancelTurn()
			m.cancelTurn = nil
			m.status = "Ready"
			if m.canceling || errors.Is(msg.err, context.Canceled) {
				m.appendEntry("notice", "Canceled", "You can send another message.")
			} else if msg.err != nil {
				m.appendEntry("error", "Agent error", msg.err.Error())
			}
			m.canceling = false
			return m, m.composer.Focus()
		}
		if !m.canceling {
			if msg.event != nil {
				m.handleEvent(*msg.event)
			}
			if msg.approval != nil {
				m.approval = msg.approval
				m.status = "Approval required"
				m.refreshApproval()
				m.preview.GotoTop()
			}
		}
		return m, m.waitForEvent()
	case spinner.TickMsg:
		if m.active {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes {
			msg.Runes = []rune(chatSanitize(string(msg.Runes)))
		}
		switch msg.String() {
		case "ctrl+c":
			if m.active {
				m.cancelActiveTurn()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "esc":
			if m.active {
				m.cancelActiveTurn()
			}
			return m, nil
		}
		if m.approval != nil {
			switch strings.ToLower(msg.String()) {
			case "y", "n":
				allowed := strings.ToLower(msg.String()) == "y"
				m.approval.reply <- allowed
				label := "Denied"
				if allowed {
					label = "Allowed once"
				}
				m.appendEntry("notice", label, m.approval.call.Name)
				m.approval = nil
				m.status = "Working"
				return m, nil
			}
			m.scroll(&m.preview, msg.String(), true)
			return m, nil
		}
		if m.scroll(&m.viewport, msg.String(), false) {
			return m, nil
		}
		if msg.String() == "enter" {
			if m.active {
				return m, nil
			}
			prompt := m.composer.Value()
			m.composer.Reset()
			return m, m.submit(prompt)
		}
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		m.removeStandaloneCredential(m.composer.Value())
		return m, cmd
	case tea.MouseMsg:
		var cmd tea.Cmd
		if m.approval != nil {
			m.preview, cmd = m.preview.Update(msg)
		} else {
			m.viewport, cmd = m.viewport.Update(msg)
		}
		return m, cmd
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.removeStandaloneCredential(m.composer.Value())
	return m, cmd
}

func (m *chatModel) submit(prompt string) tea.Cmd {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	if m.removeStandaloneCredential(prompt) {
		return nil
	}
	switch prompt {
	case "/quit", "/exit":
		m.quitting = true
		return tea.Quit
	case "/clear":
		m.opts.Engine.Reset()
		m.transcript = nil
		m.appendEntry("notice", "Conversation cleared", "Your next message starts a new conversation.")
		return nil
	case "/setup":
		if m.active {
			m.appendEntry("notice", "Setup", "Press Esc to cancel the current response before changing your AI setup.")
			return nil
		}
		m.reconfigure = true
		m.quitting = true
		return tea.Quit
	case "/help":
		m.appendEntry("notice", "Chat commands", "/help   Show this help\n/setup  Change your AI or enter an API key privately\n/clear  Clear the transcript and model conversation\n/quit   Exit chat\n\nEnter sends a message. Alt+Enter or Ctrl+J adds a new line.\nPgUp/PgDn scroll the transcript; Ctrl+Home/End jump to its start/end.\nEsc or Ctrl+C cancels active work. Ctrl+C exits when idle.\nFor tool approvals, review the arguments and press y to allow once or n to deny.\nScroll the approval with ↑/↓, PgUp/PgDn, or the mouse wheel.")
		return nil
	}
	if strings.HasPrefix(prompt, "/") && !strings.ContainsAny(prompt, " \n\t") {
		m.appendEntry("notice", "Unknown command", "Use /help to see the available chat commands.")
		return nil
	}
	return m.startTurn(prompt)
}

// Catch credentials accidentally pasted into the ordinary message box before
// they reach either the terminal renderer or the conversation. Restrict this
// check to an entire key-shaped token so prose and code remain editable.
func (m *chatModel) removeStandaloneCredential(value string) bool {
	if !looksLikeStandaloneCredential(value) {
		return false
	}
	m.composer.Reset()
	m.appendEntry("notice", "Enter API keys privately", "That looks like an API key. I removed it from the message box. Use /setup to enter it privately.")
	return true
}

func looksLikeStandaloneCredential(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) <= 20 || !strings.HasPrefix(value, "sk-") {
		return false
	}
	for _, r := range value[3:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (m *chatModel) startTurn(prompt string) tea.Cmd {
	m.turnID++
	id := m.turnID
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	m.active = true
	m.canceling = false
	m.status = "Thinking"
	m.appendEntry("user", "You", prompt)
	m.viewport.GotoBottom()
	events := make(chan turnMessage, 64)
	m.events = events
	engine, autoApprove, sessionCtx := m.opts.Engine, m.opts.AutoApprove, m.ctx

	// A single producer preserves ordering between text, tool activity, approval
	// requests, and completion. Backpressure never drops streaming tokens. Both
	// sends and approval waits release on cancellation, including program exit.
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer close(events)
		emit := func(event Event) {
			select {
			case events <- turnMessage{id: id, event: &event}:
			case <-ctx.Done():
			}
		}
		approve := func(ctx context.Context, call ToolCall) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if autoApprove {
				return true, nil
			}
			req := &approvalRequest{call: call, reply: make(chan bool, 1)}
			select {
			case events <- turnMessage{id: id, approval: req}:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			select {
			case answer := <-req.reply:
				return answer, ctx.Err()
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		err := engine.Turn(ctx, prompt, emit, approve)
		// Completion uses the session context: canceling a turn must still notify
		// the UI that it can accept another prompt after the worker has stopped.
		select {
		case events <- turnMessage{id: id, done: true, err: err}:
		case <-sessionCtx.Done():
		}
	}()
	return tea.Batch(m.waitForEvent(), m.spinner.Tick)
}

func (m *chatModel) waitForEvent() tea.Cmd {
	events, ctx := m.events, m.ctx
	return func() tea.Msg {
		select {
		case msg, ok := <-events:
			if ok {
				return msg
			}
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *chatModel) cancelActiveTurn() {
	m.canceling = true
	m.status = "Canceling…"
	m.approval = nil
	if m.cancelTurn != nil {
		m.cancelTurn()
	}
}

func (m *chatModel) handleEvent(event Event) {
	switch event.Type {
	case "text":
		if event.Text == "" {
			return
		}
		last := len(m.transcript) - 1
		if last >= 0 && m.transcript[last].kind == "assistant" {
			m.transcript[last].text += event.Text
		} else {
			m.transcript = append(m.transcript, chatEntry{kind: "assistant", title: "Wendy", text: event.Text})
		}
		m.status = "Responding"
		m.refreshTranscript()
	case "tool_start":
		name, arguments := "tool", ""
		if event.Call != nil {
			name, arguments = event.Call.Name, prettyArguments(event.Call.Arguments)
		}
		m.status = "Running " + name
		m.appendEntry("tool", "Tool · "+name, arguments)
	case "tool_result":
		name := "tool"
		if event.Call != nil {
			name = event.Call.Name
		}
		m.appendEntry("result", "Result · "+name, event.Text)
		m.status = "Thinking"
	case "status":
		m.status = event.Text
	}
}

func (m *chatModel) appendEntry(kind, title, text string) {
	m.transcript = append(m.transcript, chatEntry{kind: kind, title: title, text: text})
	m.refreshTranscript()
}

func (m *chatModel) refreshTranscript() {
	follow := m.viewport.AtBottom()
	var out strings.Builder
	for i, entry := range m.transcript {
		if i > 0 {
			out.WriteString("\n\n")
		}
		style := chatTitle
		switch entry.kind {
		case "user":
			style = chatUser
		case "tool", "result":
			style = chatTool
		case "notice":
			style = chatWarn
		case "error":
			style = chatError
		}
		out.WriteString(style.Render(ansi.Wrap(chatSanitize(entry.title), m.viewport.Width, "")))
		out.WriteByte('\n')
		// Sanitize the accumulated text, rather than each token, so escape
		// sequences split over separate streaming chunks are also removed.
		out.WriteString(ansi.Wrap(chatSanitize(entry.text), m.viewport.Width, ""))
	}
	m.viewport.SetContent(out.String())
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *chatModel) resize(width, height int) {
	follow := m.viewport.AtBottom()
	m.width, m.height = max(1, width), max(1, height)
	m.compact = m.width < 40 || m.height < 14
	contentWidth := max(1, m.width-2)
	m.viewport.Width = contentWidth
	if m.compact {
		m.composer.SetWidth(contentWidth)
		m.composer.SetHeight(1)
		m.viewport.Height = max(1, m.height-5)
	} else {
		m.composer.SetWidth(max(1, contentWidth-4))
		m.composer.SetHeight(3)
		m.viewport.Height = max(1, m.height-11)
	}
	m.preview.Width = contentWidth
	m.preview.Height = max(1, m.height-8)
	m.refreshTranscript()
	if follow {
		m.viewport.GotoBottom()
	}
	m.refreshApproval()
}

func (m *chatModel) refreshApproval() {
	if m.approval == nil {
		return
	}
	call := m.approval.call
	body := prettyArguments(call.Arguments)
	// Also show multiline string arguments as text, so scripts and full file
	// contents can be reviewed line by line instead of as JSON-escaped strings.
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) == nil {
		for _, name := range []string{"command", "content", "patch", "script"} {
			var value string
			if json.Unmarshal(args[name], &value) == nil && strings.Contains(value, "\n") {
				body += "\n\n" + name + " (expanded):\n" + value
			}
		}
	}
	m.preview.SetContent(ansi.Wrap(chatSanitize(body), m.preview.Width, ""))
}

func (m *chatModel) scroll(v *viewport.Model, k string, approval bool) bool {
	switch k {
	case "pgup", "ctrl+u":
		v.HalfViewUp()
	case "pgdown", "ctrl+d":
		v.HalfViewDown()
	case "ctrl+home":
		v.GotoTop()
	case "ctrl+end":
		v.GotoBottom()
	case "up", "k":
		if !approval {
			return false
		}
		v.ScrollUp(1)
	case "down", "j":
		if !approval {
			return false
		}
		v.ScrollDown(1)
	case "home", "g":
		if !approval {
			return false
		}
		v.GotoTop()
	case "end", "G":
		if !approval {
			return false
		}
		v.GotoBottom()
	default:
		return false
	}
	return true
}

func (m *chatModel) View() string {
	if m.quitting {
		return ""
	}
	width := max(1, m.width-2)
	line := func(s string) string { return ansi.Truncate(s, width, "…") }
	title := chatTitle.Render("◆ WENDY CHAT")
	identity := chatSingleLine(m.opts.Provider + " · " + m.opts.Model)
	workspace, device := chatSingleLine(m.opts.Workspace), chatSingleLine(m.opts.Device)
	if device == "" {
		device = "configured default or discovery"
	}
	header := line(title+"  "+chatDim.Render(identity)) + "\n" + line(chatDim.Render("Workspace "+workspace)) + "\n" + line(chatDim.Render("Device preference "+device))
	var frame string
	if m.approval != nil {
		frame = strings.Join([]string{
			header, "", line(chatWarn.Render("Allow tool: " + chatSingleLine(m.approval.call.Name) + "?")),
			m.preview.View(),
			line(chatDim.Render(fmt.Sprintf("Arguments · %.0f%% · ↑/↓ PgUp/PgDn scroll", m.preview.ScrollPercent()*100))),
			line(chatWarn.Render("[y] Allow once   [n] Deny   [Esc] Cancel turn")),
		}, "\n")
	} else {
		status := chatSingleLine(m.status)
		if m.active {
			status = m.spinner.View() + " " + status
		}
		if !m.viewport.AtBottom() {
			status += fmt.Sprintf(" · %.0f%% · Ctrl+End to follow", m.viewport.ScrollPercent()*100)
		}
		if m.opts.AutoApprove {
			status += " · auto-approve"
		}
		hints := "Enter send · Alt+Enter newline · PgUp/PgDn scroll · /help · Ctrl+C exit"
		if m.active {
			hints = "Esc/Ctrl+C cancel · PgUp/PgDn scroll · draft your next message below"
		}
		if m.compact {
			frame = strings.Join([]string{line(title), m.viewport.View(), line(chatDim.Render(status)), m.composer.View(), line(chatDim.Render(hints))}, "\n")
		} else {
			frame = strings.Join([]string{header, "", m.viewport.View(), line(chatDim.Render(status)), chatBox.Render(m.composer.View()), line(chatDim.Render(hints))}, "\n")
		}
	}
	// Small terminal sizes must never cause wrapping or emit more rows than the
	// available screen. This also bounds long provider/device/workspace labels.
	lines := strings.Split(frame, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = " " + ansi.Truncate(lines[i], max(0, m.width-1), "")
	}
	return strings.Join(lines, "\n")
}

func prettyArguments(raw json.RawMessage) string {
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") == nil {
		return out.String()
	}
	return string(raw)
}

// chatSanitize strips terminal escapes and formatting controls while retaining
// newlines and readable tab indentation in model and tool output.
func chatSanitize(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

func chatSingleLine(s string) string {
	return strings.Join(strings.Fields(chatSanitize(s)), " ")
}
