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
	Voice         bool
	VoiceFactory  func(context.Context) (VoiceSession, error)
	Input         io.Reader
	Output        io.Writer
}

// UIState keeps the visible transcript in memory when connection setup is
// canceled and the same engine resumes. A new connection uses a fresh state.
type UIState struct {
	transcript []chatEntry
	composer   string
	Voice      bool
}

// ErrReconfigure asks the command to reopen private connection setup.
var ErrReconfigure = errors.New("chat setup requested")

// ErrVoiceSetup asks the command to configure voice credentials privately.
var ErrVoiceSetup = errors.New("voice setup requested")

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
			opts.State.composer = m.composer.Value()
			opts.State.Voice = m.voiceEnabled
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
	if err == nil && m.voiceSetup {
		return ErrVoiceSetup
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

type voiceMessage struct {
	id      uint64
	session VoiceSession
	event   VoiceEvent
}

type voiceDelegation struct {
	id, prompt, display string
	generation          uint64
}

type chatModel struct {
	ctx     context.Context
	opts    UIOptions
	workers sync.WaitGroup

	width, height   int
	compact         bool
	showToolDetails bool
	transcript      []chatEntry
	viewport        viewport.Model
	composer        textarea.Model
	spinner         spinner.Model
	status          string

	turnID      uint64
	events      <-chan turnMessage
	cancelTurn  context.CancelFunc
	active      bool
	canceling   bool
	quitting    bool
	reconfigure bool
	approval    *approvalRequest
	preview     viewport.Model

	voiceEnabled      bool
	voiceStarting     bool
	voiceSetup        bool
	voiceID           uint64
	voiceSession      VoiceSession
	voiceCtx          context.Context
	voiceCancel       context.CancelFunc
	voiceClosed       <-chan struct{}
	voiceEvents       <-chan voiceMessage
	voiceSend         chan<- voiceMessage
	delegation        *voiceDelegation
	pendingDelegation *voiceDelegation
	turnReply         string
	voiceSeen         map[string]bool
	voiceActionTail   <-chan struct{}
	turnSpeechCtx     context.Context
	turnSpeechCancel  context.CancelFunc
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
		m.composer.SetValue(opts.State.composer)
	}
	m.resize(80, 24)
	if m.removeStandaloneCredential(m.opts.InitialPrompt) {
		m.opts.InitialPrompt = ""
	}
	return m
}

func (m *chatModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink}
	if m.opts.Voice {
		cmds = append(cmds, m.startVoice())
	}
	if strings.TrimSpace(m.opts.InitialPrompt) != "" {
		cmds = append(cmds, func() tea.Msg { return initialPromptMessage{} })
	}
	return tea.Batch(cmds...)
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
	case voiceMessage:
		return m, m.handleVoiceMessage(msg)
	case turnMessage:
		if msg.id != m.turnID || !m.active {
			return m, nil
		}
		if msg.done {
			canceled := m.canceling || errors.Is(msg.err, context.Canceled)
			m.active = false
			m.approval = nil
			m.cancelTurn()
			m.cancelTurn = nil
			m.status = "Ready"
			if canceled {
				m.appendEntry("notice", "Canceled", "You can send another message.")
			} else if msg.err != nil {
				m.appendEntry("error", "Agent error", msg.err.Error())
			}
			m.canceling = false
			m.finishVoiceTurn(msg.err, canceled)
			if next := m.pendingDelegation; next != nil {
				m.pendingDelegation = nil
				if m.voiceEnabled && next.generation == m.voiceID {
					cmd := m.startVoiceTurn(next)
					return m, tea.Batch(cmd, m.composer.Focus())
				}
			}
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
				m.voiceActionFor(m.turnSpeechCtx, func(ctx context.Context, session VoiceSession) error {
					return session.Reply(ctx, "", "Please review the tool call in the terminal and press y or n.")
				})
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
		case "ctrl+t":
			m.showToolDetails = !m.showToolDetails
			m.refreshTranscript()
			return m, nil
		case "ctrl+c":
			m.interruptVoice()
			m.pendingDelegation = nil
			if m.active {
				m.cancelActiveTurn()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "esc":
			m.interruptVoice()
			m.pendingDelegation = nil
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
			prompt := m.composer.Value()
			control := strings.TrimSpace(prompt)
			if m.active && !(strings.HasPrefix(control, "/voice") || control == "/setup" || control == "/quit" || control == "/help" || control == "/tools") {
				return m, nil
			}
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
	case "/tools":
		m.showToolDetails = !m.showToolDetails
		m.refreshTranscript()
		return nil
	case "/quit", "/exit":
		m.quitting = true
		return tea.Quit
	case "/clear":
		wasVoice := m.voiceEnabled
		m.stopVoice()
		m.opts.Engine.Reset()
		m.transcript = nil
		text := "Your next message starts a new conversation."
		if wasVoice {
			text += " Voice is off so its previous context is discarded; use /voice to start listening again."
		}
		m.appendEntry("notice", "Conversation cleared", text)
		return nil
	case "/setup":
		if m.active {
			m.cancelActiveTurn()
		}
		m.reconfigure = true
		m.quitting = true
		return tea.Quit
	case "/voice", "/voice on", "/voice off", "/voice setup":
		if prompt == "/voice setup" {
			return m.requestVoiceSetup()
		}
		if prompt == "/voice off" || (prompt == "/voice" && m.voiceEnabled) {
			m.stopVoice()
			m.appendEntry("notice", "Voice off", "The microphone and voice connection are shutting down. Text chat remains available.")
			return nil
		}
		if !m.voiceEnabled {
			return m.startVoice()
		}
		return nil
	case "/help":
		m.appendEntry("notice", "Chat commands", "/help   Show this help\n/tools  Show or hide full tool details (Ctrl+T)\n/setup  Change your AI or enter an API key privately\n/voice  Toggle voice; /voice setup changes voice credentials privately\n/clear  Clear the transcript and model conversation\n/quit   Exit chat\n\nEnter sends a message. Alt+Enter or Ctrl+J adds a new line.\nPgUp/PgDn scroll the transcript; Ctrl+Home/End jump to its start/end.\nEsc or Ctrl+C cancels active work and stops voice playback. Ctrl+C exits when idle.\nFor tool approvals, review the arguments and press y to allow once or n to deny. Spoken approval is never accepted.\nScroll the approval with ↑/↓, PgUp/PgDn, or the mouse wheel.")
		return nil
	}
	if strings.HasPrefix(prompt, "/") && !strings.ContainsAny(prompt, " \n\t") {
		m.appendEntry("notice", "Unknown command", "Use /help to see the available chat commands.")
		return nil
	}
	m.voiceContext("Typed request (handled by the text agent): " + prompt)
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
	return m.startTurnWithDisplay(prompt, prompt)
}

func (m *chatModel) startTurnWithDisplay(prompt, display string) tea.Cmd {
	if m.turnSpeechCancel != nil {
		m.turnSpeechCancel()
	}
	m.turnSpeechCtx, m.turnSpeechCancel = context.WithCancel(m.ctx)
	m.delegation = nil
	m.turnReply = ""
	m.turnID++
	id := m.turnID
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	m.active = true
	m.canceling = false
	m.status = "Thinking"
	m.appendEntry("user", "You", display)
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
	if m.turnSpeechCancel != nil {
		m.turnSpeechCancel()
	}
	m.canceling = true
	m.status = "Canceling…"
	m.approval = nil
	if m.cancelTurn != nil {
		m.cancelTurn()
	}
}

func (m *chatModel) requestVoiceSetup() tea.Cmd {
	if m.active {
		m.cancelActiveTurn()
	}
	m.stopVoice()
	m.voiceSetup = true
	m.quitting = true
	return tea.Quit
}

func (m *chatModel) startVoice() tea.Cmd {
	if m.opts.VoiceFactory == nil {
		return m.requestVoiceSetup()
	}
	m.voiceID++
	id := m.voiceID
	ctx, cancel := context.WithCancel(m.ctx)
	m.voiceCtx, m.voiceCancel = ctx, cancel
	m.voiceEnabled, m.voiceStarting = true, true
	m.voiceSeen = make(map[string]bool)
	m.voiceActionTail = nil
	events := make(chan voiceMessage, 64)
	m.voiceEvents, m.voiceSend = events, events
	previousClosed := m.voiceClosed
	closed := make(chan struct{})
	m.voiceClosed = closed
	factory := m.opts.VoiceFactory
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer close(closed)
		send := func(msg voiceMessage) bool {
			msg.id = id
			select {
			case events <- msg:
				return true
			case <-ctx.Done():
				return false
			}
		}
		// A quick off/on toggle must finish closing the previous microphone
		// before attempting to open it again.
		if previousClosed != nil {
			select {
			case <-previousClosed:
			case <-ctx.Done():
				return
			}
		}
		session, err := factory(ctx)
		if err == nil && session == nil {
			err = errors.New("voice provider returned no session")
		}
		if err != nil {
			send(voiceMessage{event: VoiceEvent{Type: "error", Err: err}})
			return
		}
		defer session.Close()
		if !send(voiceMessage{session: session}) {
			return
		}
		for {
			select {
			case event, ok := <-session.Events():
				if !ok {
					send(voiceMessage{event: VoiceEvent{Type: "done"}})
					return
				}
				if !send(voiceMessage{event: event}) || event.Type == "done" || event.Type == "error" {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return m.waitForVoiceEvent()
}

func (m *chatModel) waitForVoiceEvent() tea.Cmd {
	ctx, events := m.voiceCtx, m.voiceEvents
	return func() tea.Msg {
		select {
		case msg := <-events:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *chatModel) stopVoice() {
	if m.turnSpeechCancel != nil {
		m.turnSpeechCancel()
	}
	if m.voiceCancel != nil {
		m.voiceCancel()
	}
	m.voiceEnabled, m.voiceStarting = false, false
	m.voiceSession = nil
	m.pendingDelegation, m.delegation = nil, nil
}

func (m *chatModel) voiceAction(action func(context.Context, VoiceSession) error) {
	m.voiceActionFor(m.voiceCtx, action)
}

func (m *chatModel) voiceActionFor(actionCtx context.Context, action func(context.Context, VoiceSession) error) {
	if !m.voiceEnabled || m.voiceSession == nil {
		return
	}
	ctx, session, id, events := m.voiceCtx, m.voiceSession, m.voiceID, m.voiceSend
	if actionCtx == nil {
		actionCtx = ctx
	}
	previous := m.voiceActionTail
	finished := make(chan struct{})
	m.voiceActionTail = finished
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer close(finished)
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil || actionCtx.Err() != nil {
			return
		}
		// Task speech has its own lifetime: correction/escape invalidates a
		// queued result. Once a write begins it uses the session lifetime, so
		// canceling one task cannot tear down the live connection mid-write.
		if err := action(ctx, session); err != nil && ctx.Err() == nil {
			select {
			case events <- voiceMessage{id: id, event: VoiceEvent{Type: "error", Err: err}}:
			case <-ctx.Done():
			}
		}
	}()
}

func (m *chatModel) interruptVoice() {
	// Playback interruption must not wait behind queued context or speech.
	m.voiceActionTail = nil
	m.voiceAction(func(ctx context.Context, session VoiceSession) error { return session.Interrupt(ctx) })
}

func (m *chatModel) voiceContext(text string) {
	m.voiceAction(func(ctx context.Context, session VoiceSession) error { return session.SendContext(ctx, text) })
}

func (m *chatModel) handleVoiceMessage(msg voiceMessage) tea.Cmd {
	if msg.id != m.voiceID || !m.voiceEnabled {
		return nil
	}
	if msg.session != nil {
		m.voiceSession = msg.session
		contextText := "The current workspace is " + m.opts.Workspace + ". Device preference: " + m.opts.Device + ". The terminal handles all action approvals with keyboard y/n; spoken approval is never sufficient."
		// Voice can be enabled in the middle of a text conversation. Seed recent
		// visible exchanges without waiting on an engine that may be working.
		for _, entry := range m.transcript[max(0, len(m.transcript)-8):] {
			if entry.kind == "user" || entry.kind == "assistant" {
				text := []rune(entry.text)
				if len(text) > 2000 {
					text = text[:2000]
				}
				contextText += "\n" + entry.title + ": " + string(text)
			}
		}
		m.voiceContext(contextText)
		return m.waitForVoiceEvent()
	}
	event := msg.event
	switch event.Type {
	case "ready":
		m.voiceStarting = false
		m.appendEntry("notice", "Voice on · microphone sent to OpenAI · /voice to stop", "Speak naturally. Approvals stay in the terminal.")
	case "input", "output":
		if event.Text != "" {
			title := "You · voice"
			if event.Type == "output" {
				title = "Voice"
			}
			kind := "voice_" + event.Type
			last := len(m.transcript) - 1
			if last >= 0 && m.transcript[last].kind == kind {
				m.transcript[last].text += event.Text
				m.refreshTranscript()
			} else {
				m.appendEntry(kind, title, event.Text)
			}
		}
	case "delegation":
		if event.DelegationID == "" || strings.TrimSpace(event.Text) == "" || m.voiceSeen[event.DelegationID] {
			break
		}
		m.voiceSeen[event.DelegationID] = true
		prompt := event.Prompt
		if prompt == "" {
			prompt = event.Text
		}
		next := &voiceDelegation{id: event.DelegationID, prompt: prompt, display: event.Text, generation: m.voiceID}
		if m.active {
			m.pendingDelegation = next
			m.cancelActiveTurn()
		} else {
			cmd := m.startVoiceTurn(next)
			return tea.Batch(cmd, m.waitForVoiceEvent())
		}
	case "error", "done":
		m.stopVoice()
		if event.Type == "error" {
			detail := "Voice connection failed."
			if event.Err != nil {
				detail = event.Err.Error()
			}
			m.appendEntry("error", "Voice unavailable", detail+"\nText chat remains available. Use /voice to retry or /voice setup to change credentials privately.")
		} else {
			m.appendEntry("notice", "Voice disconnected", "Text chat remains available. Use /voice to reconnect.")
		}
		return nil
	}
	return m.waitForVoiceEvent()
}

func (m *chatModel) finishVoiceTurn(err error, canceled bool) {
	delegation := m.delegation
	m.delegation = nil
	if canceled || m.pendingDelegation != nil {
		return
	}
	text := strings.TrimSpace(m.turnReply)
	if err != nil {
		text = "I couldn't complete the task: " + err.Error()
	}
	if text == "" {
		return
	}
	if delegation != nil {
		if delegation.generation == m.voiceID {
			m.voiceActionFor(m.turnSpeechCtx, func(ctx context.Context, session VoiceSession) error {
				return session.Reply(ctx, delegation.id, text)
			})
		}
	} else {
		m.voiceContext("Text agent result (context only): " + text)
	}
}

func (m *chatModel) startVoiceTurn(delegation *voiceDelegation) tea.Cmd {
	prompt := delegation.prompt + "\n\nThis request came from voice. Perform the requested work, then give a concise final result suitable for speaking. Do not narrate tool progress aloud; keep required action approvals in the terminal."
	cmd := m.startTurnWithDisplay(prompt, delegation.display)
	m.delegation = delegation
	return cmd
}

func (m *chatModel) handleEvent(event Event) {
	switch event.Type {
	case "text":
		if event.Text == "" {
			return
		}
		m.turnReply += event.Text
		last := len(m.transcript) - 1
		if last >= 0 && m.transcript[last].kind == "assistant" {
			m.transcript[last].text += event.Text
		} else {
			m.transcript = append(m.transcript, chatEntry{kind: "assistant", title: "Wendy", text: event.Text})
		}
		m.status = "Responding"
		m.refreshTranscript()
	case "tool_start":
		m.turnReply = ""
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
	follow, offset := m.viewport.AtBottom(), m.viewport.YOffset
	content, _ := m.transcriptContent(m.viewport.Width)
	m.viewport.SetContent(content)
	if follow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(offset)
	}
}

// transcriptContent keeps a row count for each logical line, so a terminal
// resize can preserve the passage being read when those lines wrap differently.
func (m *chatModel) transcriptContent(width int) (string, []int) {
	var rows []string
	var counts []int
	appendBlock := func(text string, style lipgloss.Style, truncate bool) {
		for _, line := range strings.Split(text, "\n") {
			wrapped := ansi.Wrap(line, max(1, width), "")
			if truncate {
				wrapped = ansi.Truncate(line, max(1, width), "…")
			}
			rows = append(rows, style.Render(wrapped))
			counts = append(counts, strings.Count(wrapped, "\n")+1)
		}
	}
	plain := lipgloss.NewStyle()
	for i, entry := range m.transcript {
		compactTool := !m.showToolDetails && (entry.kind == "tool" || entry.kind == "result")
		if i > 0 {
			previous := m.transcript[i-1].kind
			if !compactTool || (previous != "tool" && previous != "result") {
				appendBlock("", plain, false)
			}
		}
		if compactTool {
			appendBlock(chatSingleLine(compactToolEntry(entry)), chatDim, true)
			continue
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
		appendBlock(chatSanitize(entry.title), style, false)
		// Sanitize accumulated text so escapes split over streaming chunks
		// cannot escape into the terminal.
		appendBlock(chatSanitize(entry.text), plain, false)
	}
	return strings.Join(rows, "\n"), counts
}

type chatViewportAnchor struct {
	line, row, height int
}

func captureChatViewportAnchor(counts []int, offset int) chatViewportAnchor {
	for line, height := range counts {
		if offset < height {
			return chatViewportAnchor{line: line, row: max(0, offset), height: height}
		}
		offset -= height
	}
	return chatViewportAnchor{line: len(counts)}
}

func (a chatViewportAnchor) offset(counts []int) int {
	offset := 0
	for line, height := range counts {
		if line == a.line {
			return offset + min(height-1, a.row*height/max(1, a.height))
		}
		offset += height
	}
	return offset
}

func chatWrappedLineCounts(content string, width int) []int {
	lines := strings.Split(content, "\n")
	counts := make([]int, len(lines))
	for i, line := range lines {
		counts[i] = strings.Count(ansi.Wrap(line, max(1, width), ""), "\n") + 1
	}
	return counts
}

func (m *chatModel) resize(width, height int) {
	follow := m.viewport.AtBottom()
	_, oldCounts := m.transcriptContent(m.viewport.Width)
	anchor := captureChatViewportAnchor(oldCounts, m.viewport.YOffset)
	approvalBody := m.approvalContent()
	approvalFollow := m.preview.AtBottom()
	approvalAnchor := captureChatViewportAnchor(chatWrappedLineCounts(approvalBody, m.preview.Width), m.preview.YOffset)
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
	if m.compact {
		m.preview.Height = max(1, m.height-3)
	}
	// The textarea setters only change geometry. Rendering refreshes its
	// private viewport's wrapped content before Update repositions the caret.
	_ = m.composer.View()
	m.composer, _ = m.composer.Update(nil)
	content, counts := m.transcriptContent(contentWidth)
	m.viewport.SetContent(content)
	if follow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(anchor.offset(counts))
	}
	if m.approval != nil {
		m.preview.SetContent(ansi.Wrap(approvalBody, contentWidth, ""))
		if approvalFollow {
			m.preview.GotoBottom()
		} else {
			m.preview.SetYOffset(approvalAnchor.offset(chatWrappedLineCounts(approvalBody, contentWidth)))
		}
	}
}

func (m *chatModel) refreshApproval() {
	if m.approval == nil {
		return
	}
	m.preview.SetContent(ansi.Wrap(m.approvalContent(), m.preview.Width, ""))
	m.preview.SetYOffset(m.preview.YOffset)
}

func (m *chatModel) approvalContent() string {
	if m.approval == nil {
		return ""
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
	return chatSanitize(body)
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
			line(chatDim.Render(fmt.Sprintf("Arguments · %.0f%% · ↑/↓ PgUp/PgDn scroll · %s", m.preview.ScrollPercent()*100, m.voiceStatus()))),
			line(chatWarn.Render("[y] Allow once   [n] Deny   [Esc] Cancel turn")),
		}, "\n")
		if m.compact {
			question := line(chatWarn.Render("Allow " + chatSingleLine(m.approval.call.Name) + "?"))
			controls := line(chatWarn.Render("[y] Allow [n] Deny"))
			mic := line(chatDim.Render(m.voiceStatus()))
			parts := []string{controls}
			if m.height >= 4 {
				parts = []string{question, m.preview.View(), mic, controls}
			} else if m.height == 3 {
				parts = []string{question, mic, controls}
			} else if m.height == 2 {
				parts = []string{mic, controls}
			}
			frame = strings.Join(parts, "\n")
		}
	} else {
		status := chatSingleLine(m.status)
		status += " · " + m.voiceStatus()
		if m.active {
			status = m.spinner.View() + " " + status
		}
		if !m.viewport.AtBottom() {
			status += fmt.Sprintf(" · %.0f%% · Ctrl+End to follow", m.viewport.ScrollPercent()*100)
		}
		if m.opts.AutoApprove {
			status += " · auto-approve"
		}
		hints := "Enter send · Alt+Enter newline · Ctrl+T tools · /help · Ctrl+C exit"
		if m.active {
			hints = "Esc/Ctrl+C cancel · Ctrl+T tools · PgUp/PgDn scroll"
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

func (m *chatModel) voiceStatus() string {
	if !m.voiceEnabled {
		if m.voiceClosed != nil {
			select {
			case <-m.voiceClosed:
			default:
				return "mic stopping"
			}
		}
		return "mic muted"
	}
	if m.voiceStarting {
		return "voice connecting"
	}
	if m.active {
		return "voice on · working"
	}
	return "voice on · listening"
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
