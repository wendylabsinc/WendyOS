package bttable

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Action is the intent the user picked. When a Handler is attached the model
// dispatches Actions as async tea.Cmds and stays open; with no Handler it falls
// back to recording the Action on Result and quitting (used by tests).
type Action int

const (
	ActionNone Action = iota
	ActionQuit
	ActionConnect
	ActionDisconnect
	ActionForget
)

// Result is what the caller reads after the TUI exits. Only populated for the
// no-Handler code path.
type Result struct {
	Action  Action
	Address string
	Name    string
}

// Handler performs Bluetooth operations on behalf of the Model so the TUI can
// stay open between actions. Scan events arrive as ScanResultMsg / ScanDoneMsg;
// each op command must eventually emit an OpResultMsg.
type Handler interface {
	// StartScan (re)opens a scan stream and returns a command that reads the
	// first event. Used by the `r` rescan key.
	StartScan() tea.Cmd
	// NextScanEvent reads the next event from the currently-open scan stream.
	NextScanEvent() tea.Cmd
	Connect(address string) tea.Cmd
	Disconnect(address string) tea.Cmd
	Forget(address string) tea.Cmd
}

// ScanResultMsg carries a batch of discovered peripherals. The agent currently
// sends the whole list in a single message, but the model upserts by address so
// an incremental stream would work without changes.
type ScanResultMsg struct {
	Peripherals []Peripheral
}

// ScanDoneMsg signals the scan stream ended (Err is nil on a clean EOF).
type ScanDoneMsg struct {
	Err error
}

// OpResultMsg reports the outcome of an async connect/disconnect/forget.
type OpResultMsg struct {
	Action  Action
	Name    string
	Address string
	Err     error
	// PairedKnown reports whether Paired carries the agent's actual
	// post-connect pairing state. Older agents do not report it, in which
	// case the model falls back to assuming the requested pairing succeeded.
	PairedKnown bool
	Paired      bool
}

// flashClearMsg clears the current flash message after a delay. The token
// identifies which flash scheduled the clear so a stale timer cannot wipe a
// newer message.
type flashClearMsg struct{ token int }

const flashDuration = 4 * time.Second

// Model is the Bubble Tea model for the interactive Bluetooth table.
type Model struct {
	peripherals []Peripheral
	visible     []Peripheral
	showUnnamed bool
	table       tui.BubbleTable
	spinner     spinner.Model

	handler Handler

	scanning     bool
	busy         bool
	flashMessage string
	flashIsError bool
	flashToken   int

	// scanSeen records the addresses observed during the in-flight scan so a
	// successful ScanDoneMsg can prune devices that have disappeared.
	scanSeen map[string]bool

	result Result
	done   bool
	width  int
	height int
}

// NewModel constructs the model seeded with any peripherals already received and
// starts in the scanning state (the caller continues reading the scan stream).
func NewModel(peripherals []Peripheral) Model {
	Sort(peripherals)

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(tui.ColorPrimary)

	m := Model{
		peripherals: peripherals,
		table:       tui.NewBubbleTable(true, btColumns()),
		spinner:     s,
		scanning:    true,
		scanSeen:    map[string]bool{},
	}
	m.refreshRows()
	return m
}

// WithHandler attaches a Handler, enabling inline async execution of actions so
// the TUI stays open between edits.
func (m Model) WithHandler(h Handler) Model {
	m.handler = h
	return m
}

func (m Model) Init() tea.Cmd {
	// Run the scan inside the TUI so the spinner is visible during the agent's
	// ~8s scan window. StartScan opens the stream and reads the first event;
	// subsequent events are chained from the ScanResultMsg handler.
	if m.handler != nil && m.scanning {
		return tea.Batch(m.handler.StartScan(), m.spinner.Tick)
	}
	if m.scanning {
		return m.spinner.Tick
	}
	return nil
}

func btColumns() []bubbleTable.Column {
	return []bubbleTable.Column{
		{Title: "Name", Width: 28},
		{Title: "Address", Width: 20},
		{Title: "Type", Width: 10},
		{Title: "Paired", Width: 8},
		{Title: "Connected", Width: 10},
	}
}

func (m *Model) refreshRows() {
	selectedAddress := ""
	if p, ok := m.selected(); ok {
		selectedAddress = p.Address
	}
	m.visible = nil
	rows := make([]bubbleTable.Row, 0, len(m.peripherals))
	cursor := 0
	for _, p := range m.peripherals {
		name := tui.StripControl(p.Name)
		if !m.showUnnamed && strings.TrimSpace(name) == "" {
			continue
		}
		if p.Address == selectedAddress {
			cursor = len(rows)
		}
		m.visible = append(m.visible, p)
		rows = append(rows, bubbleTable.Row{
			name,
			tui.StripControl(p.Address),
			DeviceTypeLabel(p.DeviceType),
			yesNo(p.Paired),
			yesNo(p.Connected),
		})
	}
	m.table.SetRows(rows)
	m.table.SetCursor(cursor)
	h := len(rows) + 2
	if h < 6 {
		h = 6
	}
	if m.height > 0 && h > m.height-7 {
		h = max(1, m.height-7)
	}
	m.table.SetHeight(h)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.refreshRows()
		return m, cmd

	case spinner.TickMsg:
		if !m.scanning {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case ScanResultMsg:
		if m.scanSeen == nil {
			m.scanSeen = map[string]bool{}
		}
		for _, p := range msg.Peripherals {
			m.peripherals = Upsert(m.peripherals, p)
			m.scanSeen[p.Address] = true
		}
		Sort(m.peripherals)
		m.refreshRows()
		if m.scanning && m.handler != nil {
			return m, m.handler.NextScanEvent()
		}
		return m, nil

	case ScanDoneMsg:
		m.scanning = false
		if msg.Err != nil {
			// Keep the previously-known peripherals on a failed scan.
			m.scanSeen = map[string]bool{}
			return m, m.setFlash(fmt.Sprintf("Scan failed: %s", userFacingError(msg.Err)), true)
		}
		// Reconcile: drop devices that this completed scan did not report.
		m.peripherals = pruneUnseen(m.peripherals, m.scanSeen)
		m.scanSeen = map[string]bool{}
		Sort(m.peripherals)
		m.refreshRows()
		return m, nil

	case OpResultMsg:
		m.busy = false
		text, isErr := flashFor(msg)
		if msg.Err == nil {
			m.applyOptimisticUpdate(msg)
			m.refreshRows()
		}
		// Deliberately no auto-rescan: a Bluetooth rescan is an ~8s window, so we
		// rely on the optimistic update and let the user press `r` to reconcile.
		return m, m.setFlash(text, isErr)

	case flashClearMsg:
		if msg.token == m.flashToken {
			m.flashMessage = ""
			m.flashIsError = false
		}
		return m, nil
	}

	return m.updateBrowsing(msg)
}

func (m Model) updateBrowsing(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	switch km.String() {
	case "h":
		m.showUnnamed = !m.showUnnamed
		m.refreshRows()
		return m, nil

	case "q", "ctrl+c", "esc":
		m.result.Action = ActionQuit
		m.done = true
		return m, tea.Quit

	case "enter":
		if m.busy {
			return m, nil
		}
		if m.scanning && m.handler != nil {
			// Connecting while discovery is running can fail at the HCI level
			// (paging during an active inquiry), so hold connects until the
			// ~8s scan window closes. Handler-less picker mode is exempt: it
			// never receives a ScanDoneMsg, and its connect happens after the
			// TUI exits anyway.
			return m, m.setFlash("Scan in progress — wait for it to finish, then connect.", true)
		}
		p, ok := m.selected()
		if !ok {
			return m, nil
		}
		if p.Connected {
			return m, m.setFlash(fmt.Sprintf("Already connected to %s.", displayName(p)), false)
		}
		return m.dispatchConnect(p)

	case "d":
		if m.busy {
			return m, nil
		}
		p, ok := m.selected()
		if !ok {
			return m, nil
		}
		if !p.Connected {
			return m, m.setFlash("Only connected devices can be disconnected.", true)
		}
		return m.dispatchDisconnect(p)

	case "f":
		if m.busy {
			return m, nil
		}
		p, ok := m.selected()
		if !ok {
			return m, nil
		}
		if !p.Paired {
			return m, m.setFlash("Only paired devices can be forgotten.", true)
		}
		return m.dispatchForget(p)

	case "r":
		if m.busy || m.scanning || m.handler == nil {
			return m, nil
		}
		m.scanning = true
		m.scanSeen = map[string]bool{}
		m.setFlashText("", false)
		return m, tea.Batch(m.handler.StartScan(), m.spinner.Tick)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m Model) selected() (Peripheral, bool) {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.visible) {
		return Peripheral{}, false
	}
	return m.visible[idx], true
}

func (m Model) dispatchConnect(p Peripheral) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result = Result{Action: ActionConnect, Address: p.Address, Name: p.Name}
		m.done = true
		return m, tea.Quit
	}
	m.busy = true
	m.setFlashText(fmt.Sprintf("Connecting to %s...", displayName(p)), false)
	return m, m.handler.Connect(p.Address)
}

func (m Model) dispatchDisconnect(p Peripheral) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result = Result{Action: ActionDisconnect, Address: p.Address, Name: p.Name}
		m.done = true
		return m, tea.Quit
	}
	m.busy = true
	m.setFlashText(fmt.Sprintf("Disconnecting %s...", displayName(p)), false)
	return m, m.handler.Disconnect(p.Address)
}

func (m Model) dispatchForget(p Peripheral) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result = Result{Action: ActionForget, Address: p.Address, Name: p.Name}
		m.done = true
		return m, tea.Quit
	}
	m.busy = true
	m.setFlashText(fmt.Sprintf("Forgetting %s...", displayName(p)), false)
	return m, m.handler.Forget(p.Address)
}

// applyOptimisticUpdate reflects a completed operation in the local list so the
// table updates immediately. Unlike WiFi, connecting does not clear other rows'
// Connected flag — Bluetooth supports multiple simultaneous connections.
func (m *Model) applyOptimisticUpdate(msg OpResultMsg) {
	for i := range m.peripherals {
		if m.peripherals[i].Address != msg.Address {
			continue
		}
		switch msg.Action {
		case ActionConnect:
			m.peripherals[i].Connected = true
			// A successful connect no longer implies pairing (the agent falls
			// back to a direct connect when pairing fails), so apply the
			// reported state when the agent provides it.
			if !msg.PairedKnown || msg.Paired {
				m.peripherals[i].Paired = true
				m.peripherals[i].Trusted = true
			}
		case ActionDisconnect:
			m.peripherals[i].Connected = false
		case ActionForget:
			m.peripherals[i].Paired = false
			m.peripherals[i].Connected = false
			m.peripherals[i].Trusted = false
		}
	}
	Sort(m.peripherals)
}

func flashFor(msg OpResultMsg) (string, bool) {
	label := msg.Name
	if label == "" {
		label = msg.Address
	}
	if msg.Err != nil {
		reason := userFacingError(msg.Err)
		switch msg.Action {
		case ActionConnect:
			return fmt.Sprintf("Connect to %s failed: %s", label, reason), true
		case ActionDisconnect:
			return fmt.Sprintf("Disconnect %s failed: %s", label, reason), true
		case ActionForget:
			return fmt.Sprintf("Forget %s failed: %s", label, reason), true
		default:
			return reason, true
		}
	}
	switch msg.Action {
	case ActionConnect:
		if msg.PairedKnown && !msg.Paired {
			return fmt.Sprintf("Connected to %s (not paired — the device accepted the connection without pairing).", label), false
		}
		return fmt.Sprintf("Connected to %s.", label), false
	case ActionDisconnect:
		return fmt.Sprintf("Disconnected %s.", label), false
	case ActionForget:
		return fmt.Sprintf("Forgot %s.", label), false
	}
	return "", false
}

// userFacingError renders a remote error for display, preferring the gRPC
// status message so internal "rpc error: code = ..." framing and metadata are
// not surfaced in the UI. Non-gRPC errors fall back to their plain text.
func userFacingError(err error) string {
	if err == nil {
		return ""
	}
	if _, ok := status.FromError(err); ok {
		if desc, descOK := grpcDescFromErrorString(err.Error()); descOK {
			return desc
		}
	}
	return err.Error()
}

func grpcDescFromErrorString(msg string) (string, bool) {
	idx := strings.Index(msg, "desc = ")
	if idx < 0 {
		return "", false
	}
	desc := strings.TrimSpace(msg[idx+len("desc = "):])
	return desc, desc != ""
}

// setFlash sets a transient message and returns a command that clears it after
// flashDuration, unless a newer flash supersedes it first (tracked by token).
func (m *Model) setFlash(msg string, isErr bool) tea.Cmd {
	m.setFlashText(msg, isErr)
	token := m.flashToken
	return func() tea.Msg {
		time.Sleep(flashDuration)
		return flashClearMsg{token: token}
	}
}

// setFlashText sets a message without scheduling an automatic clear, for
// in-progress status that a follow-up message replaces.
func (m *Model) setFlashText(msg string, isErr bool) {
	m.flashMessage = tui.StripControl(msg)
	m.flashIsError = isErr
	m.flashToken++
}

// pruneUnseen returns the peripherals whose address was observed in the
// completed scan (present in seen).
func pruneUnseen(list []Peripheral, seen map[string]bool) []Peripheral {
	kept := make([]Peripheral, 0, len(list))
	for _, p := range list {
		if seen[p.Address] {
			kept = append(kept, p)
		}
	}
	return kept
}

func displayName(p Peripheral) string {
	if name := tui.StripControl(p.Name); strings.TrimSpace(name) != "" {
		return name
	}
	return tui.StripControl(p.Address)
}

var (
	footerStyle     = lipgloss.NewStyle().Foreground(tui.ColorDim)
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	flashStyle      = lipgloss.NewStyle().Foreground(tui.ColorNotice)
	flashErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m Model) View() string {
	if m.done {
		return ""
	}
	var sb strings.Builder

	title := titleStyle.Render("Bluetooth peripherals")
	if m.scanning {
		title += "  " + m.spinner.View() + " scanning… (~8s)"
	}
	sb.WriteString(m.viewLine(title) + "\n\n")

	if len(m.visible) == 0 && !m.scanning {
		text := "No Bluetooth devices found. Press r to rescan."
		if len(m.peripherals) > 0 {
			text = "No named Bluetooth devices found. Press h to show unnamed devices."
		}
		sb.WriteString(m.viewLine(footerStyle.Render(text)) + "\n\n")
	} else {
		sb.WriteString(m.table.View())
		sb.WriteString("\n")
	}

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		}
		sb.WriteString(m.viewLine(style.Render(m.flashMessage)) + "\n")
	}
	visibility := fmt.Sprintf("h show unnamed (%d hidden)", len(m.peripherals)-len(m.visible))
	if m.showUnnamed {
		visibility = "h hide unnamed"
	}
	sb.WriteString(m.viewLine(footerStyle.Render(visibility)) + "\n")

	hint := "↑/↓ move · enter connect · d disconnect · f forget · r rescan · q quit"
	if m.table.CanScroll() {
		hint = "↑/↓ move · ←/→ scroll · enter connect · d disconnect · f forget · r rescan · q quit"
	}
	sb.WriteString(m.viewLine(footerStyle.Render(hint)) + "\n")
	return sb.String()
}

func (m Model) viewLine(line string) string {
	if m.width <= 0 {
		return line
	}
	return tui.CropANSIView(line, 0, m.width)
}

func (m Model) Result() Result { return m.result }
