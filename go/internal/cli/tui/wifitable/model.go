package wifitable

import (
	"fmt"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wendylabsinc/wendy/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// mode tracks which sub-view the model is showing.
type mode int

const (
	modeBrowsing mode = iota
	modeRanking
	modeUnlisted
	modePassword
)

// Action is the intent the user picked. When a Handler is attached the model
// dispatches Actions as async tea.Cmds and stays open; with no Handler set it
// falls back to recording the Action on Result and quitting (used by tests).
type Action int

const (
	ActionNone Action = iota
	ActionQuit
	ActionConnect
	ActionReorder
	ActionForget
	ActionConnectUnlisted
)

// Result is what the caller reads after the TUI exits. Only populated for the
// no-Handler code path.
type Result struct {
	Action         Action
	SSID           string
	Password       string
	Security       agentpb.WiFiSecurityType
	Hidden         bool
	Order          []string // for ActionReorder
	PromptPassword bool     // the caller should prompt out-of-band
}

// Handler performs WiFi operations on behalf of the Model so the TUI can stay
// open between actions (mirroring the `wendy discover` screen). Each method
// returns a tea.Cmd that must eventually emit an OpResultMsg.
type Handler interface {
	Connect(ssid, password string, security agentpb.WiFiSecurityType, hidden bool) tea.Cmd
	Forget(ssid string) tea.Cmd
	Reorder(order []string) tea.Cmd
	Refresh() tea.Cmd
}

// RefreshMsg replaces the visible networks. The caller fires one of these on a
// timer — or as the result of a Handler.Refresh() call — to keep the list
// fresh while the user browses.
type RefreshMsg struct {
	Networks []Network
}

// OpResultMsg is sent by Handler commands to report the outcome of an async
// operation. The Model uses it to render a flash message and refresh the list.
type OpResultMsg struct {
	Action Action
	SSID   string
	Count  int // e.g. number of networks reordered
	Err    error
}

// flashClearMsg clears the current flash message after a delay.
type flashClearMsg struct{}

const flashDuration = 4 * time.Second

// Model is the Bubble Tea model for the interactive WiFi table.
type Model struct {
	networks []Network
	table    bubbleTable.Model
	mode     mode

	handler Handler

	// ranking state
	origOrder []string

	// unlisted-network modal state
	ssidInput     textinput.Model
	passwordInput textinput.Model
	secIndex      int
	modalFocus    int // 0=ssid, 1=password, 2=security

	// per-row password prompt
	pwFor string

	flashMessage string
	flashIsError bool
	busy         bool // true while an op is in-flight
	// stickyConnectedSSID is the SSID of the most recent successful connect.
	// It survives RefreshMsgs that haven't yet reflected the new state
	// (nmcli rescan can lag behind association by a few seconds). Cleared
	// once a RefreshMsg confirms the SSID as known+connected, or on a
	// subsequent Forget/Connect.
	stickyConnectedSSID string
	result              Result
	done                bool
	width               int
	height              int
}

var securityOptions = []agentpb.WiFiSecurityType{
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP,
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA_PSK,
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK,
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE,
	agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE,
}

// NewModel constructs the initial model with the given network list.
func NewModel(networks []Network) Model {
	Sort(networks)

	ti := textinput.New()
	ti.Placeholder = "Network name"
	ti.CharLimit = 64
	ti.Width = 32

	pw := textinput.New()
	pw.Placeholder = "Password"
	pw.EchoMode = textinput.EchoPassword
	pw.EchoCharacter = '•'
	pw.CharLimit = 128
	pw.Width = 32

	m := Model{
		networks:      networks,
		table:         tui.NewBubbleTable(true, wifiColumns()),
		mode:          modeBrowsing,
		ssidInput:     ti,
		passwordInput: pw,
		secIndex:      3, // WPA2-PSK default
	}
	m.refreshRows()
	return m
}

// WithHandler attaches a Handler, enabling inline async execution of actions
// so the TUI stays open between edits.
func (m Model) WithHandler(h Handler) Model {
	m.handler = h
	return m
}

func (m Model) Init() tea.Cmd { return nil }

func wifiColumns() []bubbleTable.Column {
	return []bubbleTable.Column{
		{Title: "SSID", Width: 28},
		{Title: "Known", Width: 6},
		{Title: "Status", Width: 10},
		{Title: "Security", Width: 10},
		{Title: "Signal", Width: 8},
	}
}

func (m *Model) refreshRows() {
	rows := make([]bubbleTable.Row, 0, len(m.networks))
	for _, n := range m.networks {
		known := ""
		if n.Known {
			known = "★"
		}
		status := ""
		if n.Connected {
			status = "Connected"
		}
		signal := ""
		if n.Signal > 0 {
			signal = fmt.Sprintf("%d%%", n.Signal)
		}
		rows = append(rows, bubbleTable.Row{n.SSID, known, status, SecurityLabel(n.Security), signal})
	}
	m.table.SetRows(rows)
	if cur := m.table.Cursor(); cur >= len(rows) && len(rows) > 0 {
		m.table.SetCursor(len(rows) - 1)
	}
	h := len(rows) + 2
	if h < 6 {
		h = 6
	}
	if m.height > 0 && h > m.height-6 {
		h = m.height - 6
	}
	m.table.SetHeight(h)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.refreshRows()
		return m, nil

	case RefreshMsg:
		if m.mode == modeBrowsing {
			m.networks = m.reconcileRefresh(msg.Networks)
			Sort(m.networks)
			m.refreshRows()
		}
		return m, nil

	case OpResultMsg:
		m.busy = false
		m.flashMessage, m.flashIsError = flashFor(msg)
		if msg.Err != nil {
			return m, clearFlashAfter(flashDuration)
		}
		// Apply an optimistic update so the table reflects the change
		// immediately — the async Refresh that follows will reconcile with
		// the authoritative state from the device.
		m.applyOptimisticUpdate(msg)
		m.refreshRows()
		if m.handler != nil {
			return m, tea.Batch(m.handler.Refresh(), clearFlashAfter(flashDuration))
		}
		return m, clearFlashAfter(flashDuration)

	case flashClearMsg:
		m.flashMessage = ""
		m.flashIsError = false
		return m, nil
	}

	switch m.mode {
	case modeBrowsing:
		return m.updateBrowsing(msg)
	case modeRanking:
		return m.updateRanking(msg)
	case modeUnlisted:
		return m.updateUnlisted(msg)
	case modePassword:
		return m.updatePassword(msg)
	}
	return m, nil
}

func (m Model) updateBrowsing(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	switch km.String() {
	case "q", "ctrl+c", "esc":
		m.result.Action = ActionQuit
		m.done = true
		return m, tea.Quit
	case "enter":
		if m.busy {
			return m, nil
		}
		idx := m.table.Cursor()
		if idx < 0 || idx >= len(m.networks) {
			return m, nil
		}
		n := m.networks[idx]
		if n.Known {
			// nmcli will reuse the saved profile (and its stored password).
			return m.dispatchConnect(n.SSID, "", n.Security, false)
		}
		if n.Security == agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN {
			return m.dispatchConnect(n.SSID, "", n.Security, false)
		}
		// Unknown with a secured or ambiguous (UNSPECIFIED) security type →
		// prompt for a password. Treating UNSPECIFIED as open is unsafe,
		// since many drivers omit the security field in scan output. The
		// password input accepts an empty value for networks that turn out
		// to be open.
		m.pwFor = n.SSID
		m.passwordInput.SetValue("")
		m.passwordInput.Focus()
		m.mode = modePassword
		return m, textinput.Blink
	case "r":
		if m.busy {
			return m, nil
		}
		if !m.hasKnown() {
			m.flashMessage = "No known networks to rank."
			m.flashIsError = true
			return m, clearFlashAfter(flashDuration)
		}
		m.origOrder = snapshotSSIDs(m.networks)
		m.mode = modeRanking
		m.flashMessage = ""
		m.flashIsError = false
		return m, nil
	case "n":
		if m.busy {
			return m, nil
		}
		m.mode = modeUnlisted
		m.modalFocus = 0
		m.ssidInput.SetValue("")
		m.passwordInput.SetValue("")
		m.ssidInput.Focus()
		m.passwordInput.Blur()
		return m, textinput.Blink
	case "f":
		if m.busy {
			return m, nil
		}
		idx := m.table.Cursor()
		if idx < 0 || idx >= len(m.networks) {
			return m, nil
		}
		if !m.networks[idx].Known {
			m.flashMessage = "Only known networks can be forgotten."
			m.flashIsError = true
			return m, clearFlashAfter(flashDuration)
		}
		return m.dispatchForget(m.networks[idx].SSID)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m Model) updateRanking(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch km.String() {
	case "esc":
		// Restore order.
		m.networks = restoreOrder(m.networks, m.origOrder)
		m.mode = modeBrowsing
		m.flashMessage = "Cancelled rank edit."
		m.flashIsError = false
		m.refreshRows()
		return m, clearFlashAfter(flashDuration)
	case "enter":
		order := KnownSSIDsInOrder(m.networks)
		return m.dispatchReorder(order)
	case "up", "k":
		idx := m.table.Cursor()
		newIdx := MoveUp(m.networks, idx)
		m.table.SetCursor(newIdx)
		m.refreshRows()
		m.table.SetCursor(newIdx)
		return m, nil
	case "down", "j":
		idx := m.table.Cursor()
		newIdx := MoveDown(m.networks, idx)
		m.table.SetCursor(newIdx)
		m.refreshRows()
		m.table.SetCursor(newIdx)
		return m, nil
	}
	return m, nil
}

func (m Model) updateUnlisted(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch km.String() {
	case "esc":
		m.mode = modeBrowsing
		return m, nil
	case "tab":
		m.modalFocus = (m.modalFocus + 1) % 3
		m.syncModalFocus()
		return m, nil
	case "shift+tab":
		m.modalFocus = (m.modalFocus + 2) % 3
		m.syncModalFocus()
		return m, nil
	case "left":
		if m.modalFocus == 2 {
			m.secIndex = (m.secIndex - 1 + len(securityOptions)) % len(securityOptions)
		}
	case "right":
		if m.modalFocus == 2 {
			m.secIndex = (m.secIndex + 1) % len(securityOptions)
		}
	case "enter":
		ssid := strings.TrimSpace(m.ssidInput.Value())
		if ssid == "" {
			m.flashMessage = "Network name is required."
			m.flashIsError = true
			return m, clearFlashAfter(flashDuration)
		}
		return m.dispatchConnectUnlisted(ssid, m.passwordInput.Value(), securityOptions[m.secIndex])
	}

	var cmd tea.Cmd
	switch m.modalFocus {
	case 0:
		m.ssidInput, cmd = m.ssidInput.Update(msg)
	case 1:
		m.passwordInput, cmd = m.passwordInput.Update(msg)
	}
	return m, cmd
}

func (m *Model) syncModalFocus() {
	if m.modalFocus == 0 {
		m.ssidInput.Focus()
		m.passwordInput.Blur()
	} else if m.modalFocus == 1 {
		m.ssidInput.Blur()
		m.passwordInput.Focus()
	} else {
		m.ssidInput.Blur()
		m.passwordInput.Blur()
	}
}

func (m Model) updatePassword(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.passwordInput, cmd = m.passwordInput.Update(msg)
		return m, cmd
	}
	switch km.String() {
	case "esc":
		m.mode = modeBrowsing
		return m, nil
	case "enter":
		ssid := m.pwFor
		password := m.passwordInput.Value()
		return m.dispatchConnect(ssid, password, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED, false)
	}
	var cmd tea.Cmd
	m.passwordInput, cmd = m.passwordInput.Update(msg)
	return m, cmd
}

// dispatchConnect either fires the Handler (staying open) or records the
// action on Result and quits (legacy test path).
func (m Model) dispatchConnect(ssid, password string, sec agentpb.WiFiSecurityType, hidden bool) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result.Action = ActionConnect
		m.result.SSID = ssid
		m.result.Password = password
		m.result.Security = sec
		m.result.Hidden = hidden
		m.done = true
		return m, tea.Quit
	}
	m.mode = modeBrowsing
	m.busy = true
	m.flashMessage = "Connecting to " + ssid + "..."
	m.flashIsError = false
	return m, m.handler.Connect(ssid, password, sec, hidden)
}

func (m Model) dispatchConnectUnlisted(ssid, password string, sec agentpb.WiFiSecurityType) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result.Action = ActionConnectUnlisted
		m.result.SSID = ssid
		m.result.Password = password
		m.result.Security = sec
		m.result.Hidden = true
		m.done = true
		return m, tea.Quit
	}
	m.mode = modeBrowsing
	m.busy = true
	m.flashMessage = "Connecting to " + ssid + "..."
	m.flashIsError = false
	return m, m.handler.Connect(ssid, password, sec, true)
}

func (m Model) dispatchForget(ssid string) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result.Action = ActionForget
		m.result.SSID = ssid
		m.done = true
		return m, tea.Quit
	}
	m.busy = true
	m.flashMessage = "Forgetting " + ssid + "..."
	m.flashIsError = false
	return m, m.handler.Forget(ssid)
}

func (m Model) dispatchReorder(order []string) (tea.Model, tea.Cmd) {
	if m.handler == nil {
		m.result.Action = ActionReorder
		m.result.Order = order
		m.done = true
		return m, tea.Quit
	}
	m.mode = modeBrowsing
	m.busy = true
	m.flashMessage = "Updating ranking..."
	m.flashIsError = false
	return m, m.handler.Reorder(order)
}

// applyOptimisticUpdate mutates the local network list to reflect a completed
// operation, so the table updates immediately without waiting for the async
// refresh (which rescans WiFi on the device and can take several seconds).
func (m *Model) applyOptimisticUpdate(msg OpResultMsg) {
	switch msg.Action {
	case ActionConnect, ActionConnectUnlisted:
		m.stickyConnectedSSID = msg.SSID
		found := false
		for i := range m.networks {
			if m.networks[i].SSID == msg.SSID {
				m.networks[i].Known = true
				m.networks[i].Connected = true
				found = true
			} else {
				m.networks[i].Connected = false
			}
		}
		if !found {
			// Unlisted/hidden network — add a row so it shows up until the
			// refresh fills in the scan details.
			m.networks = append(m.networks, Network{
				SSID:      msg.SSID,
				Known:     true,
				Connected: true,
			})
		}
		Sort(m.networks)

	case ActionForget:
		if m.stickyConnectedSSID == msg.SSID {
			m.stickyConnectedSSID = ""
		}
		out := make([]Network, 0, len(m.networks))
		for _, n := range m.networks {
			if n.SSID == msg.SSID {
				// If not currently visible in the scan, drop the row; it was
				// only shown because it was a saved profile.
				if n.Signal == 0 {
					continue
				}
				n.Known = false
				n.Priority = 0
				n.Connected = false
			}
			out = append(out, n)
		}
		m.networks = out
		Sort(m.networks)
	}
}

// reconcileRefresh merges an authoritative RefreshMsg with the Model's sticky
// state. nmcli can briefly return a scan that's missing IsConnected/IsKnown
// for the network we just connected to (rescan lags activation). When that
// happens we keep the optimistic flags so the user doesn't see the row flap
// back to unknown/disconnected. The sticky SSID is cleared once the refresh
// confirms it as known+connected.
func (m *Model) reconcileRefresh(incoming []Network) []Network {
	if m.stickyConnectedSSID == "" {
		return incoming
	}
	confirmed := false
	foundRow := false
	for i := range incoming {
		if incoming[i].SSID == m.stickyConnectedSSID {
			foundRow = true
			if incoming[i].Known && incoming[i].Connected {
				confirmed = true
			} else {
				incoming[i].Known = true
				incoming[i].Connected = true
			}
		} else if incoming[i].Connected {
			// Only one network is connected at a time; the refresh may
			// legitimately disagree, but trust our recent successful connect
			// over the scan.
			incoming[i].Connected = false
		}
	}
	if !foundRow {
		// Sticky SSID dropped out of the scan — keep a placeholder row.
		incoming = append(incoming, Network{
			SSID:      m.stickyConnectedSSID,
			Known:     true,
			Connected: true,
		})
	}
	if confirmed {
		m.stickyConnectedSSID = ""
	}
	return incoming
}

// flashFor renders a user-facing message for a completed operation.
func flashFor(msg OpResultMsg) (string, bool) {
	if msg.Err != nil {
		switch msg.Action {
		case ActionConnect, ActionConnectUnlisted:
			return fmt.Sprintf("Connect to %s failed: %v", msg.SSID, msg.Err), true
		case ActionForget:
			return fmt.Sprintf("Forget %s failed: %v", msg.SSID, msg.Err), true
		case ActionReorder:
			return fmt.Sprintf("Reorder failed: %v", msg.Err), true
		default:
			return msg.Err.Error(), true
		}
	}
	switch msg.Action {
	case ActionConnect, ActionConnectUnlisted:
		return "Connected to " + msg.SSID + ".", false
	case ActionForget:
		return "Forgot " + msg.SSID + ".", false
	case ActionReorder:
		return fmt.Sprintf("Updated priority for %d known networks.", msg.Count), false
	}
	return "", false
}

func clearFlashAfter(d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return flashClearMsg{}
	}
}

func (m Model) hasKnown() bool {
	for _, n := range m.networks {
		if n.Known {
			return true
		}
	}
	return false
}

// snapshotSSIDs captures the ordering of known SSIDs before a rank edit starts.
func snapshotSSIDs(networks []Network) []string {
	out := make([]string, 0, len(networks))
	for _, n := range networks {
		if n.Known {
			out = append(out, n.SSID)
		}
	}
	return out
}

// restoreOrder rearranges the slice so the known networks appear in origOrder,
// leaving unknown networks untouched.
func restoreOrder(networks []Network, origOrder []string) []Network {
	known := make(map[string]Network, len(origOrder))
	var unknown []Network
	for _, n := range networks {
		if n.Known {
			known[n.SSID] = n
		} else {
			unknown = append(unknown, n)
		}
	}
	out := make([]Network, 0, len(networks))
	for _, ssid := range origOrder {
		if n, ok := known[ssid]; ok {
			out = append(out, n)
		}
	}
	out = append(out, unknown...)
	return out
}

var (
	footerStyle     = lipgloss.NewStyle().Foreground(tui.ColorDim)
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	modalBorder     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(tui.ColorBorder).Padding(0, 1)
	modalSelected   = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	flashStyle      = lipgloss.NewStyle().Foreground(tui.ColorNotice)
	flashErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m Model) View() string {
	if m.done {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("WiFi networks") + "\n\n")
	sb.WriteString(m.table.View())
	sb.WriteString("\n")

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		}
		sb.WriteString(style.Render(m.flashMessage) + "\n")
	}

	switch m.mode {
	case modeBrowsing:
		sb.WriteString(footerStyle.Render("↑/↓ move · enter connect · r rank · n new · f forget · q quit") + "\n")
	case modeRanking:
		sb.WriteString(footerStyle.Render("rank mode: ↑/↓ reorder · enter commit · esc cancel") + "\n")
	case modePassword:
		sb.WriteString(titleStyle.Render("Password for "+m.pwFor) + "\n")
		sb.WriteString(m.passwordInput.View() + "\n")
		sb.WriteString(footerStyle.Render("enter connect · esc cancel") + "\n")
	case modeUnlisted:
		sb.WriteString(m.renderUnlistedModal() + "\n")
	}
	return sb.String()
}

func (m Model) renderUnlistedModal() string {
	ssidLabel := "SSID"
	pwLabel := "Password"
	secLabel := "Security"
	if m.modalFocus == 0 {
		ssidLabel = modalSelected.Render("SSID")
	}
	if m.modalFocus == 1 {
		pwLabel = modalSelected.Render("Password")
	}
	if m.modalFocus == 2 {
		secLabel = modalSelected.Render("Security")
	}

	secValue := SecurityLabel(securityOptions[m.secIndex])
	if m.modalFocus == 2 {
		secValue = "← " + secValue + " →"
	}

	body := fmt.Sprintf("%s\n%s\n\n%s\n%s\n\n%s: %s",
		ssidLabel, m.ssidInput.View(),
		pwLabel, m.passwordInput.View(),
		secLabel, secValue,
	)

	return modalBorder.Render("Unlisted network\n\n"+body) + "\n" +
		footerStyle.Render("tab switch fields · ←/→ change security · enter submit · esc cancel")
}

// Result returns the user's decision after Run returns. Only meaningful when
// the Model was driven without a Handler (e.g. tests); in interactive mode the
// TUI stays open and actions are dispatched via the Handler.
func (m Model) Result() Result { return m.result }
