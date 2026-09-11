// Package bluez is the BlueZ D-Bus plumbing shared by the BLE scanner and the
// BLE central: enumerating the object tree, resolving an adapter and a device,
// reading typed properties off a variant map, and turning a raw D-Bus failure
// into something a user can act on.
//
// Nothing here is specific to any device or protocol. The one product-specific
// concession is the WENDY_BT_ADAPTER environment variable, which pins a
// controller — it has to spell the same thing the agent already reads.
//
// Everything but the error table is Linux-only, and lives in bluez_linux.go.
// This file carries no build tag so that `go build ./...` on darwin and windows
// finds a package rather than "build constraints exclude all Go files".
//
// internal/agent/bluetooth still has its own older copies of several of these
// helpers. Converging them is a separate change on device-side code; the
// versions here are the merged best of that copy and the scanner's.
package bluez
