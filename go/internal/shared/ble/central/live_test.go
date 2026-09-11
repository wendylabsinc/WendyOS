package central

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

// TestLiveGATT drives the real platform backend against real hardware. It is
// skipped unless WENDY_BLE_LIVE_ADDR names a peripheral, because CI has no
// radio — and on macOS no CI job compiles this package's cgo at all, so this is
// the only thing that exercises either bridge.
//
//	WENDY_BLE_LIVE_ADDR=AA:BB:CC:DD:EE:FF go test ./internal/shared/ble/central -run TestLiveGATT -v
//
// The address is what a scan reports on this platform: a MAC on Linux, a
// CoreBluetooth peripheral UUID on macOS. Find one with
// `WENDY_BLE_LIVE_SCAN=1 go test ./internal/shared/ble/scan -run TestLiveScan -v`.
//
// Optional:
//
//	WENDY_BLE_LIVE_SERVICE  a service UUID expected on the device
//	WENDY_BLE_LIVE_CHAR     a readable characteristic in that service
//	WENDY_BLE_LIVE_PSM      an L2CAP PSM the device listens on
func TestLiveGATT(t *testing.T) {
	address := os.Getenv("WENDY_BLE_LIVE_ADDR")
	if address == "" {
		t.Skip("set WENDY_BLE_LIVE_ADDR=<address> to run against real hardware")
	}

	const timeoutSeconds = 15

	t.Run("discover", func(t *testing.T) {
		conn, err := Connect(address, timeoutSeconds)
		if err != nil {
			t.Fatalf("Connect(%s): %v", address, err)
		}
		defer conn.Close()

		if err := conn.DiscoverServices(timeoutSeconds); err != nil {
			t.Fatalf("DiscoverServices: %v", err)
		}
		services := conn.ListServices()
		if services == "" {
			t.Fatal("discovery reported no services at all")
		}
		t.Logf("services: %s", services)

		service := os.Getenv("WENDY_BLE_LIVE_SERVICE")
		if service == "" {
			t.Log("set WENDY_BLE_LIVE_SERVICE to check for a specific service")
			return
		}
		if !conn.HasService(service) {
			t.Fatalf("HasService(%s) is false; device exposes [%s]", service, services)
		}

		char := os.Getenv("WENDY_BLE_LIVE_CHAR")
		if char == "" {
			t.Log("set WENDY_BLE_LIVE_CHAR to read a characteristic")
			return
		}
		value, err := conn.ReadCharacteristic(service, char)
		if err != nil {
			t.Fatalf("ReadCharacteristic(%s, %s): %v", service, char, err)
		}
		t.Logf("read %d byte(s): %x", len(value), value)

		// A characteristic the device does not have must come back as the
		// sentinel, not as some opaque transport error.
		_, err = conn.ReadCharacteristic(service, "0000FFFF-0000-1000-8000-00805F9B34FB")
		if !errors.Is(err, ErrGATTNotFound) {
			t.Errorf("reading an absent characteristic gave %v, want ErrGATTNotFound", err)
		}
	})

	t.Run("notify", func(t *testing.T) {
		service := os.Getenv("WENDY_BLE_LIVE_SERVICE")
		char := os.Getenv("WENDY_BLE_LIVE_CHAR")
		if service == "" || char == "" {
			t.Skip("set WENDY_BLE_LIVE_SERVICE and WENDY_BLE_LIVE_CHAR to exercise notifications")
		}

		conn, err := Connect(address, timeoutSeconds)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer conn.Close()

		if err := conn.DiscoverServices(timeoutSeconds); err != nil {
			t.Fatalf("DiscoverServices: %v", err)
		}
		if err := conn.Subscribe(service, char); err != nil {
			// Not every characteristic is notifiable; say which one it was.
			t.Skipf("Subscribe(%s): %v", char, err)
		}
		value, err := conn.WaitNotification(service, char, timeoutSeconds)
		if err != nil {
			// A quiet characteristic is a legitimate result, not a failure.
			t.Logf("no notification within %ds: %v", timeoutSeconds, err)
			return
		}
		t.Logf("notification: %d byte(s): %x", len(value), value)
	})

	// liteclient's ordering: read the PSM over GATT, then open the channel on
	// the same connection. The ACL link BlueZ brought up for the GATT read is
	// reused by the kernel rather than a second one being opened, so this is
	// also the check that the two halves coexist.
	t.Run("gatt then l2cap", func(t *testing.T) {
		raw := os.Getenv("WENDY_BLE_LIVE_PSM")
		if raw == "" {
			t.Skip("set WENDY_BLE_LIVE_PSM to exercise the L2CAP path")
		}
		psm, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			t.Fatalf("WENDY_BLE_LIVE_PSM=%q is not a PSM: %v", raw, err)
		}

		conn, err := Connect(address, timeoutSeconds)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer conn.Close()

		if err := conn.DiscoverServices(timeoutSeconds); err != nil {
			t.Fatalf("DiscoverServices: %v", err)
		}
		if err := conn.OpenL2CAP(uint16(psm), timeoutSeconds); err != nil {
			t.Fatalf("OpenL2CAP(%d) after discovery: %v", psm, err)
		}
		t.Logf("L2CAP channel open on PSM %d over the link BlueZ established", psm)
	})

	// The regression gate for the paths that already worked before GATT
	// existed: opening a channel must touch neither BlueZ nor the GATT session.
	t.Run("l2cap only", func(t *testing.T) {
		raw := os.Getenv("WENDY_BLE_LIVE_PSM")
		if raw == "" {
			t.Skip("set WENDY_BLE_LIVE_PSM to exercise the L2CAP path")
		}
		psm, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			t.Fatalf("WENDY_BLE_LIVE_PSM=%q is not a PSM: %v", raw, err)
		}

		conn, err := Connect(address, timeoutSeconds)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer conn.Close()

		if err := conn.OpenL2CAP(uint16(psm), timeoutSeconds); err != nil {
			t.Fatalf("OpenL2CAP(%d): %v", psm, err)
		}
		t.Logf("L2CAP channel open on PSM %d with no GATT call", psm)
	})
}
