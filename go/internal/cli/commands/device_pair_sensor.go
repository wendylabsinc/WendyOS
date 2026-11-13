package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sensorPairScanMsg struct {
	devices []models.DiscoveredDevice
	// localNetwork means devices came from this computer's network because the
	// device's agent cannot scan its own.
	localNetwork     bool
	pairings         []*agentpbv2.SensorPairing
	discoveryErr     error
	pairingsErr      error
	principalPairing bool
}

type sensorPairOpMsg struct {
	assetID   int32
	principal string
	pairing   *agentpbv2.SensorPairing // nil after forgetting
	err       error
}

type sensorPairHandler struct {
	ctx    context.Context
	client agentpbv2.WendySensorPairingServiceClient
	// discoverLocal scans this computer's network. It runs only when the
	// device's agent predates DiscoverSensorSources.
	discoverLocal func(context.Context) ([]models.DiscoveredDevice, error)
	orgIDs        func() (map[int32]bool, error)
	tenantIDs     func() (map[string]bool, error)
	name          string
	sensors       []string
}

func (h *sensorPairHandler) scan() tea.Cmd {
	return func() tea.Msg { return h.load(h.ctx) }
}

// load reads the device's saved pairings and the sensor sources on its
// network. The SensorLink tab and the scripted operations both start here.
func (h *sensorPairHandler) load(ctx context.Context) sensorPairScanMsg {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := h.client.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
	msg := sensorPairScanMsg{}
	if err != nil {
		msg.pairingsErr = cleanRPCError(err)
	} else {
		msg.pairings = resp.GetPairings()
		msg.principalPairing = resp.GetSupportsPrincipalPairing()
	}
	msg.devices, msg.localNetwork, msg.discoveryErr = h.discoverSources(ctx)
	if msg.discoveryErr == nil {
		msg.discoveryErr = ctx.Err()
	}
	return msg
}

// errSensorDiscoveryUnsupported is discoverSources' answer for an agent that
// predates DiscoverSensorSources when no local fallback is configured, as for
// scripted pairing.
var errSensorDiscoveryUnsupported = errors.New("this device's Wendy Agent cannot scan its network for sensor sources; update it")

// discoverSources asks the device which sensor sources it sees on its own
// network, since that is where its agent looks for the source after pairing.
// Only an agent that predates DiscoverSensorSources (UNIMPLEMENTED) falls back
// to this computer's network, and localNetwork reports that. Any other failure
// is returned rather than replaced by a local scan, whose sources the device
// may be unable to reach.
func (h *sensorPairHandler) discoverSources(ctx context.Context) (devices []models.DiscoveredDevice, localNetwork bool, err error) {
	resp, err := h.client.DiscoverSensorSources(ctx, &agentpbv2.DiscoverSensorSourcesRequest{})
	if status.Code(err) == codes.Unimplemented {
		if h.discoverLocal == nil {
			return nil, false, errSensorDiscoveryUnsupported
		}
		devices, err = h.discoverLocal(ctx)
		return devices, true, err
	}
	if err != nil {
		return nil, false, sensorDiscoveryError(ctx, err)
	}
	return sensorSourcesFromDevice(resp.GetSources()), false, nil
}

// sensorDiscoveryError maps a DiscoverSensorSources call that failed under ctx
// to text for the SensorLink tab, which prefixes it with "Discovery failed: ".
func sensorDiscoveryError(ctx context.Context, err error) error {
	switch status.Code(err) {
	case codes.Unavailable, codes.PermissionDenied:
		return cleanRPCError(err)
	case codes.DeadlineExceeded:
		if ctx.Err() != nil {
			// The tab's own deadline, which ListSensorPairings spends from
			// too, ran out: the call may never have reached the device.
			return errors.New("the device did not answer in time")
		}
		return errors.New("the device did not finish scanning its network in time")
	}
	return fmt.Errorf("the device could not scan its network: %s", userFacingGRPCError(err))
}

func (h *sensorPairHandler) pair(source models.DiscoveredDevice) tea.Cmd {
	return func() tea.Msg {
		pairing, err := h.add(h.ctx, source)
		return sensorPairOpMsg{assetID: source.AssetID, principal: sensorSourcePrincipal(source), pairing: pairing, err: err}
	}
}

// add pairs the device with source once the source's organization or tenant
// is one the CLI is signed in to.
func (h *sensorPairHandler) add(ctx context.Context, source models.DiscoveredDevice) (*agentpbv2.SensorPairing, error) {
	principal := sensorSourcePrincipal(source)
	var err error
	if principal != "" {
		if h.tenantIDs == nil {
			err = fmt.Errorf("sign in to the sensor's organization before pairing")
		} else {
			var tenants map[string]bool
			tenants, err = h.tenantIDs()
			if err == nil && !tenants[source.TenantUUID] {
				err = fmt.Errorf("sign in to the sensor's organization before pairing")
			}
		}
	} else {
		var orgs map[int32]bool
		orgs, err = h.orgIDs()
		if err == nil {
			err = orgAllowed(orgs, source.OrgID)
		}
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Send no address, even one the device itself discovered: the consumer
	// resolves the source by identity on its own network and re-resolves
	// on every reconnect, while a pinned address would be reused even
	// after the source moves.
	resp, err := h.client.AddSensorPairing(ctx, &agentpbv2.AddSensorPairingRequest{
		SourceAssetId: source.AssetID, SourcePrincipalUri: principal, Name: pairingName(h.name, &source),
		SensorAllowlist: h.sensors, Transport: transportForDevice(source),
	})
	if err != nil {
		return nil, cleanRPCError(err)
	}
	if pairing := resp.GetPairing(); pairing != nil {
		return pairing, nil
	}
	return &agentpbv2.SensorPairing{
		SourceAssetId: source.AssetID, SourcePrincipalUri: principal, OrgId: source.OrgID,
		Name: pairingName(h.name, &source), SensorAllowlist: h.sensors,
		Transport: transportForDevice(source),
	}, nil
}

func (h *sensorPairHandler) forget(row sensorPairRow) tea.Cmd {
	return func() tea.Msg {
		err := h.remove(h.ctx, row.assetID, row.principal)
		return sensorPairOpMsg{assetID: row.assetID, principal: row.principal, err: err}
	}
}

func (h *sensorPairHandler) remove(ctx context.Context, assetID int32, principal string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := h.client.RemoveSensorPairing(ctx, &agentpbv2.RemoveSensorPairingRequest{SourceAssetId: assetID, SourcePrincipalUri: principal}); err != nil {
		return cleanRPCError(err)
	}
	return nil
}

type sensorPairRow struct {
	assetID   int32
	principal string
	name      string
	source    *models.DiscoveredDevice
	pairing   *agentpbv2.SensorPairing
}

// Merge by asset identity, retaining saved pairings even when their sources
// are offline. Those rows must remain available to the forget action.
func sensorPairRows(devices []models.DiscoveredDevice, pairings []*agentpbv2.SensorPairing) []sensorPairRow {
	byID := make(map[string]sensorPairRow)
	for i := range devices {
		d := &devices[i]
		if d.Sensorlink && sensorSourcePairable(*d) {
			principal := sensorSourcePrincipal(*d)
			byID[sensorPairKey(d.AssetID, principal)] = sensorPairRow{assetID: d.AssetID, principal: principal, name: d.DisplayName, source: d}
		}
	}
	for _, p := range pairings {
		if p == nil {
			continue
		}
		key := sensorPairKey(p.SourceAssetId, p.SourcePrincipalUri)
		row := byID[key]
		row.assetID, row.principal, row.pairing = p.SourceAssetId, p.SourcePrincipalUri, p
		if p.Name != "" {
			row.name = p.Name
		}
		byID[key] = row
	}
	rows := make([]sensorPairRow, 0, len(byID))
	for _, row := range byID {
		if row.name == "" {
			row.name = sensorPairLabel(row.assetID, row.principal)
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
		return sensorPairKey(rows[i].assetID, rows[i].principal) < sensorPairKey(rows[j].assetID, rows[j].principal)
	})
	return rows
}

type sensorPairModel struct {
	handler          *sensorPairHandler
	table            tui.BubbleTable
	devices          []models.DiscoveredDevice
	pairings         []*agentpbv2.SensorPairing
	rows             []sensorPairRow
	scanning         bool
	busy             bool
	pairingsKnown    bool
	principalPairing bool
	// localNetwork: devices came from this computer's network, not the
	// device's, because the device's agent cannot scan its own.
	localNetwork  bool
	message       string
	width, height int
}

func newSensorPairModel(h *sensorPairHandler) sensorPairModel {
	return sensorPairModel{
		handler: h, scanning: true,
		table: tui.NewBubbleTable(true, []bubbleTable.Column{
			{Title: "Name", Width: 28}, {Title: "Device", Width: 18},
			{Title: "Address", Width: 20}, {Title: "Paired", Width: 8},
			{Title: "Connected", Width: 10},
		}),
	}
}

func (m sensorPairModel) Init() tea.Cmd { return m.handler.scan() }

func (m *sensorPairModel) refreshRows() {
	selectedID := ""
	if row, ok := m.selected(); ok {
		selectedID = sensorPairKey(row.assetID, row.principal)
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
		rows = append(rows, bubbleTable.Row{tui.StripControl(row.name), tui.StripControl(sensorPairLabel(row.assetID, row.principal)), tui.StripControl(address), paired, connected})
		if sensorPairKey(row.assetID, row.principal) == selectedID {
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
			m.localNetwork = msg.localNetwork
		} else {
			errors = append(errors, "Discovery failed: "+msg.discoveryErr.Error())
		}
		m.pairingsKnown = msg.pairingsErr == nil
		m.principalPairing = msg.principalPairing
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
			if sensorPairKey(p.SourceAssetId, p.SourcePrincipalUri) != sensorPairKey(msg.assetID, msg.principal) {
				pairings = append(pairings, p)
			}
		}
		m.message = fmt.Sprintf("Forgot %s.", sensorPairLabel(msg.assetID, msg.principal))
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
				return m, m.handler.forget(row)
			}
			if refusal := sensorPairRefusal(row, m.pairingsKnown, m.principalPairing); refusal != "" {
				m.message = refusal
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
	var unenrolled, enrolledUnsupported []string
	for _, source := range m.devices {
		if !source.Sensorlink || sensorSourcePairable(source) {
			continue
		}
		label := sensorSourceLabel(source)
		if sensorSourcePKIUnsupported(source) {
			enrolledUnsupported = append(enrolledUnsupported, tui.StripControl(label))
		} else {
			unenrolled = append(unenrolled, tui.StripControl(label))
		}
	}
	sort.Strings(unenrolled)
	sort.Strings(enrolledUnsupported)
	body := title + "\n\n"
	where := "the device's network"
	if m.localNetwork {
		where = "this computer's network"
		body += "This device's Wendy Agent cannot scan its own network, so this list shows sources on this computer's network.\n" +
			"The device pairs only with sources it can reach. Update its agent to list those instead.\n\n"
	}
	if len(m.rows) == 0 && len(unenrolled) == 0 && len(enrolledUnsupported) == 0 && !m.scanning {
		body += "No SensorLink devices found on " + where + ". Press r to rescan.\n"
	} else if len(m.rows) > 0 || m.scanning {
		body += m.table.View() + "\n"
	}
	if len(unenrolled) > 0 {
		body += "Found sensors that need enrollment:\n"
		for _, label := range unenrolled {
			body += "  " + label + "\n"
		}
		body += "Enroll these sources in the same organization as the target device, then press r to rescan.\n"
	}
	if len(enrolledUnsupported) > 0 {
		body += "Enrolled sensors:\n"
		for _, label := range enrolledUnsupported {
			body += "  " + label + "\n"
		}
		body += "This version does not support pairing these devices yet.\n"
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

// sensorPairRefusal says why the SensorLink tab or a scripted pair will not
// pair row, or returns "" when it can. pairingsKnown is whether the device's
// saved pairings loaded, and principalPairing whether its agent can pair by
// principal.
func sensorPairRefusal(row sensorPairRow, pairingsKnown, principalPairing bool) string {
	switch {
	case row.pairing != nil:
		return "Already paired with " + row.name + "."
	case !pairingsKnown:
		return "Rescan to load current pairings before pairing a device."
	case row.principal != "" && !principalPairing:
		return "Update the target Wendy Agent to pair PKI-enrolled sensors."
	}
	return ""
}

// sensorSourcePairable reports whether a discovered source carries an
// identity to pair by: a legacy asset ID, or a PKI principal over mTLS.
// Other SensorLink sources need enrollment first.
func sensorSourcePairable(d models.DiscoveredDevice) bool {
	return d.AssetID > 0 || (d.IsMTLS && sensorSourcePrincipal(d) != "")
}

// sensorSourcePKIUnsupported reports whether a source without a pairable
// identity is a Wendy Lite board already enrolled in a PKI tenant, which this
// version cannot pair, rather than one needing enrollment.
func sensorSourcePKIUnsupported(d models.DiscoveredDevice) bool {
	return d.WendyLite && d.IsMTLS && d.TenantUUID != ""
}

// sensorSourceLabel names a discovered source by its display name and the
// address the device sees it at.
func sensorSourceLabel(d models.DiscoveredDevice) string {
	label := d.DisplayName
	if address := d.Address(); address != "" {
		if label == "" {
			label = address
		} else {
			label += " (" + address + ")"
		}
	}
	return label
}

func sensorSourcePrincipal(d models.DiscoveredDevice) string {
	if d.AssetID > 0 {
		return ""
	}
	return d.PrincipalURI
}
func sensorPairKey(assetID int32, principal string) string {
	if principal != "" {
		return principal
	}
	return fmt.Sprintf("asset:%d", assetID)
}
func sensorPairLabel(assetID int32, principal string) string {
	if principal != "" {
		if id, err := certs.ParsePrincipal(principal); err == nil {
			return id.EntityID
		}
		return principal
	}
	return fmt.Sprintf("Asset %d", assetID)
}
