// Package streamreason holds the machine-readable reasons the agent attaches to camera and
// stream failures, plus the one place that builds and reads them. Spelling them out on both
// sides of the boundary let a rename silently degrade the CLI's diagnostic to a raw error.
package streamreason

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reasons carried in google.rpc.ErrorInfo. Renaming one is a breaking change for any client
// that switches on it, so treat these as the wire contract they are.
const (
	CameraInUse           = "CAMERA_IN_USE"
	IPCameraNoCredentials = "IP_CAMERA_NO_CREDENTIALS"
	TegraFirmwareMismatch = "TEGRA_FIRMWARE_MISMATCH"
	// RawUnavailable: StreamVideoRequest.raw was set for a camera that has no
	// uncompressed frames to give (MJPEG or native H.264 capture, a network
	// camera, one shared through PipeWire). The message names the cause.
	RawUnavailable = "RAW_UNAVAILABLE"
	// RequirementUnmet: a StreamCalibratedFrames subscriber declared a property
	// this source cannot provide -- or stopped being able to provide. Metadata
	// carries "source", "missing" (a comma-separated list of requirement slugs)
	// and one entry per slug saying why. Sent before the first frame, and again
	// mid-stream if a requirement stops holding: a consumer that silently loses
	// depth produces confident nonsense rather than an outage, which is the
	// whole reason this reason exists.
	RequirementUnmet = "REQUIREMENT_UNMET"
	// CalibratedSourceUnavailable: the named calibrated-frame source exists on
	// this device but cannot be opened -- most often a RealSense whose capture
	// helper is not installed on the image. Metadata carries "source" and,
	// where one applies, "helper" naming the missing executable.
	CalibratedSourceUnavailable = "CALIBRATED_SOURCE_UNAVAILABLE"
	// FrameTooLarge: one calibrated frame would exceed the receive limit the
	// client declared, so the stream is refused here rather than dying on the
	// first Recv. Metadata carries "frame_bytes" and "limit_bytes".
	FrameTooLarge = "FRAME_TOO_LARGE"
)

// Domain scopes the reasons to us, so a client can tell ours from a third party's.
const Domain = "wendy.dev"

// New returns an error carrying reason and metadata as an ErrorInfo detail. If the detail
// cannot be attached, the plain status still goes out: the operator keeps the message.
func New(code codes.Code, msg, reason string, metadata map[string]string) error {
	st := status.New(code, msg)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   Domain,
		Metadata: metadata,
	})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

// Info returns the ErrorInfo err carries, or nil.
func Info(err error) *errdetails.ErrorInfo {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	return nil
}

// Has reports whether err carries the given reason.
func Has(err error, reason string) bool {
	info := Info(err)
	return info != nil && info.GetReason() == reason
}
