package bluez

import (
	"errors"
	"strings"
)

// ErrDeviceNotFound reports that a peripheral is not known to BlueZ and was not
// seen during on-demand discovery. It is a sentinel so a caller can tell
// "rescan" apart from a genuine connection failure.
var ErrDeviceNotFound = errors.New("bluetooth device not found")

// FriendlyError converts a D-Bus/BlueZ error, identified by its D-Bus error
// name and message body, into user-facing text. notFound reports that the error
// means the object does not exist in BlueZ; ok=false means the name was not
// recognized and the caller should keep the raw error.
//
// This file carries no build tag, and must stay free of godbus imports, so the
// table is testable on every platform. Use ErrorInfo (Linux only) to pull the
// name and message out of a godbus error first.
//
// It is a superset of the table in internal/agent/bluetooth/errors.go: the
// agent connects to peripherals, while a central also reads and writes
// characteristics and can be denied bus access outright.
func FriendlyError(name, message string) (text string, notFound, ok bool) {
	switch name {
	case "org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.bluez.Error.DoesNotExist":
		return "device is no longer known to the Bluetooth adapter — make sure it is powered on and in range, then rescan", true, true

	case "org.bluez.Error.Failed":
		return FriendlyBearerFailure(message), false, true

	case "org.bluez.Error.AuthenticationFailed":
		return "pairing authentication failed — put the device in pairing mode and retry", false, true
	case "org.bluez.Error.AuthenticationRejected":
		return "the device rejected pairing — it may be bonded to another host; unpair it there, or forget it here and retry", false, true
	case "org.bluez.Error.AuthenticationCanceled":
		return "pairing was canceled by the device", false, true
	case "org.bluez.Error.AuthenticationTimeout":
		return "pairing timed out — make sure the device is in pairing mode and in range", false, true
	case "org.bluez.Error.ConnectionAttemptFailed":
		return "could not reach the device to pair — make sure it is powered on and in range", false, true
	case "org.bluez.Error.InProgress":
		return "another Bluetooth operation is in progress — retry in a few seconds", false, true
	case "org.bluez.Error.NotReady":
		return "the Bluetooth adapter is not ready — check that it is powered on", false, true

	// Central-side additions. A GATT client touches attributes the agent never
	// does, and runs as whatever user invoked the CLI rather than as a service.
	case "org.bluez.Error.NotConnected":
		return "the link to the device dropped — reconnect and retry", false, true
	case "org.bluez.Error.NotPermitted", "org.bluez.Error.NotAuthorized":
		return "the device requires pairing for this attribute — pair it with `bluetoothctl` first", false, true
	case "org.bluez.Error.NotSupported":
		return "the device does not support this operation on that characteristic", false, true

	case "org.freedesktop.DBus.Error.NoReply":
		return "the Bluetooth service did not respond in time — retry", false, true
	case "org.freedesktop.DBus.Error.ServiceUnknown":
		return "the Bluetooth service (bluetoothd) is not running", false, true
	case "org.freedesktop.DBus.Error.AccessDenied":
		// The failure a headless CI job or a non-console SSH session actually
		// hits: BlueZ's D-Bus policy grants at-console users by default, and
		// nothing else. Worth naming, because the raw error says only
		// "Rejected send message".
		return "the D-Bus policy denies BlueZ access to this user — run as root, or add the user to the `bluetooth` group", false, true
	}
	return "", false, false
}

// IsTransientError reports whether a D-Bus/BlueZ failure, identified by its
// error name and message body, is worth retrying rather than surfacing
// immediately. BlueZ's "unknown" bearer reason (br-connection-unknown /
// le-connection-unknown) is its catch-all for an HCI-level disconnect status it
// could not classify, and in practice this often fires as a momentary race —
// e.g. a peripheral still tearing down its previous connection — rather than a
// permanent rejection, so a short retry frequently succeeds.
// org.bluez.Error.InProgress and a bus-level NoReply are likewise transient by
// definition. A canceled attempt (br-connection-canceled /
// le-connection-abort-by-local) is a collision with another connect on the same
// device — bluetoothd's own background auto-connect races an explicit one every
// ~2s — and retrying lets the explicit attempt run to completion and observe
// the real error. Every other classified reason (refused, timeout,
// adapter-not-powered, authentication rejected, device not found, ...) reflects
// a real condition that a bare retry will not fix.
func IsTransientError(name, message string) bool {
	switch name {
	case "org.bluez.Error.InProgress", "org.freedesktop.DBus.Error.NoReply":
		return true
	case "org.bluez.Error.Failed":
		return strings.Contains(message, "br-connection-unknown") ||
			strings.Contains(message, "le-connection-unknown") ||
			strings.Contains(message, "br-connection-busy") ||
			strings.Contains(message, "le-connection-busy") ||
			strings.Contains(message, "br-connection-canceled") ||
			strings.Contains(message, "le-connection-abort-by-local")
	}
	return false
}

// FriendlyBearerFailure maps the reason strings BlueZ places in
// org.bluez.Error.Failed messages (src/error.c, e.g. "br-connection-refused")
// to actionable text. Unrecognized bearer reasons get a generic hint that
// embeds the raw reason; messages without a bearer reason pass through
// unchanged (older BlueZ reports plain strerror text there).
func FriendlyBearerFailure(message string) string {
	has := func(reasons ...string) bool {
		for _, r := range reasons {
			if strings.Contains(message, r) {
				return true
			}
		}
		return false
	}

	switch {
	case has("br-connection-page-timeout", "br-connection-timeout", "le-connection-timeout"):
		return "the device did not respond — make sure it is powered on and in range"
	case has("br-connection-refused", "br-connection-aborted-by-remote", "le-connection-refused"):
		return "the device refused the connection — put it in pairing mode; if it is paired to another host, disconnect or unpair it there first"
	case has("br-connection-unknown", "le-connection-unknown"):
		return "the device rejected or dropped the connection — put it in pairing mode and retry; if it is bonded to another device, unpair it there first"
	case has("br-connection-adapter-not-powered", "le-connection-adapter-not-powered"):
		return "the Bluetooth adapter is powered off"
	case has("br-connection-busy", "le-connection-busy"):
		return "the Bluetooth adapter is busy — wait a few seconds and retry"
	case has("br-connection-key-missing"):
		return "stored pairing keys are stale — forget the device and pair again"
	case has("br-connection-profile-unavailable", "br-connection-sdp-search"):
		return "no usable service profile found on the device — put it in pairing mode and retry"
	case has("br-connection-canceled", "le-connection-abort-by-local"):
		return "the connection attempt was canceled"
	case has("br-connection-", "le-connection-"):
		return "connection failed (" + message + ") — make sure the device is in pairing mode and in range"
	}
	return message
}
