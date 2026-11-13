package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func armRPCError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.Unimplemented {
		// An older Agent lacks the service; a Go Agent off Linux has no inventory.
		return fmt.Errorf("target Agent does not support USB arm bindings; they need an up-to-date Linux Agent or the native Mac Agent")
	}
	return fmt.Errorf("USB arm request failed: %s", userFacingGRPCError(err))
}

// Explicit USB flags have their own additive JSON contract; legacy --list
// continues to list SensorLink pairings. All calls use the selected Agent client.
func runArmPair(ctx context.Context, client pb.WendyRobotArmServiceClient, bind, forget, name string, allowLocation, jsonOut bool, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result any
	switch {
	case bind != "":
		resp, err := client.BindRobotArm(ctx, &pb.BindRobotArmRequest{Id: bind, Model: "so101", Name: name, AllowLocationBinding: allowLocation})
		if err != nil {
			return armRPCError(err)
		}
		result = resp
		if !jsonOut {
			_, err = fmt.Fprintln(out, "Saved SO-101 binding on target Agent. Arm unverified; no adapter started.")
			return err
		}
	case forget != "":
		_, err := client.ForgetRobotArm(ctx, &pb.ForgetRobotArmRequest{Id: forget})
		if err != nil {
			return armRPCError(err)
		}
		result = map[string]any{"forgotten": forget}
		if !jsonOut {
			_, err = fmt.Fprintln(out, "Forgot USB arm binding on target Agent.")
			return err
		}
	default:
		resp, err := client.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
		if err != nil {
			return armRPCError(err)
		}
		result = resp
		if !jsonOut {
			if len(resp.Devices) == 0 {
				_, err = fmt.Fprintln(out, "No USB serial endpoints or saved arm bindings found on target Agent.")
				return err
			}
			for _, d := range resp.Devices {
				fmt.Fprintf(out, "%s  %s  %s  %s\n", tui.StripControl(d.Id), tui.StripControl(d.DevicePath), armBindingState(d), tui.StripControl(d.ConnectionStatus))
			}
			return nil
		}
	}
	if message, ok := result.(proto.Message); ok {
		return writePairProtoJSON(out, message)
	}
	return writeJSON(out, result)
}

// pairJSON encodes pairing replies with snake_case fields and explicit
// defaults, so a list is [] when empty.
var pairJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// writePairProtoJSON writes a pairing reply as indented pairJSON. protojson's
// whitespace differs between builds, so it is re-indented.
func writePairProtoJSON(out io.Writer, message proto.Message) error {
	raw, err := pairJSON.Marshal(message)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err = json.Indent(&indented, raw, "", "  "); err != nil {
		return err
	}
	indented.WriteByte('\n')
	_, err = out.Write(indented.Bytes())
	return err
}

func armBindingState(d *pb.RobotArm) string {
	if d.Configured {
		return "configured / unverified"
	}
	if d.Candidate {
		return "adapter candidate / unverified"
	}
	return "unknown USB serial device"
}

type armPairHandler struct {
	ctx    context.Context
	client pb.WendyRobotArmServiceClient
	name   string
}
type armPairScanMsg struct {
	devices []*pb.RobotArm
	err     error
}
type armPairOpMsg struct {
	err    error
	forgot bool
}

func (h *armPairHandler) scan() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		resp, err := h.client.ListRobotArms(ctx, &pb.ListRobotArmsRequest{})
		return armPairScanMsg{devices: resp.GetDevices(), err: armRPCError(err)}
	}
}
func (h *armPairHandler) bind(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		_, err := h.client.BindRobotArm(ctx, &pb.BindRobotArmRequest{Id: id, Model: "so101", Name: h.name, AllowLocationBinding: true})
		return armPairOpMsg{err: armRPCError(err)}
	}
}
func (h *armPairHandler) forget(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		_, err := h.client.ForgetRobotArm(ctx, &pb.ForgetRobotArmRequest{Id: id})
		return armPairOpMsg{err: armRPCError(err), forgot: true}
	}
}

type armPairModel struct {
	handler                            *armPairHandler
	table                              tui.BubbleTable
	devices                            []*pb.RobotArm
	scanning, busy, confirming, scanOK bool
	message                            string
	width, height                      int
}

func newArmPairModel(h *armPairHandler) armPairModel {
	return armPairModel{handler: h, scanning: true, table: tui.NewBubbleTable(true, []bubbleTable.Column{
		{Title: "USB device", Width: 24}, {Title: "Current port", Width: 32}, {Title: "Binding", Width: 30}, {Title: "USB status", Width: 13},
	})}
}
func (m armPairModel) Init() tea.Cmd {
	if m.handler == nil {
		return func() tea.Msg { return armPairScanMsg{} }
	}
	return m.handler.scan()
}
func (m armPairModel) selected() *pb.RobotArm {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.devices) {
		return nil
	}
	return m.devices[i]
}
func (m *armPairModel) refreshRows(id string) {
	rows := make([]bubbleTable.Row, 0, len(m.devices))
	cursor := 0
	for i, d := range m.devices {
		label := d.Name
		if label == "" {
			label = d.Product
		}
		if label == "" {
			label = d.VendorId + ":" + d.ProductId
		}
		rows = append(rows, bubbleTable.Row{tui.StripControl(label), tui.StripControl(d.DevicePath), armBindingState(d), tui.StripControl(d.ConnectionStatus)})
		if d.Id == id {
			cursor = i
		}
	}
	m.table.SetRows(rows)
	m.table.SetCursor(cursor)
	height := max(4, len(rows)+2)
	if m.height > 0 {
		height = min(height, max(1, m.height-12))
	}
	m.table.SetHeight(height)
}
func (m armPairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table, _ = m.table.Update(msg)
		m.refreshRows(m.selected().GetId())
		return m, nil
	case armPairScanMsg:
		m.scanning = false
		m.scanOK = msg.err == nil
		if msg.err != nil {
			m.message = msg.err.Error()
			return m, nil
		}
		id := m.selected().GetId()
		m.devices = msg.devices
		m.refreshRows(id)
		return m, nil
	case armPairOpMsg:
		m.busy = false
		if msg.err != nil {
			m.message = msg.err.Error()
			m.scanOK = false
			return m, nil
		}
		m.message = "Saved binding on target Agent. Unverified; no adapter started."
		if msg.forgot {
			m.message = "Forgot USB arm binding on target Agent."
		}
		m.scanning = true
		m.scanOK = false
		return m, m.Init()
	case tea.KeyMsg:
		if m.confirming {
			switch msg.String() {
			case "y":
				m.confirming = false
				if d := m.selected(); d != nil {
					m.busy = true
					return m, m.handler.bind(d.Id)
				}
			case "n", "esc":
				m.confirming = false
				m.message = ""
			}
			return m, nil
		}
		switch msg.String() {
		case "r":
			if m.busy || m.scanning {
				return m, nil
			}
			m.scanning = true
			m.scanOK = false
			m.message = ""
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
				if !d.Configured {
					return m, nil
				}
				m.busy = true
				return m, m.handler.forget(d.Id)
			}
			if !m.scanOK || !d.Connected {
				return m, nil
			}
			m.confirming = true
			m.message = "Bind this USB adapter as SO-101? Attached arm is unverified. y save · n cancel"
			if d.IdentityKind == "usb_location" {
				m.message = "No serial ID: this binding follows the USB port, including replacements. Bind as SO-101? y acknowledge / save · n cancel"
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}
func (m armPairModel) View() string {
	body := "USB Arms — target Agent\n\n"
	if m.scanning {
		body += "Reading USB metadata...\n"
	} else if len(m.devices) == 0 {
		body += "No USB serial endpoints or saved arm bindings found.\n"
	} else {
		body += m.table.View() + "\n"
	}
	if d := m.selected(); d != nil {
		body += fmt.Sprintf("ID: %s\nUSB %s:%s · serial %s · interface %s · location %s\nManufacturer: %s · product: %s\n%s\n", tui.StripControl(d.Id), tui.StripControl(d.VendorId), tui.StripControl(d.ProductId), tui.StripControl(d.SerialNumber), tui.StripControl(d.UsbInterface), tui.StripControl(d.UsbLocation), tui.StripControl(d.Manufacturer), tui.StripControl(d.Product), tui.StripControl(d.Note))
	}
	if m.message != "" {
		body += tui.StripControl(m.message) + "\n"
	}
	body += "Pairing saves host-local configuration. Motor power / busy / access: unknown.\n↑/↓ move · enter bind as SO-101 · f forget · r rescan · q quit\n"

	if m.width > 0 {
		body = tui.CropANSIView(body, 0, m.width)
	}
	return body
}
