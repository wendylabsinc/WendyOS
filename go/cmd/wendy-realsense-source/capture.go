package main

import (
	"context"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// CaptureOptions is the geometry asked for. Zero means the device's default:
// a RealSense advertises a handful of colour modes and picking one it does not
// have would fail at pipeline start with a librealsense message nobody can act
// on, so an unset field is left to the SDK.
type CaptureOptions struct {
	Source    string
	Width     uint32
	Height    uint32
	Framerate uint32
}

// capturer is the librealsense binding, behind an interface so this command's
// argument handling, its record framing and its shutdown behaviour are all
// testable on a machine with neither librealsense nor a camera.
type capturer interface {
	// Describe enumerates the attached cameras and what each can promise.
	Describe(ctx context.Context) ([]*agentpbv2.CalibratedSource, error)
	// Open starts a capture.
	Open(ctx context.Context, opts CaptureOptions) (capture, error)
	// Close releases the SDK context.
	Close() error
}

// capture is one running pipeline.
type capture interface {
	// Descriptor is what was actually negotiated — the resolution the device
	// agreed to and the properties this capture really carries, which is not
	// always what the listing promised.
	Descriptor() *agentpbv2.CalibratedSource
	// Next blocks for the next aligned frame set.
	Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error)
	Close() error
}
