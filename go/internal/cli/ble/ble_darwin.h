#ifndef BLE_CLIENT_DARWIN_H
#define BLE_CLIENT_DARWIN_H

#include <stdbool.h>
#include <stdint.h>

// WendyBLEError codes
typedef enum {
    WENDY_BLE_OK = 0,
    WENDY_BLE_ERR_TIMEOUT = 1,
    WENDY_BLE_ERR_NOT_FOUND = 2,
    WENDY_BLE_ERR_CONNECT_FAILED = 3,
    WENDY_BLE_ERR_DISCOVER_FAILED = 4,
    WENDY_BLE_ERR_WRITE_FAILED = 5,
    WENDY_BLE_ERR_READ_FAILED = 6,
    WENDY_BLE_ERR_L2CAP_FAILED = 7,
    WENDY_BLE_ERR_DISCONNECTED = 8,
} WendyBLEError;

// Opaque handle to a BLE connection
typedef void *WendyBLEConn;

// Result of a read operation
typedef struct {
    uint8_t *data;
    int length;
    WendyBLEError error;
} WendyBLEReadResult;

// Result of an L2CAP receive operation
typedef struct {
    uint8_t *data;
    int length;
    WendyBLEError error;
} WendyBLEL2CAPRecvResult;

// Connect to a BLE peripheral by UUID string.
// Returns an opaque connection handle or NULL on failure.
// timeout_seconds: how long to wait for connection.
WendyBLEConn wendy_ble_connect(const char *peripheral_uuid, int timeout_seconds, WendyBLEError *out_error);

// Discover services and characteristics for the connection.
// Must be called after connect and before read/write/subscribe.
WendyBLEError wendy_ble_discover_services(WendyBLEConn conn, int timeout_seconds);

// Write data to a GATT characteristic (write with response).
WendyBLEError wendy_ble_write_characteristic(WendyBLEConn conn, const char *service_uuid,
                                              const char *char_uuid, const uint8_t *data, int length);

// Write data to a GATT characteristic (write without response).
WendyBLEError wendy_ble_write_characteristic_no_response(WendyBLEConn conn, const char *service_uuid,
                                                          const char *char_uuid, const uint8_t *data, int length);

// Read data from a GATT characteristic.
WendyBLEReadResult wendy_ble_read_characteristic(WendyBLEConn conn, const char *service_uuid,
                                                  const char *char_uuid);

// Subscribe to notifications on a GATT characteristic.
// After subscribing, use wendy_ble_wait_notification to receive values.
WendyBLEError wendy_ble_subscribe(WendyBLEConn conn, const char *service_uuid,
                                   const char *char_uuid);

// Wait for a notification value on a subscribed characteristic.
// Blocks until a notification arrives or timeout_seconds elapses.
WendyBLEReadResult wendy_ble_wait_notification(WendyBLEConn conn, const char *service_uuid,
                                                const char *char_uuid, int timeout_seconds);

// Open an L2CAP channel on the given PSM.
WendyBLEError wendy_ble_open_l2cap(WendyBLEConn conn, uint16_t psm, int timeout_seconds);

// Send data over the L2CAP channel.
WendyBLEError wendy_ble_l2cap_send(WendyBLEConn conn, const uint8_t *data, int length);

// Receive data from the L2CAP channel (blocks until data or timeout).
WendyBLEL2CAPRecvResult wendy_ble_l2cap_recv(WendyBLEConn conn, int timeout_seconds);

// Disconnect and free all resources.
void wendy_ble_disconnect(WendyBLEConn conn);

// Free data returned by read/recv results.
void wendy_ble_free_data(uint8_t *data);

#endif
