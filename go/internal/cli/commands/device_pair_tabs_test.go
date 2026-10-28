package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui/bttable"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pairTestClient struct {
	agentpbv2.WendySensorPairingServiceClient
	pairings          []*agentpbv2.SensorPairing
	added             *agentpbv2.AddSensorPairingRequest
	adds, removes     int
	removed           int32
	removedPrincipal  string
	supportsPrincipal bool
	err               error
	// sources and discoverErr answer DiscoverSensorSources, the scan of the
	// device's own network.
	sources     []*agentpbv2.DiscoveredSensorSource
	discoverErr error
	discoveries int
}

func (c *pairTestClient) DiscoverSensorSources(ctx context.Context, _ *agentpbv2.DiscoverSensorSourcesRequest, _ ...grpc.CallOption) (*agentpbv2.DiscoverSensorSourcesResponse, error) {
	c.discoveries++
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if c.discoverErr != nil {
		return nil, c.discoverErr
	}
	return &agentpbv2.DiscoverSensorSourcesResponse{Sources: c.sources}, nil
}

// deviceSource is the DiscoverSensorSources entry an agent reports for a LAN
// sighting on its network (see services.sensorSourceToProto).
func deviceSource(d models.LANDevice) *agentpbv2.DiscoveredSensorSource {
	return &agentpbv2.DiscoveredSensorSource{
		Id: d.ID, DisplayName: d.DisplayName, Hostname: d.Hostname, IpAddress: d.IPAddress,
		Addresses: d.Addresses, Port: int32(d.Port), IsMtls: d.IsMTLS, AssetId: d.AssetID,
		OrgId: d.OrgID, TenantUuid: d.TenantUUID, PrincipalUri: d.PrincipalURI, Caps: d.Caps,
		WendyLite: d.WendyLite,
	}
}

func (c *pairTestClient) ListSensorPairings(context.Context, *agentpbv2.ListSensorPairingsRequest, ...grpc.CallOption) (*agentpbv2.ListSensorPairingsResponse, error) {
	return &agentpbv2.ListSensorPairingsResponse{Pairings: c.pairings, SupportsPrincipalPairing: c.supportsPrincipal}, c.err
}

func (c *pairTestClient) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest, _ ...grpc.CallOption) (*agentpbv2.AddSensorPairingResponse, error) {
	c.added = req
	c.adds++
	return &agentpbv2.AddSensorPairingResponse{Pairing: &agentpbv2.SensorPairing{
		SourceAssetId: req.SourceAssetId, SourcePrincipalUri: req.SourcePrincipalUri, Name: req.Name,
		SensorAllowlist: req.SensorAllowlist, Transport: req.Transport,
	}}, c.err
}

func (c *pairTestClient) RemoveSensorPairing(_ context.Context, req *agentpbv2.RemoveSensorPairingRequest, _ ...grpc.CallOption) (*agentpbv2.RemoveSensorPairingResponse, error) {
	c.removed = req.SourceAssetId
	c.removedPrincipal = req.SourcePrincipalUri
	c.removes++
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

// pairTestHandler's device reports one pairable source on its network. It has
// no local fallback, so a test that reaches it without setting one fails.
func pairTestHandler(client *pairTestClient) *sensorPairHandler {
	client.sources = []*agentpbv2.DiscoveredSensorSource{{AssetId: 42, OrgId: 7, DisplayName: "Camera", IsMtls: true, Caps: []string{"sensors"}, IpAddress: "192.0.2.42"}}
	return &sensorPairHandler{
		ctx: context.Background(), client: client,
		orgIDs: func() (map[int32]bool, error) { return map[int32]bool{7: true}, nil },
	}
}

func pairUpdate(m devicePairModel, msg tea.Msg) (devicePairModel, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(devicePairModel), cmd
}

func TestDevicePairTabsDiscoverOnVisitAndRouteBackgroundResults(t *testing.T) {
	bt := &pairTestBluetooth{}
	client := &pairTestClient{}
	h := pairTestHandler(client)
	m := newDevicePairModel(bt, h, nil)
	batch := m.Init()().(tea.BatchMsg)
	if client.discoveries != 0 {
		t.Fatal("SensorLink scanned before its tab was opened")
	}
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.active != pairSensorLinkTab || scan == nil {
		t.Fatal("Tab did not start SensorLink discovery")
	}
	m, _ = pairUpdate(m, scan())
	if client.discoveries != 1 || !strings.Contains(m.View(), "Camera") {
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
	if client.discoveries != 2 {
		t.Fatalf("discovered %d times, want 2", client.discoveries)
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

func TestSensorPairShowsUnenrolledWendyLite(t *testing.T) {
	// Unenrolled firmware advertises its sensor capability but no asset ID.
	source := discovery.LANDeviceFromWendyLiteService(discovery.MDNSService{
		InstanceName: "wendy-4824", Hostname: "wendylt-wendy-4824.local",
		IPAddress: "192.0.2.24", Port: 5054,
		TXTRecords: map[string]string{"id": "441bf6804824", "caps": "sensors", "mtls": "false"},
	})
	client := &pairTestClient{}
	h := pairTestHandler(client)
	client.sources = []*agentpbv2.DiscoveredSensorSource{deviceSource(source)}
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	view := m.View()
	for _, want := range []string{"wendy-4824", "192.0.2.24", "need enrollment", "same organization"} {
		if !strings.Contains(view, want) {
			t.Fatalf("unenrolled sensor missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, "No SensorLink devices found") {
		t.Fatalf("discovered sensor reported as absent: %s", view)
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'f'}}} {
		_, cmd := m.Update(key)
		if cmd != nil {
			t.Fatal("unenrolled sensor must not dispatch pairing or forgetting")
		}
	}

	// Once enrollment completes, a rescan makes the same board pairable.
	client.sources[0].AssetId = 42
	client.sources[0].OrgId = 7
	client.sources[0].IsMtls = true
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if strings.Contains(m.View(), "need enrollment") || len(m.rows) != 1 {
		t.Fatalf("enrolled sensor did not become pairable: %s", m.View())
	}
	_, pair := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if pair == nil {
		t.Fatal("enrolled sensor has no pairing action")
	}
	pair()
	if client.added == nil || client.added.SourceAssetId != 42 || client.added.Transport != "wendycom" {
		t.Fatalf("wrong pairing after enrollment: %+v", client.added)
	}
}

func TestSensorPairPKIEnrollmentPairAndForget(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const device = "44444444-4444-4444-8444-444444444444"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	source := discovery.LANDeviceFromWendyLiteService(discovery.MDNSService{
		InstanceName: "joannis-home", IPAddress: "192.168.0.125", Port: 5054,
		TXTRecords: map[string]string{"id": device, "tenant": tenant, "principal": principal, "caps": "sensors", "mtls": "true"},
	})
	client := &pairTestClient{}
	h := pairTestHandler(client)
	client.sources = []*agentpbv2.DiscoveredSensorSource{deviceSource(source)}
	h.tenantIDs = func() (map[string]bool, error) { return map[string]bool{tenant: true}, nil }
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	if len(m.rows) != 1 || !strings.Contains(m.View(), "joannis-home") || strings.Contains(m.View(), "need enrollment") {
		t.Fatalf("wrong enrolled view: %s", m.View())
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(sensorPairModel)
	if cmd != nil || !strings.Contains(m.message, "Update the target") || client.added != nil {
		t.Fatal("old agent must not receive principal pairing as asset zero")
	}
	client.supportsPrincipal = true
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(sensorPairModel)
	if cmd == nil {
		t.Fatal("PKI sensor cannot pair")
	}
	updated, _ = m.Update(cmd())
	m = updated.(sensorPairModel)
	if client.added.SourceAssetId != 0 || client.added.SourcePrincipalUri != principal || client.added.SourceAddress != "" || client.added.Transport != "wendycom" {
		t.Fatalf("wrong request: %+v", client.added)
	}
	if len(m.pairings) != 1 || m.pairings[0].SourcePrincipalUri != principal {
		t.Fatal("principal pairing lost in UI")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if cmd == nil {
		t.Fatal("PKI sensor cannot forget")
	}
	cmd()
	if client.removed != 0 || client.removedPrincipal != principal {
		t.Fatal("forget lost principal")
	}
	h.tenantIDs = func() (map[string]bool, error) { return map[string]bool{}, nil }
	if msg := h.pair(sensorSourcesFromDevice(client.sources)[0])().(sensorPairOpMsg); msg.err == nil {
		t.Fatal("foreign tenant passed CLI gate")
	}
}

func TestSensorPairShowsUnenrolledAlongsideSavedPairings(t *testing.T) {
	m := newSensorPairModel(nil)
	m.scanning = false
	m.devices = []models.DiscoveredDevice{
		{DisplayName: "Sensor B", Sensorlink: true},
		{DisplayName: "Sensor A\x1b[2J\r\u202e", Sensorlink: true},
		{DisplayName: "Not a sensor"},
	}
	m.pairings = []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Saved camera"}}
	m.refreshRows()
	view := ansi.Strip(m.View())
	for _, want := range []string{"Sensor A", "Sensor B", "Saved camera"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, "Not a sensor") || strings.Contains(m.View(), "\x1b[2J") || strings.ContainsAny(view, "\r\u202e") {
		t.Fatalf("non-sensor or remote controls reached enrollment notice: %s", view)
	}
}

func TestSensorPairErrorsPreservePairingsAndBlockUnauthorizedPair(t *testing.T) {
	client := &pairTestClient{pairings: []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Saved"}}}
	h := pairTestHandler(client)
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	client.err = status.Error(codes.Unavailable, "internal details")
	client.discoverErr = status.Error(codes.Internal, "LAN unavailable")
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
	client := &pairTestClient{}
	h := pairTestHandler(client)
	client.sources = nil
	m := newDevicePairModel(bt, h, nil)
	m.active = pairSensorLinkTab
	m, _ = pairUpdate(m, m.Init()())
	if bt.scans != 0 || !strings.Contains(m.View(), "No SensorLink devices found on the device's network") {
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
	client := &pairTestClient{}
	h := pairTestHandler(client)
	h.ctx = ctx
	m := newSensorPairModel(h)
	m.devices = []models.DiscoveredDevice{{AssetID: 42, Sensorlink: true}}
	cancel()
	// The fake device answers a cancelled call the way gRPC does.
	updated, _ := m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if client.discoveries != 1 || len(m.devices) != 1 || m.scanning || !strings.Contains(m.message, "context canceled") {
		t.Fatalf("cancelled scan discarded known devices or stayed scanning: %q", m.message)
	}
	// A local scan cut short by cancellation is reported the same way.
	h.discoverLocal = func(ctx context.Context) ([]models.DiscoveredDevice, error) {
		if ctx.Err() == nil {
			t.Fatal("local discovery did not receive the cancelled context")
		}
		return nil, nil
	}
	h.client = &unimplementedDiscoveryClient{client}
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if len(m.devices) != 1 || !strings.Contains(m.message, "context canceled") {
		t.Fatalf("cancelled local scan discarded known devices: %q", m.message)
	}
}

// unimplementedDiscoveryClient is an agent that predates DiscoverSensorSources
// but otherwise answers like pairTestClient, cancelled context or not.
type unimplementedDiscoveryClient struct{ *pairTestClient }

func (c *unimplementedDiscoveryClient) DiscoverSensorSources(context.Context, *agentpbv2.DiscoverSensorSourcesRequest, ...grpc.CallOption) (*agentpbv2.DiscoverSensorSourcesResponse, error) {
	c.discoveries++
	return nil, status.Error(codes.Unimplemented, "unknown method DiscoverSensorSources for service wendy.agent.services.v2.WendySensorPairingService")
}

func TestSensorPairDiscoversOnTheDeviceNetwork(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	client := &pairTestClient{supportsPrincipal: true}
	h := pairTestHandler(client)
	h.tenantIDs = func() (map[string]bool, error) { return map[string]bool{tenant: true}, nil }
	h.discoverLocal = func(context.Context) ([]models.DiscoveredDevice, error) {
		t.Fatal("a device that answered must not trigger a local scan")
		return nil, nil
	}
	// The board sits on the device's network, at an address this computer
	// may never see.
	client.sources = []*agentpbv2.DiscoveredSensorSource{{
		Id: "441bf6804824", DisplayName: "garage-cam", Hostname: "wendylt-garage.local",
		IpAddress: "10.42.0.17", Addresses: []string{"10.42.0.17"}, Port: 5054, IsMtls: true,
		TenantUuid: tenant, PrincipalUri: "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-441bf6804824",
		Caps: []string{"sensors"}, WendyLite: true,
	}}
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	view := ansi.Strip(m.View())
	if m.localNetwork || len(m.rows) != 1 || !strings.Contains(view, "garage-cam") || !strings.Contains(view, "10.42.0.17") || strings.Contains(view, "this computer's network") {
		t.Fatalf("device-side source not shown as the device sees it: %s", view)
	}
	_, pair := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if pair == nil {
		t.Fatal("device-side source cannot be paired")
	}
	pair()
	// The device re-resolves the source by identity, so no address is pinned.
	if client.added == nil || client.added.SourcePrincipalUri != client.sources[0].PrincipalUri || client.added.SourceAddress != "" || client.added.Transport != "wendycom" {
		t.Fatalf("wrong pairing request: %+v", client.added)
	}
}

func TestSensorPairFallsBackToLocalScanOnlyForOldAgents(t *testing.T) {
	client := &pairTestClient{}
	h := pairTestHandler(client)
	old := &unimplementedDiscoveryClient{client}
	h.client = old
	locals := 0
	h.discoverLocal = func(context.Context) ([]models.DiscoveredDevice, error) {
		locals++
		return []models.DiscoveredDevice{{AssetID: 43, OrgID: 7, DisplayName: "Desk sensor", Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, IPAddress: "192.168.1.9"}}, nil
	}
	m := newSensorPairModel(h)
	updated, _ := m.Update(m.Init()())
	m = updated.(sensorPairModel)
	view := ansi.Strip(m.View())
	if locals != 1 || old.discoveries != 1 || !m.localNetwork || !strings.Contains(view, "Desk sensor") || !strings.Contains(view, "this computer's network") || !strings.Contains(view, "Update its agent") {
		t.Fatalf("old agent did not fall back to a labeled local scan: %s", view)
	}
	if m.message != "" {
		t.Fatalf("fallback reported as a failure: %q", m.message)
	}

	// Once the agent is updated, the device's own results replace the
	// local ones and the label disappears.
	h.client = client
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	view = ansi.Strip(m.View())
	if locals != 1 || m.localNetwork || strings.Contains(view, "Desk sensor") || !strings.Contains(view, "Camera") || strings.Contains(view, "this computer's network") {
		t.Fatalf("updated agent's results did not replace the local scan: %s", view)
	}

	// An old agent with no local fallback configured explains what to do.
	h.client, h.discoverLocal = old, nil
	updated, _ = m.Update(h.scan()())
	m = updated.(sensorPairModel)
	if !strings.Contains(m.message, "update it") || len(m.rows) != 1 {
		t.Fatalf("missing update hint or lost known sources: %q", m.message)
	}
}

func TestSensorPairDeviceDiscoveryErrorsAreNotReplacedByLocalScan(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{status.Error(codes.Unavailable, "tunnel closed"), "device is not reachable"},
		{status.Error(codes.PermissionDenied, "org mismatch"), "not authorized"},
		{status.Error(codes.DeadlineExceeded, "context deadline exceeded"), "the device did not finish scanning its network in time"},
		{status.Error(codes.Internal, "sensor source discovery failed on this device"), "the device could not scan its network: sensor source discovery failed on this device"},
	} {
		client := &pairTestClient{}
		h := pairTestHandler(client)
		h.discoverLocal = func(context.Context) ([]models.DiscoveredDevice, error) {
			t.Fatalf("%v: local scan replaced a device error", tc.err)
			return nil, nil
		}
		m := newSensorPairModel(h)
		updated, _ := m.Update(m.Init()())
		m = updated.(sensorPairModel)
		client.discoverErr = tc.err
		updated, _ = m.Update(h.scan()())
		m = updated.(sensorPairModel)
		view := ansi.Strip(m.View())
		if !strings.Contains(m.message, "Discovery failed: "+tc.want) || strings.Contains(view, "rpc error") || m.localNetwork {
			t.Fatalf("%v: message = %q", tc.err, m.message)
		}
		// Sources from the previous successful scan stay listed.
		if len(m.rows) != 1 || !strings.Contains(view, "Camera") {
			t.Fatalf("%v: failed rescan dropped known sources: %s", tc.err, view)
		}
	}
}

// When the tab's own deadline runs out (ListSensorPairings spends from it
// too), the device may never have seen the call, so the scan is not blamed.
func TestSensorPairDiscoveryTimeoutOnThisSideBlamesTheWait(t *testing.T) {
	client := &pairTestClient{}
	h := pairTestHandler(client)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	h.ctx = ctx
	msg := h.scan()().(sensorPairScanMsg)
	if client.discoveries != 1 || msg.discoveryErr == nil || msg.discoveryErr.Error() != "the device did not answer in time" {
		t.Fatalf("discoveries = %d, err = %v; want the wait blamed", client.discoveries, msg.discoveryErr)
	}
}

func TestSensorSourcesFromDeviceRechecksIdentityHints(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const other = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/lite-1"
	devices := sensorSourcesFromDevice([]*agentpbv2.DiscoveredSensorSource{
		nil,
		{Id: "a", DisplayName: "valid", Hostname: "a.local", IpAddress: "10.0.0.1", Addresses: []string{"10.0.0.1", "fe80::1"}, Port: 5054, IsMtls: true, TenantUuid: tenant, PrincipalUri: principal, Caps: []string{"sensors"}, WendyLite: true},
		{Id: "b", DisplayName: "foreign principal", TenantUuid: tenant, PrincipalUri: "spiffe://wendy.sh/tenant/" + other + "/device/lite-2", Caps: []string{"sensors"}},
		{Id: "c", DisplayName: "uppercase tenant", TenantUuid: strings.ToUpper(other), PrincipalUri: "spiffe://wendy.sh/tenant/" + strings.ToUpper(other) + "/device/lite-3", Caps: []string{"sensors"}},
		{Id: "d", DisplayName: "legacy", AssetId: 9, OrgId: 2, Port: 50052, IsMtls: true, Caps: []string{"camera", "sensors"}},
		{Id: "e", DisplayName: "negative ids", AssetId: -4, OrgId: -1, Caps: []string{"camera"}},
	})
	if len(devices) != 5 {
		t.Fatalf("got %d devices, want 5: %+v", len(devices), devices)
	}
	byName := map[string]models.DiscoveredDevice{}
	for _, d := range devices {
		byName[d.DisplayName] = d
	}
	valid := byName["valid"]
	if valid.PrincipalURI != principal || valid.TenantUUID != tenant || !valid.WendyLite || !valid.IsMTLS || !valid.Sensorlink ||
		valid.IPAddress != "10.0.0.1" || len(valid.Addresses) != 2 || valid.Port() != 5054 || valid.LAN.Hostname != "a.local" ||
		sensorSourcePrincipal(valid) != principal || transportForDevice(valid) != "wendycom" {
		t.Fatalf("valid source mapped wrong: %+v", valid)
	}
	if d := byName["foreign principal"]; d.PrincipalURI != "" || d.TenantUUID != tenant {
		t.Fatalf("principal outside its tenant survived: %+v", d)
	}
	if d := byName["uppercase tenant"]; d.TenantUUID != "" || d.PrincipalURI != "" {
		t.Fatalf("non-canonical tenant survived: %+v", d)
	}
	if d := byName["legacy"]; d.AssetID != 9 || d.OrgID != 2 || !d.Sensorlink || transportForDevice(d) != "grpc" {
		t.Fatalf("legacy source mapped wrong: %+v", d)
	}
	if d := byName["negative ids"]; d.AssetID != 0 || d.OrgID != 0 || d.Sensorlink {
		t.Fatalf("invalid ids or capability survived: %+v", d)
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
