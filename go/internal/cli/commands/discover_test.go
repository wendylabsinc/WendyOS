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
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/env"
	"github.com/wendylabsinc/wendy/internal/shared/models"
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
	// Use tiny intervals so the test doesn't actually sleep.
	t.Setenv("WENDY_DISCOVER_USB_INTERVAL", "1ms")
	t.Setenv("WENDY_DISCOVER_ETHERNET_INTERVAL", "1ms")
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "1ms")

	m := newDiscoverModel(context.Background(), defaultOpts())

	// Each scan message type should return a non-nil command (the delayed rescan).
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"usb", usbScanMsg{devices: []models.USBDevice{{DisplayName: "test"}}}},
		{"ethernet", ethScanMsg{devices: []models.EthernetInterface{{DisplayName: "eth0"}}}},
		{"external", extScanMsg{devices: []models.ExternalDevice{{DisplayName: "ext0"}}}},
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

func TestDiscoverModel_QuitOnKeyMsg(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts())

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
	m := newDiscoverModel(context.Background(), defaultOpts())
	cmd := m.Init()
	if cmd == nil {
		t.Error("expected non-nil Init cmd (batch of scan commands)")
	}
}

func TestDiscoverModel_TableNavigation(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts())

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

func TestRenderDeviceTable(t *testing.T) {
	collection := &models.DevicesCollection{
		LANDevices: []models.LANDevice{{
			DisplayName:  "wendy-alpha",
			IPAddress:    "192.168.1.10",
			Port:         8443,
			AgentVersion: "1.2.3",
		}},
	}

	output := renderDeviceTable(collection)
	for _, want := range []string{"Name", "Device Type", "Address", "Version", "wendy-alpha", "192.168.1.10", "1.2.3"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, output)
		}
	}
}

func TestDiscoverDeviceInfo_JSONSingleDevice(t *testing.T) {
	info := discoverDeviceInfo{
		Name:    "wendyos-brave-phoenix",
		Type:    "LAN",
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

	m := newDiscoverModel(context.Background(), defaultOpts())
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

	m := newDiscoverModel(context.Background(), defaultOpts())
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

	m := newDiscoverModel(context.Background(), defaultOpts())
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
	m := newDiscoverModel(context.Background(), defaultOpts())
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
