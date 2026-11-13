package commands

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/unitreecloud"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The old RPC token field is a workflow marker, not an account credential.
const unitreeAccountFlowMarker = unitree.AccountFlowMarker

type unitreePairHandler struct {
	ctx            context.Context
	client         agentpbv2.WendyUnitreeServiceClient
	region         string
	initialIP      string
	transportError error
	login          func(context.Context, string, string, string) (*unitree.Account, error)
}

func unitreePairTransportError(conn *grpcclient.AgentConnection) error {
	if conn != nil && (conn.IsMTLS || conn.IsSessionProxy || strings.HasPrefix(conn.Host, "unix:")) {
		return nil
	}
	return errors.New("Unitree pairing requires an authenticated WendyOS connection; enroll the device or use its local agent socket")
}

func unitreePairLogin(ctx context.Context, region, email, password string) (*unitree.Account, error) {
	client, err := unitreecloud.NewClient(region)
	if err != nil {
		return nil, err
	}
	return client.Login(ctx, email, password)
}

type unitreePairScanMsg struct {
	discovered, paired    []*agentpbv2.UnitreeRobot
	discoveryErr, listErr error
}

type unitreePairLoginMsg struct {
	account *unitree.Account
	err     error
}

type unitreePairOpMsg struct {
	serial string
	robot  *agentpbv2.UnitreeRobot
	forgot bool
	err    error
}

const unitreeAgentTooOld = "Unitree pairing requires a newer WendyOS agent"

func (h *unitreePairHandler) scan(serials []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
		defer cancel()
		listed, listErr := h.client.ListUnitreePairings(ctx, &agentpbv2.ListUnitreePairingsRequest{})
		known := append([]string(nil), serials...)
		seen := make(map[string]bool, len(known))
		for _, serial := range known {
			seen[serial] = true
		}
		for _, robot := range listed.GetRobots() {
			if serial := robot.GetSerial(); serial != "" && !seen[serial] {
				known = append(known, serial)
				seen[serial] = true
			}
		}
		discovered, discoveryErr := h.client.DiscoverUnitreeRobots(ctx, &agentpbv2.DiscoverUnitreeRobotsRequest{KnownSerials: known})
		return unitreePairScanMsg{paired: listed.GetRobots(), discovered: discovered.GetRobots(), listErr: listErr, discoveryErr: discoveryErr}
	}
}

func (h *unitreePairHandler) signIn(email, password string) tea.Cmd {
	return func() tea.Msg {
		if h.transportError != nil {
			return unitreePairLoginMsg{err: h.transportError}
		}
		ctx, cancel := context.WithTimeout(h.ctx, 45*time.Second)
		defer cancel()
		login := h.login
		if login == nil {
			login = unitreePairLogin
		}
		account, err := login(ctx, h.region, email, password)
		if err != nil {
			// Login errors must never expose a vendor response or credentials.
			return unitreePairLoginMsg{err: errors.New("Unitree login failed; check your account, password, region, and internet connection")}
		}
		if account == nil || len(account.Devices) == 0 {
			return unitreePairLoginMsg{err: errors.New("no robots found in this Unitree account; check the account and region")}
		}
		return unitreePairLoginMsg{account: account}
	}
}

func (h *unitreePairHandler) pair(robot *agentpbv2.UnitreeRobot, key string, fromAccount bool) tea.Cmd {
	// Capture a request rather than the mutable table row. Never send a cloud
	// bearer token to the agent; unkeyed firmware needs only a workflow marker.
	req := &agentpbv2.PairUnitreeRequest{Serial: robot.Serial, Name: robot.Name, Address: robot.Address, EncryptionKey: key}
	if fromAccount && key == "" {
		req.Token = unitreeAccountFlowMarker
	}
	return func() tea.Msg {
		if h.transportError != nil {
			return unitreePairOpMsg{serial: req.Serial, err: h.transportError}
		}
		ctx, cancel := context.WithTimeout(h.ctx, 45*time.Second)
		defer cancel()
		resp, err := h.client.PairUnitree(ctx, req)
		if err != nil {
			// The request contains secrets. Do not echo arbitrary remote text.
			message := "Unitree connection failed; check the robot's IP, account, and network"
			if status.Code(err) == codes.Unimplemented {
				message = unitreeAgentTooOld
			}
			return unitreePairOpMsg{serial: req.Serial, err: errors.New(message)}
		}
		if resp.GetRobot() == nil {
			return unitreePairOpMsg{serial: req.Serial, err: errors.New("the WendyOS device returned no Unitree connection status")}
		}
		return unitreePairOpMsg{serial: req.Serial, robot: resp.GetRobot()}
	}
}

func (h *unitreePairHandler) forget(serial string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		_, err := h.client.ForgetUnitree(ctx, &agentpbv2.ForgetUnitreeRequest{Serial: serial})
		return unitreePairOpMsg{serial: serial, forgot: true, err: err}
	}
}

type unitreePairForm int

const (
	unitreeNoForm unitreePairForm = iota
	unitreeLoginForm
	unitreeIPForm
)

type unitreePairModel struct {
	handler                       *unitreePairHandler
	table                         tui.BubbleTable
	discovered, paired, rows      []*agentpbv2.UnitreeRobot
	account                       *unitree.Account
	scanning, busy, pairingsKnown bool
	form                          unitreePairForm
	email, password, address      textinput.Model
	passwordFocused               bool
	editingSerial, pendingIP      string
	message, scanError            string
	width, height                 int
}

func newUnitreePairModel(h *unitreePairHandler) unitreePairModel {
	email, password, address := textinput.New(), textinput.New(), textinput.New()
	email.Prompt, password.Prompt, address.Prompt = "Email: ", "Password: ", "Robot IPv4: "
	password.EchoMode, password.EchoCharacter = textinput.EchoPassword, '*'
	email.CharLimit, password.CharLimit, address.CharLimit = 254, 1024, 64
	m := unitreePairModel{handler: h, scanning: true, email: email, password: password, address: address,
		table: tui.NewBubbleTable(true, []bubbleTable.Column{
			{Title: "Name", Width: 20}, {Title: "Serial", Width: 22}, {Title: "Address", Width: 16}, {Title: "Status", Width: 14},
		}),
	}
	if h != nil {
		m.pendingIP = h.initialIP
	}
	return m
}

func (m unitreePairModel) Init() tea.Cmd {
	if m.handler == nil {
		return func() tea.Msg { return unitreePairScanMsg{} }
	}
	var serials []string
	if m.account != nil {
		for _, d := range m.account.Devices {
			serials = append(serials, d.Serial)
		}
	}
	return m.handler.scan(serials)
}

func (m unitreePairModel) selected() *agentpbv2.UnitreeRobot {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.rows) {
		return nil
	}
	return m.rows[i]
}

func (m unitreePairModel) accountDevice(serial string) *unitree.Device {
	if m.account != nil && serial != "" {
		for i := range m.account.Devices {
			if m.account.Devices[i].Serial == serial {
				return &m.account.Devices[i]
			}
		}
	}
	return nil
}

func (m *unitreePairModel) refreshRows(selectedSerial string) {
	byID := make(map[string]*agentpbv2.UnitreeRobot)
	add := func(d *agentpbv2.UnitreeRobot) {
		if d == nil {
			return
		}
		id := d.Serial
		if id == "" {
			id = "ip:" + d.Address
		}
		prior := byID[id]
		if prior == nil {
			prior = &agentpbv2.UnitreeRobot{Serial: d.Serial}
			byID[id] = prior
		}
		// Rows are added account, saved, then discovered. Keep the first name:
		// discovery only reports a generic model name, never the user's alias.
		if prior.Name == "" {
			prior.Name = d.Name
		}
		if d.Address != "" {
			prior.Address = d.Address
		}
	}
	if m.account != nil {
		for _, d := range m.account.Devices {
			// Account addresses can be stale. Require discovery or a manual IP.
			add(&agentpbv2.UnitreeRobot{Serial: d.Serial, Name: d.Name})
		}
	}
	for _, d := range m.paired {
		add(d)
	}
	for _, d := range m.discovered {
		add(d)
	}
	// The saved-pairing response is authoritative for connection status.
	// A retained discovery snapshot must not make a disconnected robot appear
	// connected after a later scan fails.
	for _, d := range m.paired {
		if d != nil && byID[d.Serial] != nil {
			byID[d.Serial].Paired, byID[d.Serial].Connected = d.Paired, d.Connected
		}
	}
	m.rows = nil
	for _, d := range byID {
		m.rows = append(m.rows, d)
	}
	sort.Slice(m.rows, func(i, j int) bool {
		a, b := m.rows[i], m.rows[j]
		if a.Paired != b.Paired {
			return a.Paired
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Serial != b.Serial {
			return a.Serial < b.Serial
		}
		return a.Address < b.Address
	})
	rows := make([]bubbleTable.Row, 0, len(m.rows))
	cursor := 0
	for i, d := range m.rows {
		state := "discovered"
		if d.Address == "" {
			state = "account"
		}
		if d.Paired {
			state = "saved"
		}
		if d.Connected {
			state = "connected"
		}
		if d.Paired && !m.pairingsKnown {
			state = "unknown"
		}
		name := d.Name
		if name == "" {
			name = "Unitree Go2"
		}
		rows = append(rows, bubbleTable.Row{tui.StripControl(name), tui.StripControl(d.Serial), tui.StripControl(d.Address), state})
		if d.Serial == selectedSerial {
			cursor = i
		}
	}
	m.table.SetRows(rows)
	m.table.SetCursor(cursor)
	height := max(6, len(rows)+2)
	if m.height > 0 {
		height = min(height, max(1, m.height-7))
	}
	m.table.SetHeight(height)
}

func (m unitreePairModel) beginLogin() (tea.Model, tea.Cmd) {
	if m.handler.transportError != nil {
		m.message = m.handler.transportError.Error()
		return m, nil
	}
	m.form, m.passwordFocused, m.message = unitreeLoginForm, false, ""
	m.password.SetValue("")
	m.password.Blur()
	return m, m.email.Focus()
}

func (m unitreePairModel) beginIP(d *agentpbv2.UnitreeRobot) (tea.Model, tea.Cmd) {
	m.form, m.editingSerial = unitreeIPForm, d.Serial
	m.address.SetValue(d.Address)
	if m.pendingIP != "" {
		m.address.SetValue(m.pendingIP)
	}
	m.message = "Enter the robot's IP on the selected WendyOS device's network."
	return m, m.address.Focus()
}

func unitreePairIPv4(address string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || ip.To4() == nil || strings.Contains(address, ":") || !(ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast()) || ip.IsLoopback() {
		return "", errors.New("enter a unicast IPv4 address for the robot")
	}
	return ip.String(), nil
}

func (m unitreePairModel) connect(d *agentpbv2.UnitreeRobot, address string) (tea.Model, tea.Cmd) {
	ip, err := unitreePairIPv4(address)
	if err != nil {
		m.message = err.Error()
		return m, nil
	}
	key := ""
	accountDevice := m.accountDevice(d.Serial)
	if accountDevice != nil {
		key = accountDevice.EncryptionKey
	} else if !d.Paired {
		m.message = "Select the matching robot from your Unitree account before connecting."
		return m, nil
	}
	robot := &agentpbv2.UnitreeRobot{Serial: d.Serial, Name: d.Name, Address: ip}
	m.form, m.busy, m.message = unitreeNoForm, true, "Connecting from the selected WendyOS device..."
	m.address.Blur()
	return m, m.handler.pair(robot, key, accountDevice != nil)
}

func (m unitreePairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table, _ = m.table.Update(msg)
		m.email.Width, m.password.Width, m.address.Width = max(1, msg.Width-12), max(1, msg.Width-12), max(1, msg.Width-14)
		m.refreshRows(m.selected().GetSerial())
		return m, nil
	case unitreePairScanMsg:
		m.scanning, m.pairingsKnown = false, msg.listErr == nil
		selected := m.selected().GetSerial()
		var errs []string
		if msg.listErr != nil {
			errs = append(errs, "Saved Unitree pairings: "+userFacingGRPCError(msg.listErr))
		} else {
			m.paired = msg.paired
		}
		if msg.discoveryErr != nil {
			errs = append(errs, "Unitree discovery: "+userFacingGRPCError(msg.discoveryErr))
		} else {
			m.discovered = msg.discovered
		}
		m.scanError = strings.Join(errs, "; ")
		if status.Code(msg.listErr) == codes.Unimplemented || status.Code(msg.discoveryErr) == codes.Unimplemented {
			m.scanError = unitreeAgentTooOld
		}
		m.refreshRows(selected)
		return m, nil
	case unitreePairLoginMsg:
		m.busy = false
		if msg.err != nil {
			m.message = msg.err.Error()
			return m, nil
		}
		m.account = msg.account
		m.refreshRows(m.editingSerial)
		m.message = "Select your account robot, then press Enter to connect or i to enter its IP."
		if m.pendingIP != "" {
			m.message = "Select the account robot at " + m.pendingIP + ", then press Enter to connect."
		}
		m.scanning = true
		return m, m.Init()
	case unitreePairOpMsg:
		m.busy = false
		if msg.err != nil {
			m.message = "Unitree request failed: " + userFacingGRPCError(msg.err)
			return m, nil
		}
		paired := make([]*agentpbv2.UnitreeRobot, 0, len(m.paired)+1)
		for _, d := range m.paired {
			if d.Serial != msg.serial {
				paired = append(paired, d)
			}
		}
		m.paired = paired
		if msg.forgot {
			m.message = "Forgot Unitree " + msg.serial + " and its saved key."
		} else {
			m.paired = append(m.paired, msg.robot)
			// An explicit IP change supersedes the earlier discovery address.
			discovered := make([]*agentpbv2.UnitreeRobot, 0, len(m.discovered))
			for _, d := range m.discovered {
				if d.GetSerial() != msg.serial {
					discovered = append(discovered, d)
				}
			}
			m.discovered = discovered
			m.message = "Saved Unitree " + msg.serial + ". The robot is not connected."
			if msg.robot.GetConnected() {
				m.message = "Connected to Unitree " + msg.serial + " from the selected WendyOS device."
			}
			m.pendingIP = ""
		}
		m.refreshRows(msg.serial)
		return m, nil
	case tea.KeyMsg:
		if m.form != unitreeNoForm {
			return m.updateForm(msg)
		}
		switch msg.String() {
		case "r":
			if m.busy || m.scanning {
				return m, nil
			}
			m.scanning = true
			return m, m.Init()
		case "l", "i", "enter", "f":
			if m.busy || m.scanning || m.handler == nil {
				return m, nil
			}
			if msg.String() == "l" {
				return m.beginLogin()
			}
			if !m.pairingsKnown {
				m.message = "Refresh saved pairings before making changes."
				return m, nil
			}
			d := m.selected()
			if msg.String() == "f" {
				if d == nil || !d.Paired {
					return m, nil
				}
				m.busy, m.message = true, "Forgetting Unitree pairing..."
				return m, m.handler.forget(d.Serial)
			}
			if d != nil {
				m.editingSerial = d.Serial
			}
			if (d == nil || !d.Paired) && m.account == nil {
				return m.beginLogin()
			}
			if d == nil {
				m.message = "Log in to a Unitree account with a Go2 robot."
				return m, nil
			}
			if !d.Paired && m.accountDevice(d.Serial) == nil {
				m.message = "Select the matching account serial, then press i to enter this robot's IP."
				return m, nil
			}
			if msg.String() == "i" {
				return m.beginIP(d)
			}
			address := d.Address
			if m.pendingIP != "" {
				address = m.pendingIP
			}
			if address == "" {
				return m.beginIP(d)
			}
			return m.connect(d, address)
		}
	}
	if m.form == unitreeLoginForm {
		var cmd tea.Cmd
		if m.passwordFocused {
			m.password, cmd = m.password.Update(msg)
		} else {
			m.email, cmd = m.email.Update(msg)
		}
		return m, cmd
	}
	if m.form == unitreeIPForm {
		var cmd tea.Cmd
		m.address, cmd = m.address.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *unitreePairModel) clearForm() {
	m.form = unitreeNoForm
	m.password.SetValue("")
	m.email.Blur()
	m.password.Blur()
	m.address.Blur()
}

func (m unitreePairModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.clearForm()
		return m, nil
	}
	if m.form == unitreeIPForm {
		switch msg.String() {
		case "tab", "shift+tab":
			return m, nil
		case "enter":
			for _, d := range m.rows {
				if d.Serial == m.editingSerial {
					return m.connect(d, m.address.Value())
				}
			}
			m.message = "The selected robot is no longer listed. Cancel and select it again."
			return m, nil
		}
		var cmd tea.Cmd
		m.address, cmd = m.address.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "tab", "shift+tab", "up", "down":
		m.passwordFocused = !m.passwordFocused
		if m.passwordFocused {
			m.email.Blur()
			return m, m.password.Focus()
		}
		m.password.Blur()
		return m, m.email.Focus()
	case "enter":
		if strings.TrimSpace(m.email.Value()) == "" {
			m.message = "Enter your Unitree account email."
			return m, nil
		}
		if !m.passwordFocused {
			m.passwordFocused = true
			m.email.Blur()
			return m, m.password.Focus()
		}
		if m.password.Value() == "" {
			m.message = "Enter your Unitree password."
			return m, nil
		}
		cmd := m.handler.signIn(strings.TrimSpace(m.email.Value()), m.password.Value())
		m.clearForm()
		m.busy, m.message = true, "Logging in to Unitree and fetching robot keys..."
		return m, cmd
	}
	var cmd tea.Cmd
	if m.passwordFocused {
		m.password, cmd = m.password.Update(msg)
	} else {
		m.email, cmd = m.email.Update(msg)
	}
	return m, cmd
}

func (m unitreePairModel) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("Unitree Go2")
	if m.scanning {
		title += "  Discovering on the WendyOS device's LAN..."
	}
	body := title + "\n\n"
	switch m.form {
	case unitreeLoginForm:
		region := m.handler.region
		if region == "" {
			region = "global"
		}
		body += "Unitree account (" + region + ")\n" + m.email.View() + "\n" + m.password.View() + "\n\n"
	case unitreeIPForm:
		body += "Connect Unitree " + tui.StripControl(m.editingSerial) + "\n" + m.address.View() + "\n\n"
	default:
		if len(m.rows) == 0 && !m.scanning {
			body += "No Unitree robots found. Press l to log in, or i to connect by IP.\n"
		} else {
			body += m.table.View() + "\n"
		}
	}
	if m.scanError != "" {
		body += tui.StripControl(m.scanError) + "\n"
	}
	if m.message != "" {
		body += tui.StripControl(m.message) + "\n"
	}
	hint := "↑/↓ move · enter connect · l login · i enter IP · f forget · r rescan · q quit"
	if m.form == unitreeLoginForm {
		hint = "tab next field · enter continue / login · esc cancel"
	}
	if m.form == unitreeIPForm {
		hint = "enter connect · esc cancel"
	}
	body += devicePickerTabInactive.Render(hint) + "\n"
	if m.width > 0 {
		body = tui.CropANSIView(body, 0, m.width)
	}
	return body
}
