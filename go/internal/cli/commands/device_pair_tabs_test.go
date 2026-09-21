package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui/bttable"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pairTestClient struct {
	agentpbv2.WendySensorPairingServiceClient
	pairings []*agentpbv2.SensorPairing
	added    *agentpbv2.AddSensorPairingRequest
	removed  int32
	err      error
}

func (c *pairTestClient) ListSensorPairings(context.Context, *agentpbv2.ListSensorPairingsRequest, ...grpc.CallOption) (*agentpbv2.ListSensorPairingsResponse, error) {
	return &agentpbv2.ListSensorPairingsResponse{Pairings: c.pairings}, c.err
}

func (c *pairTestClient) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest, _ ...grpc.CallOption) (*agentpbv2.AddSensorPairingResponse, error) {
	c.added = req
	return &agentpbv2.AddSensorPairingResponse{Pairing: &agentpbv2.SensorPairing{SourceAssetId: req.SourceAssetId, Name: req.Name}}, c.err
}

func (c *pairTestClient) RemoveSensorPairing(_ context.Context, req *agentpbv2.RemoveSensorPairingRequest, _ ...grpc.CallOption) (*agentpbv2.RemoveSensorPairingResponse, error) {
	c.removed = req.SourceAssetId
	return &agentpbv2.RemoveSensorPairingResponse{}, c.err
}

type pairTestBluetooth struct {
	scans  int
	paired string
	forgot string
}

func (h *pairTestBluetooth) StartScan() tea.Cmd {
	return func() tea.Msg {
		h.scans++
		return bttable.ScanResultMsg{Peripherals: []bttable.Peripheral{{Name: "Headphones", Address: "AA:BB"}}}
	}
}
func (h *pairTestBluetooth) NextScanEvent() tea.Cmd {
	return func() tea.Msg { return bttable.ScanDoneMsg{} }
}
func (h *pairTestBluetooth) Connect(address string) tea.Cmd {
	return func() tea.Msg {
		h.paired = address
		return bttable.OpResultMsg{Action: bttable.ActionConnect, Address: address, PairedKnown: true, Paired: true}
	}
}
func (h *pairTestBluetooth) Disconnect(address string) tea.Cmd {
	return func() tea.Msg { return bttable.OpResultMsg{Action: bttable.ActionDisconnect, Address: address} }
}
func (h *pairTestBluetooth) Forget(address string) tea.Cmd {
	return func() tea.Msg {
		h.forgot = address
		return bttable.OpResultMsg{Action: bttable.ActionForget, Address: address}
	}
}

func pairTestHandler(client *pairTestClient) *sensorPairHandler {
	return &sensorPairHandler{
		ctx: context.Background(), client: client,
		discover: func(context.Context) ([]models.DiscoveredDevice, error) {
			return []models.DiscoveredDevice{{AssetID: 42, OrgID: 7, DisplayName: "Camera", Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, IPAddress: "192.0.2.42"}}, nil
		},
		orgIDs: func() (map[int32]bool, error) { return map[int32]bool{7: true}, nil },
	}
}

func pairUpdate(m devicePairModel, msg tea.Msg) (devicePairModel, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(devicePairModel), cmd
}

func TestDevicePairTabsDiscoverOnVisitAndRouteBackgroundResults(t *testing.T) {
	bt := &pairTestBluetooth{}
	discoveries := 0
	h := pairTestHandler(&pairTestClient{})
	discover := h.discover
	h.discover = func(ctx context.Context) ([]models.DiscoveredDevice, error) {
		discoveries++
		return discover(ctx)
	}
	m := newDevicePairModel(bt, h, nil)
	batch := m.Init()().(tea.BatchMsg)
	if discoveries != 0 {
		t.Fatal("SensorLink scanned before its tab was opened")
	}
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.active != pairSensorLinkTab || scan == nil {
		t.Fatal("Tab did not start SensorLink discovery")
	}
	m, _ = pairUpdate(m, scan())
	if discoveries != 1 || !strings.Contains(m.View(), "Camera") {
		t.Fatal("SensorLink discovery was not rendered")
	}
	// Complete Bluetooth's initial scan while SensorLink is visible.
	m, next := pairUpdate(m, batch[0]())
	if !strings.Contains(m.View(), "Camera") || strings.Contains(m.View(), "Headphones") {
		t.Fatal("Bluetooth result changed the visible SensorLink tab")
	}
	m, _ = pairUpdate(m, next())
	if !strings.Contains(m.bluetooth.View(), "Headphones") {
		t.Fatal("inactive Bluetooth tab lost its scan result")
	}
	m, rescan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.active != pairBluetoothTab || rescan == nil {
		t.Fatal("Shift+Tab did not return to Bluetooth and rescan")
	}
	if view := ansi.Strip(m.View()); !strings.HasPrefix(view, "Bluetooth | SensorLink") {
		t.Fatalf("unexpected tab header: %s", view)
	}
	// Re-entering a tab refreshes discovery; an in-flight scan is not duplicated.
	m, rescan = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if rescan == nil {
		t.Fatal("returning to SensorLink did not refresh discovery")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	m, duplicate := pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if duplicate != nil {
		t.Fatal("tab switching duplicated an in-flight scan")
	}
	m, _ = pairUpdate(m, rescan())
	if discoveries != 2 {
		t.Fatalf("discovered %d times, want 2", discoveries)
	}
}

func TestDevicePairBluetoothActionsStayInTheirTab(t *testing.T) {
	bt := &pairTestBluetooth{}
	m := newDevicePairModel(bt, pairTestHandler(&pairTestClient{}), nil)
	batch := m.Init()().(tea.BatchMsg)
	m, next := pairUpdate(m, batch[0]())
	m, _ = pairUpdate(m, next())
	m, pair := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = pairUpdate(m, pair())
	if bt.paired != "AA:BB" || len(m.sensor.pairings) != 0 {
		t.Fatal("Bluetooth pairing was sent to the wrong tab")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	m, forget := pairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if forget == nil {
		t.Fatal("paired Bluetooth device cannot be forgotten")
	}
	m, _ = pairUpdate(m, forget())
	if bt.forgot != "AA:BB" || m.done {
		t.Fatal("forget failed or closed the pairing view")
	}
}

func TestDevicePairSensorActionsAndOfflineForget(t *testing.T) {
	client := &pairTestClient{pairings: []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Offline sensor"}}}
	h := pairTestHandler(client)
	h.name, h.sensors = "Front camera", []string{"video"}
	m := newDevicePairModel(&pairTestBluetooth{}, h, nil)
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = pairUpdate(m, scan())
	if len(m.sensor.rows) != 2 || m.sensor.rows[0].assetID != 99 {
		t.Fatal("saved offline pairing missing from discovery")
	}
	m, forget := pairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m, _ = pairUpdate(m, forget())
	if client.removed != 99 || len(m.sensor.rows) != 1 {
		t.Fatal("offline pairing was not forgotten")
	}
	m, pair := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if pair == nil {
		t.Fatal("nearby source cannot be paired")
	}
	m, duplicate := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if duplicate != nil {
		t.Fatal("duplicate pair operation while busy")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = pairUpdate(m, pair())
	if client.added == nil || client.added.SourceAssetId != 42 || client.added.SourceAddress != "" || client.added.Transport != "grpc" || client.added.Name != "Front camera" || strings.Join(client.added.SensorAllowlist, ",") != "video" {
		t.Fatalf("pair request lost identity or options: %+v", client.added)
	}
	if len(m.sensor.pairings) != 1 || m.sensor.pairings[0].SourceAssetId != 42 || m.done {
		t.Fatal("background pairing failed to update SensorLink state")
	}
}

func TestSensorPairRowsMergeByIdentity(t *testing.T) {
	rows := sensorPairRows([]models.DiscoveredDevice{
		{AssetID: 42, DisplayName: "First address", Sensorlink: true},
		{AssetID: 42, DisplayName: "Second address", Sensorlink: true},
		{AssetID: 43, DisplayName: "Not a sensor"},
		{DisplayName: "Unenrolled", Sensorlink: true},
	}, []*agentpbv2.SensorPairing{{SourceAssetId: 42, Name: "Saved camera", Connected: true}, {SourceAssetId: 99}})
	if len(rows) != 2 {
		t.Fatalf("expected one discovered pairing and one offline pairing: %+v", rows)
	}
	for _, row := range rows {
		if row.assetID == 42 && (row.name != "Saved camera" || row.source == nil || row.pairing == nil) {
			t.Fatal("discovered source did not merge with its saved pairing")
		}
	}
}

func TestSensorPairErrorsPreservePairingsAndBlockUnauthorizedPair(t *testing.T) {
	client := &pairTestClient{pairings: []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Saved"}}}
	h := pairTestHandler(client)
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	client.err = status.Error(codes.Unavailable, "internal details")
	h.discover = func(context.Context) ([]models.DiscoveredDevice, error) { return nil, errors.New("LAN unavailable") }
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if len(m.rows) != 2 || !strings.Contains(m.View(), "LAN unavailable") || !strings.Contains(m.View(), "not reachable") || strings.Contains(m.View(), "rpc error") {
		t.Fatalf("failed scan lost rows or error context: %s", m.View())
	}
	// A failed forget leaves the saved pairing available for retry.
	updated, forget := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = updated.(sensorPairModel)
	updated, _ = m.Update(forget())
	m = updated.(sensorPairModel)
	if len(m.pairings) != 1 || m.busy {
		t.Fatal("failed forget removed a pairing or left the UI busy")
	}
	m.table.SetCursor(1)
	_, pair := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if pair != nil {
		t.Fatal("pairing allowed when saved pairing state is unknown")
	}
	client.err = nil
	h.orgIDs = func() (map[int32]bool, error) { return map[int32]bool{8: true}, nil }
	result := h.pair(models.DiscoveredDevice{AssetID: 42, OrgID: 7})().(sensorPairOpMsg)
	if result.err == nil || client.added != nil {
		t.Fatal("cross-organization pairing reached the agent")
	}
}

func TestDevicePairTabFailureAndResizeDoNotBlockOtherTab(t *testing.T) {
	m := newDevicePairModel(&pairTestBluetooth{}, pairTestHandler(&pairTestClient{}), nil)
	m, _ = pairUpdate(m, pairTabMsg{tab: pairBluetoothTab, msg: bttable.ScanDoneMsg{Err: errors.New("Bluetooth unavailable")}})
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = pairUpdate(m, scan())
	m, _ = pairUpdate(m, tea.WindowSizeMsg{Width: 38, Height: 14})
	if !strings.Contains(m.View(), "Camera") {
		t.Fatal("Bluetooth failure prevented SensorLink discovery")
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if ansi.StringWidth(line) > 38 {
			t.Fatalf("view exceeds terminal width: %q", line)
		}
	}
	m, quit := pairUpdate(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil || m.View() != "" {
		t.Fatal("cannot quit the tabbed pairing view")
	}
}

func TestDevicePairStartsOnSensorLinkAndHandlesEmptyScan(t *testing.T) {
	bt := &pairTestBluetooth{}
	h := pairTestHandler(&pairTestClient{})
	h.discover = func(context.Context) ([]models.DiscoveredDevice, error) { return nil, nil }
	m := newDevicePairModel(bt, h, nil)
	m.active = pairSensorLinkTab
	m, _ = pairUpdate(m, m.Init()())
	if bt.scans != 0 || !strings.Contains(m.View(), "No SensorLink devices found") {
		t.Fatal("initial SensorLink tab started Bluetooth or failed to show the empty state")
	}
	m, pair := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if pair != nil || m.done {
		t.Fatal("empty discovery should keep the view open without dispatching an action")
	}
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if scan == nil {
		t.Fatal("first Bluetooth visit did not start discovery")
	}
	batch := scan().(tea.BatchMsg)
	m, next := pairUpdate(m, batch[0]())
	m, _ = pairUpdate(m, next())
	if bt.scans != 1 || !strings.Contains(m.View(), "Headphones") {
		t.Fatal("first Bluetooth visit failed after starting on SensorLink")
	}
}

func TestSensorPairScanCancellationKeepsKnownDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := pairTestHandler(&pairTestClient{})
	h.ctx = ctx
	// Discovery can return an empty collection when its context expires.
	h.discover = func(ctx context.Context) ([]models.DiscoveredDevice, error) {
		if ctx.Err() == nil {
			t.Fatal("discovery did not receive the cancelled context")
		}
		return nil, nil
	}
	m := newSensorPairModel(h)
	m.devices = []models.DiscoveredDevice{{AssetID: 42, Sensorlink: true}}
	cancel()
	updated, _ := m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if len(m.devices) != 1 || m.scanning || !strings.Contains(m.message, "context canceled") {
		t.Fatal("cancelled scan discarded known devices or stayed scanning")
	}
}

func TestPairingViewsStripRemoteControls(t *testing.T) {
	bad := "Device\x1b[2J\r\u202e"
	sensor := newSensorPairModel(nil)
	sensor.pairings = []*agentpbv2.SensorPairing{{SourceAssetId: 1, Name: bad}}
	sensor.message = "Paired " + bad
	sensor.refreshRows()
	camera := newCameraPairModel(nil)
	camera.devices = []*agentpb.VideoDevice{{Id: 1, Name: bad, Address: bad}}
	camera.message = bad
	camera.refreshRows(1)
	for _, view := range []string{sensor.View(), camera.View()} {
		if strings.Contains(view, "\x1b[2J") || strings.ContainsAny(ansi.Strip(view), "\r\u202e") {
			t.Fatalf("remote controls reached view: %q", view)
		}
	}
}
