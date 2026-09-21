package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cameraPairClient interface {
	RefreshCameras(context.Context, *agentpb.RefreshCamerasRequest, ...grpc.CallOption) (*agentpb.RefreshCamerasResponse, error)
	ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error)
	SetCameraCredentials(context.Context, *agentpb.SetCameraCredentialsRequest, ...grpc.CallOption) (*agentpb.SetCameraCredentialsResponse, error)
	ForgetCamera(context.Context, *agentpb.ForgetCameraRequest, ...grpc.CallOption) (*agentpb.ForgetCameraResponse, error)
}

type cameraPairHandler struct {
	ctx    context.Context
	client cameraPairClient
}

type cameraPairScanMsg struct {
	devices []*agentpb.VideoDevice
	err     error
}

type cameraPairOpMsg struct {
	id     uint32
	forgot bool
	err    error
}

func (h *cameraPairHandler) scan() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		resp, err := h.client.RefreshCameras(ctx, &agentpb.RefreshCamerasRequest{})
		if status.Code(err) == codes.Unimplemented {
			listed, listErr := h.client.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
			return cameraPairScanMsg{devices: listed.GetDevices(), err: listErr}
		}
		return cameraPairScanMsg{devices: resp.GetDevices(), err: err}
	}
}

func (h *cameraPairHandler) pair(id uint32, username, password string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()
		_, err := h.client.SetCameraCredentials(ctx, &agentpb.SetCameraCredentialsRequest{
			DeviceId: id, Username: username, Password: password,
		})
		if err != nil {
			// Remote text may contain encoded or partial credentials.
			err = errors.New("saving camera login failed")
		}
		return cameraPairOpMsg{id: id, err: err}
	}
}

func (h *cameraPairHandler) forget(id uint32) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()
		_, err := h.client.ForgetCamera(ctx, &agentpb.ForgetCameraRequest{DeviceId: id})
		return cameraPairOpMsg{id: id, forgot: true, err: err}
	}
}

type cameraPairModel struct {
	handler                 *cameraPairHandler
	table                   tui.BubbleTable
	devices                 []*agentpb.VideoDevice
	scanning, busy, editing bool
	username, password      textinput.Model
	passwordFocused         bool
	editingID               uint32
	message                 string
	width, height           int
}

func newCameraPairModel(h *cameraPairHandler) cameraPairModel {
	username := textinput.New()
	username.Prompt = "Username: "
	username.SetValue(defaultCameraUser)
	password := textinput.New()
	password.Prompt = "Password: "
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '*'
	return cameraPairModel{
		handler: h, scanning: true, username: username, password: password,
		table: tui.NewBubbleTable(true, []bubbleTable.Column{
			{Title: "Name", Width: 28}, {Title: "Address", Width: 24},
			{Title: "Paired", Width: 8}, {Title: "Status", Width: 12},
		}),
	}
}

func (m cameraPairModel) Init() tea.Cmd {
	if m.handler == nil {
		return func() tea.Msg { return cameraPairScanMsg{} }
	}
	return m.handler.scan()
}

func (m cameraPairModel) selected() *agentpb.VideoDevice {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.devices) {
		return nil
	}
	return m.devices[i]
}

func (m *cameraPairModel) refreshRows(selectedID uint32) {
	sort.Slice(m.devices, func(i, j int) bool {
		a, b := m.devices[i], m.devices[j]
		if a.HasCredentials != b.HasCredentials {
			return a.HasCredentials
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Id < b.Id
	})
	rows := make([]bubbleTable.Row, 0, len(m.devices))
	cursor := 0
	for i, d := range m.devices {
		paired, state := "", "offline"
		if d.HasCredentials {
			paired = "yes"
		}
		if d.Online {
			state = "online"
		}
		name := d.Name
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("Camera %d", d.Id)
		}
		rows = append(rows, bubbleTable.Row{tui.StripControl(name), tui.StripControl(d.Address), paired, state})
		if d.Id == selectedID {
			cursor = i
		}
	}
	m.table.SetRows(rows)
	m.table.SetCursor(cursor)
	height := max(6, len(rows)+2)
	if m.height > 0 {
		height = min(height, max(1, m.height-6))
	}
	m.table.SetHeight(height)
}

func (m cameraPairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table, _ = m.table.Update(msg)
		m.username.Width, m.password.Width = max(1, msg.Width-12), max(1, msg.Width-12)
		m.refreshRows(m.selected().GetId())
		return m, nil
	case cameraPairScanMsg:
		m.scanning = false
		if msg.err != nil {
			m.message = "Camera discovery failed: " + userFacingGRPCError(msg.err)
			return m, nil
		}
		selectedID := m.selected().GetId()
		m.devices = nil
		for _, d := range msg.devices {
			if d.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
				m.devices = append(m.devices, d)
			}
		}
		m.refreshRows(selectedID)
		return m, nil
	case cameraPairOpMsg:
		m.busy = false
		if msg.err != nil {
			m.message = "Camera request failed: " + userFacingGRPCError(msg.err)
			return m, nil
		}
		devices := make([]*agentpb.VideoDevice, 0, len(m.devices))
		for _, d := range m.devices {
			if d.Id == msg.id {
				if msg.forgot {
					continue
				}
				d.HasCredentials = true
			}
			devices = append(devices, d)
		}
		m.devices = devices
		m.message = fmt.Sprintf("Saved login for camera %d.", msg.id)
		if msg.forgot {
			m.message = fmt.Sprintf("Forgot camera %d and its login.", msg.id)
		}
		m.refreshRows(msg.id)
		return m, nil
	case tea.KeyMsg:
		if m.editing {
			return m.updateCredentials(msg)
		}
		switch msg.String() {
		case "r":
			if m.busy || m.scanning {
				return m, nil
			}
			m.scanning, m.message = true, ""
			return m, m.Init()
		case "enter", "f":
			if m.busy || m.scanning || m.handler == nil {
				return m, nil
			}
			d := m.selected()
			if d == nil {
				return m, nil
			}
			if msg.String() == "f" {
				m.busy, m.message = true, "Forgetting camera..."
				return m, m.handler.forget(d.Id)
			}
			m.editing, m.editingID, m.passwordFocused = true, d.Id, false
			m.message = ""
			m.username.SetValue(defaultCameraUser)
			m.password.SetValue("")
			m.password.Blur()
			return m, m.username.Focus()
		}
	}
	if m.editing {
		var cmd tea.Cmd
		if m.passwordFocused {
			m.password, cmd = m.password.Update(msg)
		} else {
			m.username, cmd = m.username.Update(msg)
		}
		return m, cmd
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m cameraPairModel) updateCredentials(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		m.password.SetValue("")
		m.username.Blur()
		m.password.Blur()
		return m, nil
	case "tab", "shift+tab", "up", "down":
		m.passwordFocused = !m.passwordFocused
		if m.passwordFocused {
			m.username.Blur()
			return m, m.password.Focus()
		}
		m.password.Blur()
		return m, m.username.Focus()
	case "enter":
		if strings.TrimSpace(m.username.Value()) == "" {
			m.message = "Enter a username."
			return m, nil
		}
		if !m.passwordFocused {
			m.passwordFocused = true
			m.username.Blur()
			return m, m.password.Focus()
		}
		cmd := m.handler.pair(m.editingID, strings.TrimSpace(m.username.Value()), m.password.Value())
		m.password.SetValue("")
		m.password.Blur()
		m.editing, m.busy, m.message = false, true, "Saving camera login..."
		return m, cmd
	}
	var cmd tea.Cmd
	if m.passwordFocused {
		m.password, cmd = m.password.Update(msg)
	} else {
		m.username, cmd = m.username.Update(msg)
	}
	return m, cmd
}

func (m cameraPairModel) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("IP Cameras")
	if m.scanning {
		title += "  Discovering..."
	}
	body := title + "\n\n"
	if m.editing {
		body += fmt.Sprintf("Login for camera %d\n", m.editingID) + m.username.View() + "\n" + m.password.View() + "\n\n"
	} else if len(m.devices) == 0 && !m.scanning {
		body += "No IP cameras found on the device's network. Press r to rescan.\n"
	} else {
		body += m.table.View() + "\n"
	}
	if m.message != "" {
		body += tui.StripControl(m.message) + "\n"
	}
	hint := "↑/↓ move · enter pair / edit login · f forget · r rescan · q quit"
	if m.editing {
		hint = "tab next field · enter continue / save · esc cancel"
	}
	body += devicePickerTabInactive.Render(hint) + "\n"
	if m.width > 0 {
		body = tui.CropANSIView(body, 0, m.width)
	}
	return body
}
