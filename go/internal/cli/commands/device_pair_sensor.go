package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type sensorPairScanMsg struct {
	devices      []models.DiscoveredDevice
	pairings     []*agentpbv2.SensorPairing
	discoveryErr error
	pairingsErr  error
}

type sensorPairOpMsg struct {
	assetID int32
	pairing *agentpbv2.SensorPairing // nil after forgetting
	err     error
}

type sensorPairHandler struct {
	ctx      context.Context
	client   agentpbv2.WendySensorPairingServiceClient
	discover func(context.Context) ([]models.DiscoveredDevice, error)
	orgIDs   func() (map[int32]bool, error)
	name     string
	sensors  []string
}

func (h *sensorPairHandler) scan() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		resp, err := h.client.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
		msg := sensorPairScanMsg{}
		if err != nil {
			msg.pairingsErr = cleanRPCError(err)
		} else {
			msg.pairings = resp.GetPairings()
		}
		msg.devices, msg.discoveryErr = h.discover(ctx)
		if msg.discoveryErr == nil {
			msg.discoveryErr = ctx.Err()
		}
		return msg
	}
}

func (h *sensorPairHandler) pair(source models.DiscoveredDevice) tea.Cmd {
	return func() tea.Msg {
		msg := sensorPairOpMsg{assetID: source.AssetID}
		orgs, err := h.orgIDs()
		if err == nil {
			err = orgAllowed(orgs, source.OrgID)
		}
		if err != nil {
			msg.err = err
			return msg
		}
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()
		// The consumer resolves the stable asset ID on its own network. The
		// CLI's discovered IP may be unreachable from that device.
		resp, err := h.client.AddSensorPairing(ctx, &agentpbv2.AddSensorPairingRequest{
			SourceAssetId: source.AssetID, Name: pairingName(h.name, &source),
			SensorAllowlist: h.sensors, Transport: transportForDevice(source),
		})
		if err != nil {
			msg.err = cleanRPCError(err)
		} else {
			msg.pairing = resp.GetPairing()
			if msg.pairing == nil {
				msg.pairing = &agentpbv2.SensorPairing{
					SourceAssetId: source.AssetID, OrgId: source.OrgID,
					Name: pairingName(h.name, &source), SensorAllowlist: h.sensors,
					Transport: transportForDevice(source),
				}
			}
		}
		return msg
	}
}

func (h *sensorPairHandler) forget(assetID int32) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()
		_, err := h.client.RemoveSensorPairing(ctx, &agentpbv2.RemoveSensorPairingRequest{SourceAssetId: assetID})
		if err != nil {
			err = cleanRPCError(err)
		}
		return sensorPairOpMsg{assetID: assetID, err: err}
	}
}

type sensorPairRow struct {
	assetID int32
	name    string
	source  *models.DiscoveredDevice
	pairing *agentpbv2.SensorPairing
}

// Merge by asset identity, retaining saved pairings even when their sources
// are offline. Those rows must remain available to the forget action.
func sensorPairRows(devices []models.DiscoveredDevice, pairings []*agentpbv2.SensorPairing) []sensorPairRow {
	byID := make(map[int32]sensorPairRow)
	for i := range devices {
		d := &devices[i]
		if d.Sensorlink && d.AssetID > 0 {
			byID[d.AssetID] = sensorPairRow{assetID: d.AssetID, name: d.DisplayName, source: d}
		}
	}
	for _, p := range pairings {
		if p == nil {
			continue
		}
		row := byID[p.SourceAssetId]
		row.assetID, row.pairing = p.SourceAssetId, p
		if p.Name != "" {
			row.name = p.Name
		}
		byID[row.assetID] = row
	}
	rows := make([]sensorPairRow, 0, len(byID))
	for _, row := range byID {
		if row.name == "" {
			row.name = fmt.Sprintf("Asset %d", row.assetID)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].pairing != nil) != (rows[j].pairing != nil) {
			return rows[i].pairing != nil
		}
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].assetID < rows[j].assetID
	})
	return rows
}

type sensorPairModel struct {
	handler       *sensorPairHandler
	table         tui.BubbleTable
	devices       []models.DiscoveredDevice
	pairings      []*agentpbv2.SensorPairing
	rows          []sensorPairRow
	scanning      bool
	busy          bool
	pairingsKnown bool
	message       string
	width, height int
}

func newSensorPairModel(h *sensorPairHandler) sensorPairModel {
	return sensorPairModel{
		handler: h, scanning: true,
		table: tui.NewBubbleTable(true, []bubbleTable.Column{
			{Title: "Name", Width: 28}, {Title: "Asset", Width: 10},
			{Title: "Address", Width: 20}, {Title: "Paired", Width: 8},
			{Title: "Connected", Width: 10},
		}),
	}
}

func (m sensorPairModel) Init() tea.Cmd { return m.handler.scan() }

func (m *sensorPairModel) refreshRows() {
	selectedID := int32(0)
	if row, ok := m.selected(); ok {
		selectedID = row.assetID
	}
	m.rows = sensorPairRows(m.devices, m.pairings)
	rows := make([]bubbleTable.Row, 0, len(m.rows))
	cursor := 0
	for i, row := range m.rows {
		address, paired, connected := "", "", ""
		if row.source != nil {
			address = row.source.IPAddress
		}
		if row.pairing != nil {
			paired, connected = "yes", "no"
			if row.pairing.Connected {
				connected = "yes"
			}
		}
		rows = append(rows, bubbleTable.Row{tui.StripControl(row.name), fmt.Sprint(row.assetID), tui.StripControl(address), paired, connected})
		if row.assetID == selectedID {
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

func (m sensorPairModel) selected() (sensorPairRow, bool) {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return sensorPairRow{}, false
	}
	return m.rows[idx], true
}

func (m sensorPairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table, _ = m.table.Update(msg)
		m.refreshRows()
		return m, nil
	case sensorPairScanMsg:
		m.scanning = false
		var errors []string
		if msg.discoveryErr == nil {
			m.devices = msg.devices
		} else {
			errors = append(errors, "Discovery failed: "+msg.discoveryErr.Error())
		}
		m.pairingsKnown = msg.pairingsErr == nil
		if m.pairingsKnown {
			m.pairings = msg.pairings
		} else {
			errors = append(errors, "Could not load pairings: "+msg.pairingsErr.Error())
		}
		m.message = strings.Join(errors, "\n")
		m.refreshRows()
		return m, nil
	case sensorPairOpMsg:
		m.busy = false
		if msg.err != nil {
			m.message = msg.err.Error()
			return m, nil
		}
		pairings := make([]*agentpbv2.SensorPairing, 0, len(m.pairings)+1)
		for _, p := range m.pairings {
			if p.SourceAssetId != msg.assetID {
				pairings = append(pairings, p)
			}
		}
		m.message = fmt.Sprintf("Forgot asset %d.", msg.assetID)
		if msg.pairing != nil {
			pairings = append(pairings, msg.pairing)
			m.message = fmt.Sprintf("Paired %s. Its sensors will appear on this device.", msg.pairing.Name)
		}
		m.pairings = pairings
		m.refreshRows()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			if m.scanning || m.busy {
				return m, nil
			}
			m.scanning, m.message = true, ""
			return m, m.handler.scan()
		case "enter", "f":
			if m.busy || m.scanning {
				return m, nil
			}
			row, ok := m.selected()
			if !ok {
				return m, nil
			}
			if msg.String() == "f" {
				if row.pairing == nil {
					m.message = "Only paired devices can be forgotten."
					return m, nil
				}
				m.busy, m.message = true, "Forgetting "+row.name+"..."
				return m, m.handler.forget(row.assetID)
			}
			if row.pairing != nil {
				m.message = "Already paired with " + row.name + "."
				return m, nil
			}
			if !m.pairingsKnown {
				m.message = "Rescan to load current pairings before pairing a device."
				return m, nil
			}
			if row.source == nil {
				return m, nil
			}
			m.busy, m.message = true, "Pairing "+row.name+"..."
			return m, m.handler.pair(*row.source)
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m sensorPairModel) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("SensorLink devices")
	if m.scanning {
		title += "  Scanning..."
	}
	body := title + "\n\n"
	if len(m.rows) == 0 && !m.scanning {
		body += "No SensorLink devices found. Press r to rescan.\n"
	} else {
		body += m.table.View() + "\n"
	}
	if m.message != "" {
		body += tui.StripControl(m.message) + "\n"
	}
	hint := "↑/↓ move · enter pair · f forget · r rescan · q quit"
	if m.table.CanScroll() {
		hint = "↑/↓ move · ←/→ scroll · enter pair · f forget · r rescan · q quit"
	}
	body += devicePickerTabInactive.Render(hint) + "\n"
	if m.width > 0 {
		body = tui.CropANSIView(body, 0, m.width)
	}
	return body
}
