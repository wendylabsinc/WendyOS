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
	// KeyState gives each sharing app's state, e.g. "cv-a (reading), cv-b (idle)".
	KeyState = "share_state"
)
