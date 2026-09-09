// Package ble is the one Wendy-specific resident of internal/shared/ble, whose
// central and scan subpackages are otherwise free of Wendy identifiers.
//
// It lives here rather than in internal/cli/ble — where the rest of the Wendy
// protocol layer is — for one reason: internal/shared/discovery reads a Lite
// board's info service to learn its L2CAP PSM, and a shared package importing a
// cli one is an upward edge. Nothing else about it wants to be shared, so keep
// this package to the info service and let internal/cli/ble own the rest.
//
// internal/cli/ble is also named ble. No file currently needs both, and none
// should have to; a future one must alias.
//
// This package must never import internal/shared/discovery, which imports it.
package ble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/central"
)

// Wendy Lite device info service. Base 4E57454E-4459-0002-xxxx-000000000000
// ("NWENDY" + service 0002) — sibling of the 0001 provisioning service in
// lite_client.go. The device publishes this before any TLS work happens, so it
// is untrusted: treat it as a label for picking a device, never as proof of
// identity. The mTLS handshake is what authenticates.
const (
	// LiteInfoServiceUUID is exported because a Lite board also advertises it,
	// so it doubles as the scan filter that finds one (see shared/discovery's
	// BLELiteDeviceDiscoverContinuous). Every other UUID here is only ever read
	// off an open connection.
	LiteInfoServiceUUID     = "4E57454E-4459-0002-0000-000000000000"
	liteInfoPSMCharUUID     = "4E57454E-4459-0002-0001-000000000000"
	liteInfoDeviceIDUUID    = "4E57454E-4459-0002-0002-000000000000"
	liteInfoDeviceNameUUID  = "4E57454E-4459-0002-0003-000000000000"
	liteInfoDisplayNameUUID = "4E57454E-4459-0002-0004-000000000000"
	liteInfoMTLSUUID        = "4E57454E-4459-0002-0005-000000000000"
)

// LiteInfo is the content of a Wendy Lite device's GATT info service.
type LiteInfo struct {
	PSM         uint16
	DeviceID    string
	DeviceName  string
	DisplayName string
	MTLSEnabled bool
}

// ErrLiteInfoUnavailable reports that the device does not publish the info
// service, or that this platform cannot read GATT at all — Windows has no GATT
// client, so it always takes this path. Callers fall back to
// liteclient.DefaultL2CAPPSM rather than failing.
var ErrLiteInfoUnavailable = errors.New("Wendy Lite info service unavailable")

// ReadLiteInfoAt connects to a device, reads its info service and drops the
// link before returning. It is what a discovery pass wants: a Lite board
// accepts one BLE connection at a time, so a probe that kept the connection
// open would lock out the ConnectViaBLE that follows when the user picks the
// device.
//
// address is what a scan reported for this platform — a CoreBluetooth
// peripheral UUID on macOS, a MAC elsewhere. timeout bounds the initial connect
// and service discovery; characteristic reads use backend per-op timeouts.
func ReadLiteInfoAt(address string, timeout time.Duration) (*LiteInfo, error) {
	conn, err := central.Connect(address, central.TimeoutSeconds(timeout))
	if err != nil {
		return nil, fmt.Errorf("%w: connecting to %s: %w", ErrLiteInfoUnavailable, address, err)
	}
	defer conn.Close()
	return ReadLiteInfo(conn, timeout)
}

// ReadLiteInfo reads the device's GATT info service, which carries the L2CAP
// PSM to open along with the identity the device advertises for itself.
//
// Every characteristic in the service is required — identity, mTLS, and PSM
// alike; a device that answers some but not all of them is not recognized, and
// this returns ErrLiteInfoUnavailable rather than a partially filled LiteInfo.
// A caller must never be handed a partial identity — bleExternalDevice would
// surface a blank name, and MicroWendyProvider's mTLS filter cannot tell "not
// provisioned" from "couldn't read it". Every failure is wrapped in
// ErrLiteInfoUnavailable so a caller can fall back to its own default PSM, as
// liteclient.DefaultL2CAPPSM does.
func ReadLiteInfo(conn *central.Connection, timeout time.Duration) (*LiteInfo, error) {
	// Required before any characteristic op: both backends resolve a
	// characteristic against what discovery found, and report "not found"
	// against an empty index. This is also where Windows bows out.
	if err := conn.DiscoverServices(central.TimeoutSeconds(timeout)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLiteInfoUnavailable, err)
	}
	if !conn.HasService(LiteInfoServiceUUID) {
		return nil, fmt.Errorf("%w: device exposes [%s]", ErrLiteInfoUnavailable, conn.ListServices())
	}

	raw, err := conn.ReadCharacteristic(LiteInfoServiceUUID, liteInfoPSMCharUUID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading PSM: %w", ErrLiteInfoUnavailable, err)
	}
	if len(raw) < 2 {
		return nil, fmt.Errorf("%w: PSM characteristic is %d bytes, want 2", ErrLiteInfoUnavailable, len(raw))
	}
	// Little-endian, matching what the firmware publishes.
	psm := binary.LittleEndian.Uint16(raw[:2])
	if psm == 0 {
		return nil, fmt.Errorf("%w: device published PSM 0", ErrLiteInfoUnavailable)
	}

	deviceID, err := readLiteInfoString(conn, liteInfoDeviceIDUUID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading device ID: %w", ErrLiteInfoUnavailable, err)
	}
	deviceName, err := readLiteInfoString(conn, liteInfoDeviceNameUUID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading device name: %w", ErrLiteInfoUnavailable, err)
	}
	displayName, err := readLiteInfoString(conn, liteInfoDisplayNameUUID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading display name: %w", ErrLiteInfoUnavailable, err)
	}
	mtls, err := conn.ReadCharacteristic(LiteInfoServiceUUID, liteInfoMTLSUUID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading mTLS flag: %w", ErrLiteInfoUnavailable, err)
	}
	if len(mtls) == 0 {
		return nil, fmt.Errorf("%w: mTLS characteristic is empty", ErrLiteInfoUnavailable)
	}

	return &LiteInfo{
		PSM:         psm,
		DeviceID:    deviceID,
		DeviceName:  deviceName,
		DisplayName: displayName,
		MTLSEnabled: mtls[0] != 0,
	}, nil
}

// readLiteInfoString reads one UTF-8 characteristic. Every characteristic in
// the service is required (see ReadLiteInfo), so an empty value is as much a
// failure here as a GATT read error — ReadCharacteristic returns (nil, nil)
// when the characteristic holds no bytes, which this turns into an error too.
func readLiteInfoString(conn *central.Connection, charUUID string) (string, error) {
	data, err := conn.ReadCharacteristic(LiteInfoServiceUUID, charUUID)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("characteristic %s is empty", charUUID)
	}
	return string(data), nil
}
