// Package camerashare names the hardware-capability properties through which
// the agent reports camera sharing, so the agent and the CLI agree on them.
package camerashare

const (
	// KeyMode is how a shared camera is captured, e.g. "1280x720 YUYV, 30 fps".
	KeyMode = "share_mode"
	// KeyUnavailable is the reason a camera is not shared.
	KeyUnavailable = "share_unavailable"
	// KeyWith lists the apps sharing the camera, e.g. "cv-a, cv-b".
	KeyWith = "shared_with"
	// KeyState gives each sharing app's node settings and state, e.g.
	// "cv-a [format pixels, stamp time] (reading), cv-b [format native, stamp frame] (idle)".
	KeyState = "share_state"
)

// KeyModeOfFormat is the mode of the node an app gets for the camera when its
// entitlement asks for a format other than the default, e.g.
// "share_mode_native". The default format's mode is under KeyMode.
func KeyModeOfFormat(format string) string { return KeyMode + "_" + format }

// KeyUnavailableOfFormat is why a shared camera cannot be declared in a
// format, e.g. "share_unavailable_native". An app that asks for that format
// gets a node of another format instead (appconfig.CameraFormatsServedFor).
func KeyUnavailableOfFormat(format string) string { return KeyUnavailable + "_" + format }
