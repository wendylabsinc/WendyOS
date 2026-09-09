package central

import "github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"

// friendlyGATTError renders a BlueZ failure on a characteristic operation as
// user-facing text, or returns ok=false to say the raw error is the best
// available and the caller should keep it.
//
// It sits in front of bluez.FriendlyError rather than replacing it, because a
// few names mean something different once the object in question is a
// characteristic rather than a device:
//
//   - UnknownObject on a device path means "rescan"; on a characteristic path
//     it means bluetoothd tore the GATT tree down, which only happens when the
//     link dropped.
//   - Failed on a device path carries a bearer reason; on a characteristic it
//     carries an ATT error, which has no bearer vocabulary at all.
//
// This file carries no build tag, and imports only the untagged half of bluez,
// so its test runs on every platform.
func friendlyGATTError(name, message string) (text string, ok bool) {
	switch name {
	case "org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.bluez.Error.DoesNotExist":
		return "the characteristic is gone — the link dropped, or the device changed its GATT layout; reconnect and rediscover", true

	case "org.bluez.Error.InvalidValueLength":
		return "the device rejected the value's length — it may exceed the negotiated MTU", true
	case "org.bluez.Error.InvalidArguments":
		return "the device rejected the request arguments", true

	case "org.bluez.Error.Failed":
		// An ATT-layer failure, not a bearer one: pass the device's own words
		// through rather than running them past the bearer-reason table, which
		// has nothing to say about them.
		if message == "" {
			return "the device rejected the operation", true
		}
		return message, true
	}

	// Everything else — NotPermitted, NotAuthorized, NotSupported,
	// NotConnected, InProgress, AccessDenied, ServiceUnknown, NoReply — means
	// the same thing for a characteristic as for a device.
	text, _, ok = bluez.FriendlyError(name, message)
	return text, ok
}
