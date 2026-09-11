// CoreBluetooth scanning bridge for internal/shared/ble/scan.
//
// Symbol naming is not free here: three cgo BLE translation units link into one
// wendy binary — this one, internal/shared/ble/central/ble_darwin.m
// (wendy_ble_connect and friends, class WendyBLEConnection) and
// internal/shared/discovery/bluetooth_darwin.m (wendy_ble_scan/_check/
// _free_result, class WendyBLEScanner). Objective-C class names are
// process-global and a duplicate makes the runtime pick one arbitrarily, so
// everything here is prefixed wendy_blescan_ and the class is
// WendyBLEScanSession.
#ifndef BLE_SCAN_DARWIN_H
#define BLE_SCAN_DARWIN_H

#include <stdint.h>

// Opaque handle to a running scan session.
typedef void *WendyBLEScanHandle;

// WendyBLEScanDeviceC is one discovered peripheral.
typedef struct {
    const char *address;       // CoreBluetooth peripheral identifier UUID string
    const char *name;          // advertised local name, or "" when unknown
    const char *service_uuids; // comma-separated advertised service UUIDs, or ""
    int rssi;                  // signal strength in dBm
} WendyBLEScanDeviceC;

// WendyBLEScanSnapshot is the set of peripherals seen so far this session.
typedef struct {
    WendyBLEScanDeviceC *devices;
    int count;
    const char *error; // NULL when fine, otherwise a message
} WendyBLEScanSnapshot;

// wendy_blescan_check tests whether CoreBluetooth is usable. Returns 0 when BLE
// is available, 1 when denied, restricted or absent. May SIGABRT in a sandboxed
// terminal, so callers run it in a subprocess.
int wendy_blescan_check(void);

// wendy_blescan_start begins an open-ended scan for every advertising
// peripheral, waiting up to ready_timeout_seconds for the adapter to power on.
// Returns NULL when BLE is unavailable. Filtering by service UUID is left to
// the caller: CoreBluetooth's own service filter drops peripherals whose
// advertisement omits the UUID, which is exactly the case a name-based fallback
// exists to catch.
WendyBLEScanHandle wendy_blescan_start(int ready_timeout_seconds);

// wendy_blescan_snapshot copies out everything seen so far. The caller must
// release it with wendy_blescan_free_snapshot.
WendyBLEScanSnapshot wendy_blescan_snapshot(WendyBLEScanHandle handle);

// wendy_blescan_free_snapshot frees a snapshot's strings and device array.
void wendy_blescan_free_snapshot(WendyBLEScanSnapshot snapshot);

// wendy_blescan_stop stops scanning and releases the session.
void wendy_blescan_stop(WendyBLEScanHandle handle);

#endif
