#ifndef BLUETOOTH_DARWIN_H
#define BLUETOOTH_DARWIN_H

// WendyBLEDevice holds a single discovered BLE peripheral.
typedef struct {
    const char *uuid; // peripheral identifier UUID string
    const char *name; // local name from advertisement data
    int rssi;         // signal strength in dBm
} WendyBLEDevice;

// WendyBLEScanResult holds the array of discovered BLE peripherals.
typedef struct {
    WendyBLEDevice *devices;
    int count;
} WendyBLEScanResult;

// wendy_ble_scan scans for BLE peripherals advertising the WendyOS service UUID
// for scan_seconds. Returns discovered devices. Caller must call wendy_ble_free_result.
WendyBLEScanResult wendy_ble_scan(int scan_seconds);

// wendy_ble_free_result frees all memory allocated by wendy_ble_scan.
void wendy_ble_free_result(WendyBLEScanResult result);

#endif
