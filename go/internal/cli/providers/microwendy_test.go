package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestEspIdfBinaryPath(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		product string
		wantBin string // basename of the expected binary; "" means an error is expected
	}{
		{
			name: "binary named after CMake project name",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\ninclude($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(myfw)\n",
				"build/myfw.bin": "firmware",
			},
			product: "my-folder",
			wantBin: "myfw.bin",
		},
		{
			name: "falls back to product without a project() declaration",
			files: map[string]string{
				"build/my-folder.bin": "firmware",
			},
			product: "my-folder",
			wantBin: "my-folder.bin",
		},
		{
			name: "missing binary",
			files: map[string]string{
				"CMakeLists.txt": "include($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(myfw)\n",
			},
			product: "my-folder",
			wantBin: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			binPath, err := espIdfBinaryPath(dir, tt.product)
			if tt.wantBin == "" {
				if err == nil {
					t.Fatal("espIdfBinaryPath() expected an error for a missing binary")
				}
				return
			}
			if err != nil {
				t.Fatalf("espIdfBinaryPath() error = %v", err)
			}
			if want := filepath.Join(dir, "build", tt.wantBin); binPath != want {
				t.Errorf("espIdfBinaryPath() = %q, want %q", binPath, want)
			}
		})
	}
}

func TestBoardToTarget(t *testing.T) {
	// Boards currently map to targets one-to-one by name; this pins the
	// identity mapping until real board names diverge from SoC names.
	if got := boardToTarget("esp32c6"); got != "esp32c6" {
		t.Errorf("boardToTarget(esp32c6) = %q, want %q", got, "esp32c6")
	}
}

func TestSerialExternalDeviceUnresponsive(t *testing.T) {
	p := &MicroWendyProvider{}
	dev := p.serialExternalDevice(discovery.SerialDevice{Port: "/dev/cu.usbmodem2101", Responsive: false})

	if dev.ConnectionInfo["needsInstall"] != "true" {
		t.Errorf("expected needsInstall=true for an unresponsive port, got ConnectionInfo=%+v", dev.ConnectionInfo)
	}
	if dev.ConnectionInfo["serialPort"] != "/dev/cu.usbmodem2101" {
		t.Errorf("expected serialPort to be preserved, got %+v", dev.ConnectionInfo)
	}
	if !dev.IsWendyDevice {
		t.Errorf("expected an unresponsive ESP32 board to still be flagged IsWendyDevice")
	}
}

func TestSerialExternalDeviceResponsive(t *testing.T) {
	p := &MicroWendyProvider{}
	dev := p.serialExternalDevice(discovery.SerialDevice{
		Port: "/dev/cu.usbmodem2101", Responsive: true,
		ID: "esp", Name: "esp", DisplayName: "esp",
	})

	if dev.ConnectionInfo["needsInstall"] != "" {
		t.Errorf("expected no needsInstall marker for a responsive port, got ConnectionInfo=%+v", dev.ConnectionInfo)
	}
	if dev.DisplayName != "esp" {
		t.Errorf("expected the resolved identity display name to be used, got %q", dev.DisplayName)
	}
}

func TestConnectableLiteMDNSService(t *testing.T) {
	tests := []struct {
		name string
		svc  discovery.MDNSService
		want bool
	}{
		{name: "resolved", svc: discovery.MDNSService{IPAddress: "192.0.2.10", Port: 5054}, want: true},
		{name: "stale record without address", svc: discovery.MDNSService{Hostname: "old.local", Port: 5054}, want: false},
		{name: "missing port", svc: discovery.MDNSService{IPAddress: "192.0.2.10"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectableLiteMDNSService(tt.svc); got != tt.want {
				t.Errorf("connectableLiteMDNSService(%+v) = %v, want %v", tt.svc, got, tt.want)
			}
		})
	}
}

func TestContendedPortsError(t *testing.T) {
	tests := []struct {
		name  string
		ports []string
		want  string
	}{
		{
			name:  "single port",
			ports: []string{"/dev/cu.usbmodem101"},
			want:  "found an ESP32 serial port but couldn't open it: /dev/cu.usbmodem101 is in use by another process (e.g. a running `wendy device camera view` or `wendy run`) — stop it and try again",
		},
		{
			name:  "multiple ports",
			ports: []string{"/dev/cu.usbmodem101", "/dev/cu.usbmodem201"},
			want:  "found 2 ESP32 serial ports but couldn't open them: /dev/cu.usbmodem101, /dev/cu.usbmodem201 are in use by another process (e.g. a running `wendy device camera view` or `wendy run`) — stop it and try again",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contendedPortsError(tt.ports).Error(); got != tt.want {
				t.Errorf("contendedPortsError(%v) = %q, want %q", tt.ports, got, tt.want)
			}
		})
	}
}

func TestConnectSerialWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	origFn, origDelay := connectSerialFn, serialConnectRetryDelay
	defer func() { connectSerialFn, serialConnectRetryDelay = origFn, origDelay }()
	serialConnectRetryDelay = time.Millisecond

	var attempts int
	connectSerialFn = func(_ *liteclient.WendyLiteClient, _ string) error {
		attempts++
		if attempts < serialConnectMaxAttempts {
			return errors.New("reading header: read timeout")
		}
		return nil
	}

	if err := connectSerialWithRetry(nil, "/dev/cu.usbmodem2101"); err != nil {
		t.Fatalf("connectSerialWithRetry() error = %v, want nil after eventual success", err)
	}
	if attempts != serialConnectMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, serialConnectMaxAttempts)
	}
}

func TestConnectSerialWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	origFn, origDelay := connectSerialFn, serialConnectRetryDelay
	defer func() { connectSerialFn, serialConnectRetryDelay = origFn, origDelay }()
	serialConnectRetryDelay = time.Millisecond

	wantErr := errors.New("reading header: read timeout")
	var attempts int
	connectSerialFn = func(_ *liteclient.WendyLiteClient, _ string) error {
		attempts++
		return wantErr
	}

	err := connectSerialWithRetry(nil, "/dev/cu.usbmodem2101")
	if !errors.Is(err, wantErr) {
		t.Fatalf("connectSerialWithRetry() error = %v, want it to wrap %v", err, wantErr)
	}
	if attempts != serialConnectMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, serialConnectMaxAttempts)
	}
	if !strings.Contains(err.Error(), "/dev/cu.usbmodem2101") {
		t.Errorf("error %q should mention the serial port", err.Error())
	}
}

func TestGetDeviceInfoShortCircuitsForNeedsInstall(t *testing.T) {
	p := &MicroWendyProvider{}
	device := p.serialExternalDevice(discovery.SerialDevice{Port: "/dev/cu.usbmodem2101", Responsive: false})

	_, err := p.GetDeviceInfo(context.Background(), device)

	var unsupported *AppRequirementsUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("GetDeviceInfo() error = %v, want an *AppRequirementsUnsupportedError", err)
	}
}

func TestBLEExternalDevice(t *testing.T) {
	p := &MicroWendyProvider{}
	dev := p.bleExternalDevice(discovery.BLELiteDevice{
		Address: "1B2C3D4E-0000-0000-0000-000000000000",
		Name:    "wendy-5f2c",
		RSSI:    -42,
		Info: ble.LiteInfo{
			PSM: 129, DeviceID: "5f2c", DeviceName: "wendy-5f2c",
			DisplayName: "Kitchen Sensor", MTLSEnabled: true,
		},
	})

	if dev.ID != "wendy-lite:1B2C3D4E-0000-0000-0000-000000000000" {
		t.Errorf("expected the BLE address to identify the connection, got ID %q", dev.ID)
	}
	if dev.DisplayName != "Kitchen Sensor" {
		t.Errorf("expected the device's display name, got %q", dev.DisplayName)
	}
	if dev.ConnectionType() != "BLE" {
		t.Errorf("expected connection type BLE, got %q", dev.ConnectionType())
	}
	if dev.ConnectionInfo["address"] != "1B2C3D4E-0000-0000-0000-000000000000" {
		t.Errorf("expected the address to be preserved, got ConnectionInfo=%+v", dev.ConnectionInfo)
	}
	if dev.ConnectionInfo["psm"] != "129" {
		t.Errorf("expected the published PSM to travel with the row, got %q", dev.ConnectionInfo["psm"])
	}
	if dev.ConnectionInfo["mtls"] != "true" {
		t.Errorf("expected mtls=true for a device that reported it, got %q", dev.ConnectionInfo["mtls"])
	}
	if !dev.IsWendyDevice {
		t.Error("expected a Wendy Lite board to be flagged IsWendyDevice")
	}
}

func TestBLELiteDisplayName(t *testing.T) {
	tests := []struct {
		name string
		dev  discovery.BLELiteDevice
		want string
	}{
		{
			name: "display name wins",
			dev:  discovery.BLELiteDevice{Name: "adv", Info: ble.LiteInfo{DeviceName: "device", DisplayName: "display"}},
			want: "display",
		},
		{
			name: "falls back to the device name",
			dev:  discovery.BLELiteDevice{Name: "adv", Info: ble.LiteInfo{DeviceName: "device"}},
			want: "device",
		},
		{
			name: "falls back to the advertised name",
			dev:  discovery.BLELiteDevice{Name: "adv"},
			want: "adv",
		},
		{
			name: "generic label when the board named itself nothing",
			dev:  discovery.BLELiteDevice{Address: "aa:bb"},
			want: "Wendy Lite",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bleLiteDisplayName(tt.dev); got != tt.want {
				t.Errorf("bleLiteDisplayName(%+v) = %q, want %q", tt.dev, got, tt.want)
			}
		})
	}
}

// collectExternalDevices runs streamDevices over the given sources and returns
// its last emission — every snapshot is the whole set, so the final one is the
// state the stream ended on. The caller ends the stream by cancelling ctx
// (streamDevices no longer returns just because one source's channel closes —
// see TestStreamDevicesSurvivesMDNSStreamEnding).
func collectExternalDevices(
	ctx context.Context,
	svcCh <-chan discovery.MDNSService,
	serialUpdates <-chan []discovery.SerialDevice,
	bleCh <-chan []discovery.BLELiteDevice,
) []models.ExternalDevice {
	out := make(chan []models.ExternalDevice, 16)
	go func() {
		defer close(out)
		(&MicroWendyProvider{}).streamDevices(ctx, svcCh, serialUpdates, bleCh, nil, out)
	}()

	var devices []models.ExternalDevice
	for snapshot := range out {
		devices = snapshot
	}
	return devices
}

func TestStreamDevicesEmitsBLEDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bleCh := make(chan []discovery.BLELiteDevice, 1)
	bleCh <- []discovery.BLELiteDevice{
		{Address: "aa", Info: ble.LiteInfo{PSM: 128, DisplayName: "one", MTLSEnabled: true}},
		{Address: "bb", Info: ble.LiteInfo{PSM: 128, DisplayName: "two", MTLSEnabled: true}},
	}
	close(bleCh)

	svcCh := make(chan discovery.MDNSService)
	go func() {
		// Give the BLE snapshot above time to be drained before ending the
		// stream.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	devices := collectExternalDevices(ctx, svcCh, nil, bleCh)
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want both BLE boards: %+v", len(devices), devices)
	}
	for _, dev := range devices {
		if dev.ConnectionType() != "BLE" {
			t.Errorf("expected BLE rows, got %+v", dev)
		}
	}
}

func TestStreamDevicesSurvivesBLEStreamEnding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bleCh := make(chan []discovery.BLELiteDevice)
	close(bleCh)

	svcCh := make(chan discovery.MDNSService, 1)
	svcCh <- discovery.MDNSService{Hostname: "lite.local", IPAddress: "192.0.2.10", Port: 5054}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	devices := collectExternalDevices(ctx, svcCh, nil, bleCh)
	if len(devices) != 1 || devices[0].ConnectionType() != "LAN" {
		t.Fatalf("a closed BLE stream must not stop mDNS discovery; got %+v", devices)
	}
}

// TestStreamDevicesSurvivesMDNSStreamEnding is the regression test for the
// mirror-image bug: streamDevices used to treat the mDNS browse closing as a
// reason to end the whole merge, which stopped draining bleCh — against the
// real sources, that left startBLELiteSource's forwarder permanently blocked
// on an unbuffered send (see startBLELiteSource), keeping BLE scan goroutines
// and the radio alive for no reason. Against these plain channels, the same
// bug deadlocks this test: the goroutine below would block forever sending to
// bleCh once svcCh has already ended the stream.
func TestStreamDevicesSurvivesMDNSStreamEnding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svcCh := make(chan discovery.MDNSService)
	close(svcCh) // mDNS browse died immediately; ctx stays live.

	bleCh := make(chan []discovery.BLELiteDevice)
	out := make(chan []models.ExternalDevice, 16)
	go func() {
		defer close(out)
		(&MicroWendyProvider{}).streamDevices(ctx, svcCh, nil, bleCh, nil, out)
	}()

	// Sent after svcCh has already closed, so this only succeeds if the merge
	// loop is still running (and still reading bleCh) rather than having
	// returned when mDNS died. If that regression comes back this send blocks
	// forever and the test times out, which is the failure we want.
	bleCh <- []discovery.BLELiteDevice{
		{Address: "aa", Info: ble.LiteInfo{PSM: 128, DisplayName: "one", MTLSEnabled: true}},
	}

	// Take the snapshot before cancelling, rather than cancelling straight
	// after the send. emit() selects between sending to out and ctx.Done(), so
	// once the context is already cancelled both cases are ready — out has room
	// — and Go picks at random, dropping the snapshot often enough to fail this
	// test roughly 3% of the time under -race.
	devices := <-out
	cancel()
	for range out { //nolint:revive // drain so streamDevices' goroutine can exit
	}

	if len(devices) != 1 || devices[0].ConnectionType() != "BLE" {
		t.Fatalf("a closed mDNS browse must not stop BLE discovery; got %+v", devices)
	}
}

// TestStreamDevicesSnapshotMirrorsEachSource pins the ContinuousDiscoverer
// contract: an emission is the union of the sources, and each source's shape
// is respected — the newest serial set replaces the previous one (so an
// unplugged board drops out) while mDNS rows, which only ever arrive as
// announcements, stay.
func TestStreamDevicesSnapshotMirrorsEachSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unbuffered, so each send lands before the next one is attempted and the
	// sequence below is the order streamDevices sees.
	svcCh := make(chan discovery.MDNSService)
	serialUpdates := make(chan []discovery.SerialDevice)

	go func() {
		serialUpdates <- []discovery.SerialDevice{
			{Port: "/dev/ttyUSB0", ID: "board", Name: "board", DisplayName: "Board", Responsive: true},
		}
		svcCh <- discovery.MDNSService{Hostname: "lite.local", IPAddress: "192.0.2.10", Port: 5054}
		// The board is unplugged: the scanner now reports an empty set.
		serialUpdates <- nil
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	devices := collectExternalDevices(ctx, svcCh, serialUpdates, nil)
	if len(devices) != 1 || devices[0].ConnectionType() != "LAN" {
		t.Fatalf("the unplugged board must drop out and the mDNS row must stay; got %+v", devices)
	}
}

// TestStreamDevicesDeduplicatesRepeatedMDNSService proves a re-announced
// service updates its row in place instead of appending a second one — a
// browse re-sends records freely, and every snapshot is consumed as-is.
func TestStreamDevicesDeduplicatesRepeatedMDNSService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svcCh := make(chan discovery.MDNSService, 2)
	svc := discovery.MDNSService{Hostname: "lite.local", IPAddress: "192.0.2.10", Port: 5054}
	svcCh <- svc
	svcCh <- svc
	go func() {
		// Give both buffered sends time to be drained before ending the
		// stream.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	devices := collectExternalDevices(ctx, svcCh, nil, nil)
	if len(devices) != 1 {
		t.Fatalf("a re-announced service must not duplicate its row; got %+v", devices)
	}
}

func TestConnectClientRejectsBLEWithoutAddress(t *testing.T) {
	p := &MicroWendyProvider{}
	_, err := p.connectClient(models.ExternalDevice{
		ConnectionInfo: map[string]string{"type": "BLE"},
	})
	if err == nil || !strings.Contains(err.Error(), "missing BLE address") {
		t.Errorf("expected a missing-address error, got %v", err)
	}
}
