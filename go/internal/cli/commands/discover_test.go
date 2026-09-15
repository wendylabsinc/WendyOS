package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestEnvDiscoverIntervals(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		envVal string
		fn     func() time.Duration
		want   time.Duration
	}{
		{"usb default", "WENDY_DISCOVER_USB_INTERVAL", "", env.DiscoverUSBInterval, 3 * time.Second},
		{"usb custom", "WENDY_DISCOVER_USB_INTERVAL", "5s", env.DiscoverUSBInterval, 5 * time.Second},
		{"usb invalid", "WENDY_DISCOVER_USB_INTERVAL", "notaduration", env.DiscoverUSBInterval, 3 * time.Second},
		{"ethernet default", "WENDY_DISCOVER_ETHERNET_INTERVAL", "", env.DiscoverEthernetInterval, 3 * time.Second},
		{"ethernet custom", "WENDY_DISCOVER_ETHERNET_INTERVAL", "500ms", env.DiscoverEthernetInterval, 500 * time.Millisecond},
		{"external default", "WENDY_DISCOVER_EXTERNAL_INTERVAL", "", env.DiscoverExternalInterval, 5 * time.Second},
		{"external custom", "WENDY_DISCOVER_EXTERNAL_INTERVAL", "10s", env.DiscoverExternalInterval, 10 * time.Second},
		{"usb zero", "WENDY_DISCOVER_USB_INTERVAL", "0s", env.DiscoverUSBInterval, 3 * time.Second},
		{"usb negative", "WENDY_DISCOVER_USB_INTERVAL", "-1s", env.DiscoverUSBInterval, 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envKey, tt.envVal)
			got := tt.fn()
			if got != tt.want {
				t.Errorf("got %v; want %v", got, tt.want)
			}
		})
	}
}

func TestDelayThen(t *testing.T) {
	called := false
	inner := func() tea.Msg {
		called = true
		return "done"
	}

	// With zero delay the inner cmd should execute immediately.
	cmd := delayThen(0, inner)
	msg := cmd()
	if !called {
		t.Fatal("inner cmd was not called")
	}
	if msg != "done" {
		t.Errorf("msg = %v; want \"done\"", msg)
	}
}

func TestDelayThen_ActuallyDelays(t *testing.T) {
	inner := func() tea.Msg { return "done" }

	start := time.Now()
	cmd := delayThen(50*time.Millisecond, inner)
	cmd()
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Errorf("delayThen returned too fast (%v); expected >= 50ms delay", elapsed)
	}
}

func TestDiscoverModel_UpdateReturnsDelayedCmd(t *testing.T) {
	// Use tiny intervals so the test doesn't actually sleep if a returned command runs.
	t.Setenv("WENDY_DISCOVER_USB_INTERVAL", "1ms")
	t.Setenv("WENDY_DISCOVER_ETHERNET_INTERVAL", "1ms")
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "1ms")

	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	// Each scan message type should return a non-nil command (the delayed rescan).
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"usb", usbScanMsg{devices: []models.USBDevice{{DisplayName: "test"}}}},
		{"ethernet", ethScanMsg{devices: []models.EthernetInterface{{DisplayName: "eth0"}}}},
		{"external", extScanMsg{provider: &fakeProvider{key: "fake-ext"}, devices: []models.ExternalDevice{{DisplayName: "ext0"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, cmd := m.Update(tc.msg)
			um := updated.(discoverModel)
			if !um.hasResults {
				t.Error("expected hasResults = true after scan message")
			}
			if cmd == nil {
				t.Error("expected non-nil cmd (delayed rescan)")
			}
		})
	}
}

func TestDiscoverModel_UpdateRampsRescanIntervals(t *testing.T) {
	t.Setenv("WENDY_DISCOVER_USB_INTERVAL", "3s")

	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	updated, _ := m.Update(usbScanMsg{devices: []models.USBDevice{{DisplayName: "test"}}})
	m = updated.(discoverModel)
	if m.usbInterval.next != time.Second {
		t.Fatalf("next USB interval = %v, want 1s", m.usbInterval.next)
	}

	updated, _ = m.Update(usbScanMsg{devices: []models.USBDevice{{DisplayName: "test"}}})
	m = updated.(discoverModel)
	if m.usbInterval.next != 2*time.Second {
		t.Fatalf("next USB interval = %v, want 2s", m.usbInterval.next)
	}
}

func TestDiscoverModel_QuitOnKeyMsg(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(discoverModel)
	if !um.quitting {
		t.Error("expected quitting = true after 'q' key")
	}
	if cmd == nil {
		t.Error("expected non-nil quit cmd")
	}
}

func TestDiscoverModel_Init(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	cmd := m.Init()
	if cmd == nil {
		t.Error("expected non-nil Init cmd (batch of scan commands)")
	}
}

func TestDiscoverModel_TableNavigation(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	updated, _ := m.Update(usbScanMsg{devices: []models.USBDevice{
		{DisplayName: "alpha"},
		{DisplayName: "beta"},
	}})
	um := updated.(discoverModel)

	if um.table.Cursor() != 0 {
		t.Fatalf("expected cursor to start at row 0, got %d", um.table.Cursor())
	}

	updated, _ = um.Update(tea.KeyMsg{Type: tea.KeyDown})
	um = updated.(discoverModel)

	if um.table.Cursor() != 1 {
		t.Fatalf("expected cursor to move to row 1, got %d", um.table.Cursor())
	}
}

func TestDiscoverModel_WindowWidthCropsViewLines(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = updated.(discoverModel)
	updated, _ = m.Update(lanFoundEvent(models.LANDevice{
		DisplayName:  "Ardent Cashew",
		Hostname:     "wendyos-ardent-cashew.local",
		Port:         defaultAgentPort,
		AgentVersion: "2026.05.30-161141",
		OSVersion:    "WendyOS-0.13.2",
	}))
	m = updated.(discoverModel)

	if !m.canScrollTable() {
		t.Fatalf("expected table width %d to exceed viewport %d", tui.PickerTableWidth(m.table.Columns()), m.tableViewportWidth())
	}

	view := m.View()
	if !strings.Contains(view, "←/→ scroll") {
		t.Fatalf("expected scroll hint in cropped view, got %q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if got := ansi.StringWidth(line); got > 60 {
			t.Fatalf("view line width = %d, want <= 60: %q", got, line)
		}
	}
}

func TestDiscoverModel_LeftRightScrollsWithoutBreakingVerticalNavigation(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(discoverModel)
	updated, _ = m.Update(lanFoundEvent(models.LANDevice{
		DisplayName:  "alpha",
		Hostname:     "wendyos-alpha.local",
		Port:         defaultAgentPort,
		AgentVersion: "2026.05.30-161141",
		OSVersion:    "WendyOS-0.13.2",
	}))
	m = updated.(discoverModel)
	updated, _ = m.Update(lanFoundEvent(models.LANDevice{
		DisplayName:  "beta",
		Hostname:     "wendyos-beta.local",
		Port:         defaultAgentPort,
		AgentVersion: "2026.05.30-161141",
		OSVersion:    "WendyOS-0.13.2",
	}))
	m = updated.(discoverModel)

	before := m.tableView()
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(discoverModel)

	if m.table.ScrollOffset() == 0 {
		t.Fatal("expected right arrow to advance horizontal offset")
	}
	if after := m.tableView(); after == before {
		t.Fatal("expected scrolled table view to change")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(discoverModel)

	if m.table.Cursor() != 1 {
		t.Fatalf("expected down arrow to move cursor, got %d", m.table.Cursor())
	}
	if m.table.ScrollOffset() == 0 {
		t.Fatal("expected vertical navigation to preserve horizontal offset")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(discoverModel)

	if m.table.ScrollOffset() != 0 {
		t.Fatalf("expected left arrow to return to zero offset, got %d", m.table.ScrollOffset())
	}
}

func TestDiscoverModel_DKeySetsDefaultAndMarksSelectedDevice(t *testing.T) {
	setTempConfig(t, &config.Config{})

	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	updated, _ := m.Update(lanFoundEvent(models.LANDevice{
		DisplayName: "ubuntu",
		Hostname:    "ubuntu.local",
		Port:        defaultAgentPort,
	}))
	m = updated.(discoverModel)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	um := updated.(discoverModel)

	if um.flashMessage != "Default device set to: ubuntu.local" {
		t.Fatalf("flash message = %q, want default device confirmation", um.flashMessage)
	}
	if cmd == nil {
		t.Fatal("expected clearFlashAfter cmd")
	}
	if !strings.Contains(um.table.View(), "✦") {
		t.Fatalf("expected selected default device to show marker, got %q", um.table.View())
	}
}

func TestDiscoverTableItemsHidesAddressForDockerAndLocal(t *testing.T) {
	collection := &models.DevicesCollection{
		ExternalDevices: []models.ExternalDevice{
			{ID: "docker", DisplayName: "Docker", ProviderKey: "docker"},
			{ID: "local", DisplayName: "This Mac", ProviderKey: "local"},
			{ID: "emulator-5554", DisplayName: "Pixel 8", ProviderKey: "android-adb"},
		},
	}

	items := discoverTableItems(collection)
	byName := make(map[string]discoverTableItem, len(items))
	for _, item := range items {
		byName[item.info.Name] = item
	}

	for _, name := range []string{"Docker", "This Mac"} {
		if got := byName[name].picker.Address; got != "" {
			t.Fatalf("%s Address = %q, want hidden", name, got)
		}
	}
	if got := byName["Pixel 8"].picker.Address; got != "android-adb: emulator-5554" {
		t.Fatalf("Pixel 8 Address = %q, want provider-qualified ID", got)
	}
}

func TestDiscoverModel_ViewShowsLegendWithDevices(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	if strings.Contains(m.View(), tui.DeviceTableLegendBase) {
		t.Fatalf("expected no legend before any device is found, got %q", m.View())
	}

	updated, _ := m.Update(lanFoundEvent(models.LANDevice{
		DisplayName: "ubuntu",
		Hostname:    "ubuntu.local",
		Port:        defaultAgentPort,
	}))
	m = updated.(discoverModel)

	if !strings.Contains(m.View(), tui.DeviceTableLegendBase) {
		t.Fatalf("expected legend under device table, got %q", m.View())
	}
}

func TestRenderDeviceTable(t *testing.T) {
	collection := &models.DevicesCollection{
		LANDevices: []models.LANDevice{{
			DisplayName:  "wendy-alpha",
			IPAddress:    "192.168.1.10",
			Port:         8443,
			AgentVersion: "1.2.3",
			OSVersion:    "WendyOS-0.10.4",
		}},
	}

	output := renderDeviceTable(collection)
	for _, want := range []string{"Name", "Type", "Address", "Agent", "OS", "wendy-alpha", "1.2.3", "WendyOS-0.10.4", "LAN", "192.168.1.10", tui.DeviceTableLegendBase} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, output)
		}
	}
	// The agent port is an implementation detail and is stripped from the list.
	if strings.Contains(output, "192.168.1.10:8443") {
		t.Fatalf("expected port stripped from displayed address, got %q", output)
	}
	if strings.Contains(output, "wendy-alpha v1.2.3") {
		t.Fatalf("expected version in dedicated column, got suffixed name in %q", output)
	}
}

func TestDiscoverTableItemsPrioritizesUSBDevices(t *testing.T) {
	collection := &models.DevicesCollection{
		LANDevices: []models.LANDevice{
			{
				DisplayName: "wendy-wifi",
				IPAddress:   "192.168.1.20",
			},
			{
				DisplayName: "wendy-usb",
				IPAddress:   "169.254.20.30",
				USB:         "enp0s20f0u9 480 Mbps",
			},
		},
	}

	items := discoverTableItems(collection)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].info.Name != "wendy-usb" {
		t.Fatalf("first item name = %q, want USB device first", items[0].info.Name)
	}
	if items[0].picker.USB != "enp0s20f0u9 480 Mbps" {
		t.Fatalf("USB detail = %q, want interface summary", items[0].picker.USB)
	}
	if items[1].picker.USB != "" {
		t.Fatalf("non-USB item USB detail = %q, want empty", items[1].picker.USB)
	}

	_, rows := tui.PickerDeviceTableData(discoverPickerItems(items), "", true)
	if rows[0][2] != "USB, LAN" {
		t.Fatalf("Type display cell = %q, want \"USB, LAN\"", rows[0][2])
	}
}

func TestDiscoverTableItemsAnnotatesLANUSBFromEthernetInterface(t *testing.T) {
	collection := &models.DevicesCollection{
		LANDevices: []models.LANDevice{{
			DisplayName:      "wendy-usb",
			IPAddress:        "169.254.20.30",
			NetworkInterface: "Ethernet 3",
		}},
		EthernetInterfaces: []models.EthernetInterface{{
			Name:        "Ethernet 3",
			DisplayName: "Wendy Gadget Mode",
			LinkSpeed:   "1 Gbps",
		}},
	}

	items := discoverTableItems(collection)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (ethernet hidden)", len(items))
	}
	if items[0].info.Name != "wendy-usb" {
		t.Fatalf("first item name = %q, want annotated LAN device first", items[0].info.Name)
	}
	wantUSB := "Wendy Gadget Mode (Ethernet 3) 1 Gbps"
	if items[0].picker.USB != wantUSB {
		t.Fatalf("USB detail = %q, want %q", items[0].picker.USB, wantUSB)
	}

	_, rows := tui.PickerDeviceTableData(discoverPickerItems(items), "", true)
	if rows[0][2] != "USB, LAN" {
		t.Fatalf("Type display cell = %q, want \"USB, LAN\"", rows[0][2])
	}
}

func TestDiscoverTableItemsProvisionedStateAndNoAccessHint(t *testing.T) {
	collection := &models.DevicesCollection{
		LANDevices: []models.LANDevice{
			// Provisioned device the CLI cannot read (metadata probe failed).
			{DisplayName: "wendy-locked", IPAddress: "192.168.1.30", IsMTLS: true},
			// Provisioned device the CLI can read.
			{DisplayName: "wendy-mine", IPAddress: "192.168.1.31", IsMTLS: true, AgentVersion: "1.2.3"},
			// Unprovisioned device.
			{DisplayName: "wendy-open", IPAddress: "192.168.1.32", AgentVersion: "1.2.3"},
		},
	}

	items := discoverTableItems(collection)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	byName := make(map[string]discoverTableItem, len(items))
	for _, item := range items {
		byName[item.info.Name] = item
	}

	locked := byName["wendy-locked"]
	if locked.picker.Provisioned != "Provisioned" {
		t.Fatalf("locked Provisioned = %q, want \"Provisioned\"", locked.picker.Provisioned)
	}
	if locked.picker.Hint != discoverNoAccessHint {
		t.Fatalf("locked Hint = %q, want no-access hint", locked.picker.Hint)
	}
	if locked.info.Provisioned != "Provisioned" {
		t.Fatalf("locked info.Provisioned = %q, want \"Provisioned\"", locked.info.Provisioned)
	}

	mine := byName["wendy-mine"]
	if mine.picker.Provisioned != "Provisioned" {
		t.Fatalf("mine Provisioned = %q, want \"Provisioned\"", mine.picker.Provisioned)
	}
	if mine.picker.Hint != "" {
		t.Fatalf("mine Hint = %q, want empty for accessible device", mine.picker.Hint)
	}

	open := byName["wendy-open"]
	if open.picker.Provisioned != "Unprovisioned" {
		t.Fatalf("open Provisioned = %q, want \"Unprovisioned\"", open.picker.Provisioned)
	}
	if open.picker.Hint != "" {
		t.Fatalf("open Hint = %q, want empty", open.picker.Hint)
	}

	_, rows := tui.PickerDeviceTableData(discoverPickerItems(items), "", true)
	// Columns: 0=marker (provisioned glyph + ✦ default) 1=Name 2=Type 3=Address 4=Agent 5=OS.
	if rows[0][0] != "●" {
		t.Fatalf("provisioned marker cell = %q, want \"●\"", rows[0][0])
	}
}

func TestMergePickerItemClearsNoAccessHintWhenVersionKnown(t *testing.T) {
	lanItem := func(dev models.LANDevice) tui.PickerItem {
		return tui.PickerItem{
			Name:         dev.DisplayName,
			Type:         "LAN",
			AgentVersion: dev.AgentVersion,
			Provisioned:  lanProvisionedDisplay(&dev),
			Hint:         lanNoAccessHint(&dev, dev.AgentVersion),
			DedupKey:     dev.DisplayName,
			Value:        &pickerEntry{mergedDevice: &models.DiscoveredDevice{DisplayName: dev.DisplayName, AgentVersion: dev.AgentVersion, LAN: &dev}},
		}
	}

	// A successful probe followed by a failed re-probe: the carried-over
	// version must not coexist with a hint claiming details are unreadable.
	existing := lanItem(models.LANDevice{DisplayName: "wendy-mine", IsMTLS: true, AgentVersion: "1.2.3"})
	mergePickerItem(&existing, lanItem(models.LANDevice{DisplayName: "wendy-mine", IsMTLS: true}))
	if existing.Hint != "" {
		t.Fatalf("Hint = %q, want empty when AgentVersion is already known", existing.Hint)
	}
	if existing.AgentVersion != "1.2.3" {
		t.Fatalf("AgentVersion = %q, want carried-over version", existing.AgentVersion)
	}

	// A failed probe followed by a successful one clears the hint.
	existing = lanItem(models.LANDevice{DisplayName: "wendy-locked", IsMTLS: true})
	if existing.Hint == "" {
		t.Fatal("expected no-access hint on inaccessible provisioned device")
	}
	mergePickerItem(&existing, lanItem(models.LANDevice{DisplayName: "wendy-locked", IsMTLS: true, AgentVersion: "1.2.3"}))
	if existing.Hint != "" {
		t.Fatalf("Hint = %q, want cleared after successful probe", existing.Hint)
	}

	// Still-failing probes keep the hint while no version is known.
	existing = lanItem(models.LANDevice{DisplayName: "wendy-locked", IsMTLS: true})
	mergePickerItem(&existing, lanItem(models.LANDevice{DisplayName: "wendy-locked", IsMTLS: true}))
	if existing.Hint != discoverNoAccessHint {
		t.Fatalf("Hint = %q, want no-access hint to persist", existing.Hint)
	}

	// A BLE backfill that supplies the version must also clear the hint,
	// not just LAN merges.
	existing = lanItem(models.LANDevice{DisplayName: "wendy-locked", IsMTLS: true})
	bleDev := models.BluetoothDevice{DisplayName: "wendy-locked", AgentVersion: "1.2.3"}
	mergePickerItem(&existing, tui.PickerItem{
		Name:         bleDev.DisplayName,
		Type:         "Bluetooth",
		AgentVersion: bleDev.AgentVersion,
		DedupKey:     bleDev.DisplayName,
		Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
			DisplayName:  bleDev.DisplayName,
			AgentVersion: bleDev.AgentVersion,
			Bluetooth:    &bleDev,
		}},
	})
	if existing.AgentVersion != "1.2.3" {
		t.Fatalf("AgentVersion = %q, want BLE-backfilled version", existing.AgentVersion)
	}
	if existing.Hint != "" {
		t.Fatalf("Hint = %q, want cleared after BLE version backfill", existing.Hint)
	}
}

// TestMergePickerItemMergesBLEIntoLANByHostname verifies the run-picker collapses
// a BLE row (raw hostname) into the LAN row (friendly displayname) for the same
// physical device, regardless of which transport is discovered first. This is the
// picker-side counterpart to the MergedDevices hostname match.
func TestMergePickerItemMergesBLEIntoLANByHostname(t *testing.T) {
	lanDev := models.LANDevice{DisplayName: "Agx Orin", Hostname: "wendyos-agx-orin.local", IPAddress: "192.168.1.10", Port: 50051}
	bleDev := models.BluetoothDevice{DisplayName: "wendyos-agx-orin", Address: "EE48983F-300C-3A22-395B-290A76741CA1", L2CAPPSM: 128}

	lanItem := tui.PickerItem{
		Name:     lanDev.DisplayName,
		Type:     "LAN",
		Address:  preferredLANAddress(lanDev),
		DedupKey: deviceDedupKey(lanDev.HostKey(), lanDev.DisplayName),
		SortKey:  deviceSortKey(lanDev.DisplayName, lanDev.USB),
		Value:    &pickerEntry{mergedDevice: &models.DiscoveredDevice{DisplayName: lanDev.DisplayName, LAN: &lanDev}},
	}
	bleItem := tui.PickerItem{
		Name:     bleDev.DisplayName,
		Type:     "BLE",
		Address:  bleDev.Address,
		DedupKey: deviceDedupKey(bleDev.HostKey(), bleDev.DisplayName),
		SortKey:  deviceSortKey(bleDev.DisplayName, ""),
		Value:    &pickerEntry{mergedDevice: &models.DiscoveredDevice{DisplayName: bleDev.DisplayName, Bluetooth: &bleDev}},
	}

	// The picker dedups by DedupKey, so the shared hostname key is what collapses
	// the two rows into one.
	if lanItem.DedupKey != bleItem.DedupKey {
		t.Fatalf("dedup keys differ: LAN=%q BLE=%q (should both be the hostname)", lanItem.DedupKey, bleItem.DedupKey)
	}

	// LAN discovered first, then BLE merges in.
	t.Run("LANThenBLE", func(t *testing.T) {
		existing := lanItem
		existing.Value = &pickerEntry{mergedDevice: &models.DiscoveredDevice{DisplayName: lanDev.DisplayName, LAN: &lanDev}}
		mergePickerItem(&existing, bleItem)
		if existing.Type != "LAN, BLE" {
			t.Errorf("Type = %q, want %q", existing.Type, "LAN, BLE")
		}
		if existing.Name != "Agx Orin" {
			t.Errorf("Name = %q, want friendly LAN name %q", existing.Name, "Agx Orin")
		}
	})

	// BLE discovered first, then LAN merges in: the friendly name and LAN address
	// must win.
	t.Run("BLEThenLAN", func(t *testing.T) {
		existing := bleItem
		existing.Value = &pickerEntry{mergedDevice: &models.DiscoveredDevice{DisplayName: bleDev.DisplayName, Bluetooth: &bleDev}}
		mergePickerItem(&existing, lanItem)
		if existing.Type != "LAN, BLE" {
			t.Errorf("Type = %q, want %q", existing.Type, "LAN, BLE")
		}
		if existing.Name != "Agx Orin" {
			t.Errorf("Name = %q, want friendly LAN name to win over BLE hostname", existing.Name)
		}
		if existing.Address != preferredLANAddress(lanDev) {
			t.Errorf("Address = %q, want LAN address %q", existing.Address, preferredLANAddress(lanDev))
		}
	})
}

func TestDiscoverModelViewShowsNoAccessHintForHighlightedDevice(t *testing.T) {
	m := newDiscoverModel(context.Background(), discovery.DiscoveryOptions{}, true)
	dev := models.LANDevice{
		DisplayName: "wendy-locked",
		IPAddress:   "192.168.1.30",
		IsMTLS:      true,
	}
	updated, _ := m.Update(lanCachedEvent(dev))
	dm := updated.(discoverModel)

	// While the probe is still in flight the row is "connecting" (spinner), so
	// the no-access hint is suppressed.
	if view := ansi.Strip(dm.View()); strings.Contains(view, "does not have access") {
		t.Fatalf("no-access hint should be suppressed while connecting, got %q", view)
	}

	// Once the device is confirmed offline without ever resolving a version,
	// the provisioned device shows the no-access hint — offline is a
	// concluded probe outcome, not a pending one.
	updated, _ = dm.Update(lanOfflineEvent(dev))
	dm = updated.(discoverModel)

	view := ansi.Strip(dm.View())
	if !strings.Contains(view, "does not have access") {
		t.Fatalf("expected no-access hint in view, got %q", view)
	}
	if !strings.Contains(view, "wendy auth login") {
		t.Fatalf("expected login suggestion in view, got %q", view)
	}
}

func TestDiscoverModelViewOmitsHintForAccessibleDevice(t *testing.T) {
	m := newDiscoverModel(context.Background(), discovery.DiscoveryOptions{}, true)
	updated, _ := m.Update(lanFoundEvent(models.LANDevice{
		DisplayName:  "wendy-mine",
		IPAddress:    "192.168.1.31",
		IsMTLS:       true,
		AgentVersion: "1.2.3",
	}))
	dm := updated.(discoverModel)

	view := ansi.Strip(dm.View())
	if strings.Contains(view, "does not have access") {
		t.Fatalf("unexpected no-access hint for accessible device: %q", view)
	}
}

func TestDiscoverDeviceInfo_JSONSingleDevice(t *testing.T) {
	info := discoverDeviceInfo{
		Name:    "wendyos-brave-phoenix",
		Type:    "LAN",
		USB:     "en6 1 Gbps",
		Address: "192.168.1.42",
		Version: "2026.03.16-163942",
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["name"] != "wendyos-brave-phoenix" {
		t.Errorf("name = %v", parsed["name"])
	}
	if parsed["address"] != "192.168.1.42" {
		t.Errorf("address = %v", parsed["address"])
	}
	if parsed["usb"] != "en6 1 Gbps" {
		t.Errorf("usb = %v", parsed["usb"])
	}
}

func TestDiscoverDeviceInfo_OmitsEmptyFields(t *testing.T) {
	info := discoverDeviceInfo{
		Name:    "wendyos-test",
		Type:    "USB",
		Address: "192.168.55.100",
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := parsed["version"]; ok {
		t.Error("empty version should be omitted")
	}
}

func TestDiscoverDeviceInfo_AllDevicesArray(t *testing.T) {
	all := []discoverDeviceInfo{
		{Name: "device-1", Type: "LAN", Address: "192.168.1.1"},
		{Name: "device-2", Type: "USB", Address: "192.168.55.100"},
	}

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(parsed) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(parsed))
	}
	if parsed[0]["name"] != "device-1" {
		t.Errorf("first device name = %v", parsed[0]["name"])
	}
}

func TestDiscoverModel_EnterCopiesSelectedDevice(t *testing.T) {
	orig := clipboardWriter
	t.Cleanup(func() { clipboardWriter = orig })

	var copied string
	clipboardWriter = func(text string) error {
		copied = text
		return nil
	}

	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	updated0, _ := m.Update(usbScanMsg{devices: []models.USBDevice{
		{DisplayName: "wendyos-test", Hostname: "192.168.1.5"},
	}})
	m = updated0.(discoverModel)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(discoverModel)

	if um.flashMessage != "Copied device info as JSON to clipboard." {
		t.Errorf("flash = %q, want success message", um.flashMessage)
	}
	if cmd == nil {
		t.Error("expected clearFlashAfter cmd")
	}
	if !strings.Contains(copied, "wendyos-test") {
		t.Errorf("clipboard content should contain device name, got %q", copied)
	}

	// Verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(copied), &parsed); err != nil {
		t.Fatalf("clipboard content is not valid JSON: %v", err)
	}
}

func TestDiscoverModel_ACopiesAllDevices(t *testing.T) {
	orig := clipboardWriter
	t.Cleanup(func() { clipboardWriter = orig })

	var copied string
	clipboardWriter = func(text string) error {
		copied = text
		return nil
	}

	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	updated0, _ := m.Update(usbScanMsg{devices: []models.USBDevice{
		{DisplayName: "device-1", Hostname: "10.0.0.1"},
		{DisplayName: "device-2", Hostname: "10.0.0.2"},
	}})
	m = updated0.(discoverModel)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	um := updated.(discoverModel)

	if um.flashMessage != "Copied all devices as JSON to clipboard." {
		t.Errorf("flash = %q, want all-devices message", um.flashMessage)
	}
	if cmd == nil {
		t.Error("expected clearFlashAfter cmd")
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(copied), &parsed); err != nil {
		t.Fatalf("clipboard content is not valid JSON array: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(parsed))
	}
}

func TestDiscoverModel_EnterShowsErrorOnClipboardFailure(t *testing.T) {
	orig := clipboardWriter
	t.Cleanup(func() { clipboardWriter = orig })

	clipboardWriter = func(text string) error {
		return fmt.Errorf("xclip not found")
	}

	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	updated0, _ := m.Update(usbScanMsg{devices: []models.USBDevice{
		{DisplayName: "test-device", Hostname: "10.0.0.1"},
	}})
	m = updated0.(discoverModel)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(discoverModel)

	if !strings.Contains(um.flashMessage, "Copy failed") {
		t.Errorf("flash = %q, expected error message", um.flashMessage)
	}
}

func TestDiscoverModel_FlashClearMsg(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)
	m.flashMessage = "test flash"

	updated, _ := m.Update(flashClearMsg{})
	um := updated.(discoverModel)
	if um.flashMessage != "" {
		t.Errorf("flashMessage should be cleared, got %q", um.flashMessage)
	}
}

func defaultOpts() discovery.DiscoveryOptions {
	return discovery.DiscoveryOptions{Timeout: time.Second}
}

func TestCopyToClipboard_FallsBackOnRunFailure(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS has only one clipboard candidate (pbcopy); fallback-to-second-tool test is Linux-only")
	}
	origLookPath := execLookPath
	origCommand := execCommand
	origWriter := clipboardWriter
	t.Cleanup(func() {
		execLookPath = origLookPath
		execCommand = origCommand
		clipboardWriter = origWriter
	})

	// All tools are "found" by LookPath.
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	callCount := 0
	// First tool fails, second succeeds.
	execCommand = func(name string, args ...string) *exec.Cmd {
		callCount++
		if callCount == 1 {
			// Return a command that will fail (false always exits 1).
			return exec.Command("false")
		}
		// Return a command that succeeds (true always exits 0).
		return exec.Command("true")
	}

	err := copyToClipboard("hello")
	if err != nil {
		t.Fatalf("expected success after fallback, got: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 tool attempts, got %d", callCount)
	}
}

func TestCopyToClipboard_AllToolsFailReportsErrors(t *testing.T) {
	origLookPath := execLookPath
	origCommand := execCommand
	origWriter := clipboardWriter
	t.Cleanup(func() {
		execLookPath = origLookPath
		execCommand = origCommand
		clipboardWriter = origWriter
	})

	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}
	execCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	err := copyToClipboard("hello")
	if err == nil {
		t.Fatal("expected error when all tools fail")
	}
	if !strings.Contains(err.Error(), "all clipboard tools failed") {
		t.Errorf("error should mention all tools failed, got: %v", err)
	}
}

func TestShouldCaptureClipboardStderr_DisablesLinuxPipes(t *testing.T) {
	if shouldCaptureClipboardStderr("linux") {
		t.Fatal("linux clipboard helpers should not capture stderr through pipes")
	}
	if !shouldCaptureClipboardStderr("darwin") {
		t.Fatal("darwin clipboard helper should capture stderr")
	}
}

func TestRunClipboardCommand_TimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}

	start := time.Now()
	err := runClipboardCommand(exec.Command("sleep", "5"), 25*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestCopyToClipboard_NoToolsFound(t *testing.T) {
	origLookPath := execLookPath
	origCommand := execCommand
	origWriter := clipboardWriter
	t.Cleanup(func() {
		execLookPath = origLookPath
		execCommand = origCommand
		clipboardWriter = origWriter
	})

	execLookPath = func(file string) (string, error) {
		return "", fmt.Errorf("not found")
	}

	err := copyToClipboard("hello")
	if err == nil {
		t.Fatal("expected error when no tools found")
	}
	if !strings.Contains(err.Error(), "no clipboard tool found") {
		t.Errorf("error should list candidates, got: %v", err)
	}
	if !strings.Contains(err.Error(), "install one of") {
		t.Errorf("error should suggest installation, got: %v", err)
	}
}

// TestDiscoverCmdHasNoAllFlag verifies `wendy discover` no longer carries an
// --all flag: local run targets are hidden by default and revealed only via the
// WENDY_SHOW_LOCAL_DEVICES environment variable.
func TestDiscoverCmdHasNoAllFlag(t *testing.T) {
	cmd := newDiscoverCmd()
	if flag := cmd.Flags().Lookup("all"); flag != nil {
		t.Fatal("discover should no longer define an --all flag")
	}
}
