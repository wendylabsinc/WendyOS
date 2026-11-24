// Package camerashare names the hardware-capability properties through which
// the agent reports camera sharing, so the agent and the CLI agree on them.
package camerashare

import "strings"

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
	// KeyStableID is the camera's stable id as the camera list gives it, e.g.
	// "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0", and empty for a camera
	// without one. An entitlement's allowlist may name the camera by it. An
	// agent that matches allowlist entries by stable id and by "v4l2:" path
	// sets the key on the row of every camera in its video listing, and an
	// agent from before that on none. A row for a node the listing leaves out,
	// such as a camera's metadata node, never has it.
	KeyStableID = "stable_id"
	// KeyH264Encoder names the element that makes the camera's H.264 on this
	// device and what kind it is, e.g. "x264enc (software)". Only a camera
	// whose H.264 the device encodes in software has it: a camera that
	// produces H.264 itself, a device with a hardware encoder and an agent
	// from before the key set none.
	KeyH264Encoder = "h264_encoder"
)

const softwareEncoderSuffix = " (software)"

// SoftwareEncoder is the KeyH264Encoder value for a software element.
func SoftwareEncoder(element string) string { return element + softwareEncoderSuffix }

// SoftwareEncoderOf takes the element out of a KeyH264Encoder value, and
// reports false for a value that does not name a software encoder.
func SoftwareEncoderOf(value string) (element string, ok bool) {
	element, ok = strings.CutSuffix(value, softwareEncoderSuffix)
	return element, ok && element != ""
}

// KeyModeOfFormat is the mode of the node an app gets for the camera when its
// entitlement asks for a format other than the default, e.g.
// "share_mode_native". The default format's mode is under KeyMode.
func KeyModeOfFormat(format string) string { return KeyMode + "_" + format }

// KeyUnavailableOfFormat is why a shared camera cannot be declared in a
// format, e.g. "share_unavailable_native". An app that asks for that format
// gets a node of another format instead (appconfig.CameraFormatsServedFor).
func KeyUnavailableOfFormat(format string) string { return KeyUnavailable + "_" + format }
