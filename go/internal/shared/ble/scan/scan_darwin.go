//go:build darwin

package scan

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreBluetooth -framework Foundation
#include "scan_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"strings"
	"sync"
	"unsafe"
)

// readyTimeoutSeconds is how long wendy_blescan_start waits for the adapter to
// report powered-on before giving up.
const readyTimeoutSeconds = 5

// RunBLECheck reports whether BLE is usable on this host: 0 if available, 1 if
// denied or restricted.
//
// On macOS this touches CoreBluetooth, which can SIGABRT rather than return an
// error when the terminal has no Bluetooth TCC permission. Run it in a
// subprocess — a long-lived process cannot survive that abort — and wire the
// result back through Options.Preflight.
func RunBLECheck() int {
	return int(C.wendy_blescan_check())
}

// darwinScanner holds a running CoreBluetooth scan session.
type darwinScanner struct {
	mu     sync.Mutex
	handle C.WendyBLEScanHandle
}

// newScanner starts a CoreBluetooth scan. services is ignored: the scan is
// unfiltered and the engine matches UUIDs in Go, because CoreBluetooth's own
// service filter also drops peripherals whose advertisement omits the UUID.
func newScanner(_ context.Context, _ []string) (scanner, error) {
	handle := C.wendy_blescan_start(C.int(readyTimeoutSeconds))
	if handle == nil {
		return nil, errors.New("Bluetooth unavailable - your terminal may not have Bluetooth permission")
	}
	return &darwinScanner{handle: handle}, nil
}

// Snapshot ignores ctx: it reads CoreBluetooth's already-cached state through a
// non-blocking cgo call, so there is nothing here that can hang.
func (s *darwinScanner) Snapshot(_ context.Context) ([]BLEDeviceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return nil, errors.New("BLE scan session is closed")
	}

	snapshot := C.wendy_blescan_snapshot(s.handle)
	defer C.wendy_blescan_free_snapshot(snapshot)

	if snapshot.error != nil {
		return nil, errors.New(C.GoString(snapshot.error))
	}
	if snapshot.count == 0 || snapshot.devices == nil {
		return nil, nil
	}

	count := int(snapshot.count)
	cDevices := unsafe.Slice(snapshot.devices, count)
	devices := make([]BLEDeviceInfo, 0, count)
	for _, cd := range cDevices {
		devices = append(devices, BLEDeviceInfo{
			Address:      C.GoString(cd.address),
			Name:         C.GoString(cd.name),
			ServiceUUIDs: splitList(C.GoString(cd.service_uuids)),
			RSSI:         int(cd.rssi),
		})
	}
	return devices, nil
}

// Close stops the scan. Idempotent, and it holds the same lock Snapshot does so
// it can never release the session out from under an in-flight snapshot.
func (s *darwinScanner) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return
	}
	C.wendy_blescan_stop(s.handle)
	s.handle = nil
}

// splitList parses the comma-separated service UUID list the Objective-C bridge
// produces, dropping empty entries. A C array of strings would need a second
// allocation and free per device for no real gain.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
