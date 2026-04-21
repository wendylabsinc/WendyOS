package wifitable

import (
	"fmt"
	"strings"

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

// Action is the intent the user picked when the TUI exited. The caller is
// responsible for actually talking to the agent; keeping the TUI pure makes it
// testable.
type Action int

const (
	ActionNone Action = iota
	ActionQuit
	ActionConnect
	ActionReorder
	ActionForget
	ActionConnectUnlisted
)

// Result is what the caller reads after the TUI exits.
type Result struct {
	Action         Action
	SSID           string
	Password       string
	Security       agentpb.WiFiSecurityType
	Hidden         bool
	Order          []string // for ActionReorder
	PromptPassword bool     // the caller should prompt out-of-band
}

// RefreshMsg replaces the visible networks. The caller fires one of these on a
// timer to keep the list fresh while the user browses.
type RefreshMsg struct {
	Networks []Network
}

// Model is the Bubble Tea model for the interactive WiFi table.
type Model struct {
	networks []Network
	table    bubbleTable.Model
	mode     mode

	// ranking state
	origOrder []string

	// unlisted-network modal state
	ssidInput     textinput.Model
	passwordInput textinput.Model
	secIndex      int
	modalFocus    int // 0=ssid, 1=password, 2=security

	// per-row password prompt
	pwFor string

	message string
	result  Result
	done    bool
	width   int
	height  int
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
			m.networks = msg.Networks
			Sort(m.networks)
			m.refreshRows()
		}
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
		idx := m.table.Cursor()
		if idx < 0 || idx >= len(m.networks) {
			return m, nil
		}
		n := m.networks[idx]
		m.result.SSID = n.SSID
		m.result.Security = n.Security
		if n.Known || !IsSecured(n.Security) {
			m.result.Action = ActionConnect
			m.done = true
			return m, tea.Quit
		}
		// Unknown + secured → prompt for password inline.
		m.pwFor = n.SSID
		m.passwordInput.SetValue("")
		m.passwordInput.Focus()
		m.mode = modePassword
		return m, textinput.Blink
	case "r":
		if !m.hasKnown() {
			m.message = "No known networks to rank."
			return m, nil
		}
		m.origOrder = snapshotSSIDs(m.networks)
		m.mode = modeRanking
		m.message = ""
		return m, nil
	case "n":
		m.mode = modeUnlisted
		m.modalFocus = 0
		m.ssidInput.SetValue("")
		m.passwordInput.SetValue("")
		m.ssidInput.Focus()
		m.passwordInput.Blur()
		return m, textinput.Blink
	case "f":
		idx := m.table.Cursor()
		if idx < 0 || idx >= len(m.networks) {
			return m, nil
		}
		if !m.networks[idx].Known {
			m.message = "Only known networks can be forgotten."
			return m, nil
		}
		m.result.Action = ActionForget
		m.result.SSID = m.networks[idx].SSID
		m.done = true
		return m, tea.Quit
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
		m.message = "Cancelled rank edit."
		m.refreshRows()
		return m, nil
	case "enter":
		m.result.Action = ActionReorder
		m.result.Order = KnownSSIDsInOrder(m.networks)
		m.done = true
		return m, tea.Quit
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
			m.message = "Network name is required."
			return m, nil
		}
		m.result.Action = ActionConnectUnlisted
		m.result.SSID = ssid
		m.result.Password = m.passwordInput.Value()
		m.result.Security = securityOptions[m.secIndex]
		m.result.Hidden = true
		m.done = true
		return m, tea.Quit
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
		m.result.Action = ActionConnect
		m.result.SSID = m.pwFor
		m.result.Password = m.passwordInput.Value()
		m.done = true
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.passwordInput, cmd = m.passwordInput.Update(msg)
	return m, cmd
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
	footerStyle   = lipgloss.NewStyle().Foreground(tui.ColorDim)
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	modalBorder   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(tui.ColorBorder).Padding(0, 1)
	modalSelected = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	messageStyle  = lipgloss.NewStyle().Foreground(tui.ColorNotice)
)

func (m Model) View() string {
	if m.done {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("WiFi networks") + "\n\n")
	sb.WriteString(m.table.View())
	sb.WriteString("\n")

	if m.message != "" {
		sb.WriteString(messageStyle.Render(m.message) + "\n")
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

// Result returns the user's decision after Run returns.
func (m Model) Result() Result { return m.result }
