//go:build !linux

package services

import "context"

// captureDepthFramesNative has no V4L2 to read outside Linux.
func captureDepthFramesNative(context.Context, *VideoService, string, uint32, uint32, func(uint32), func([]byte) bool) error {
	return errDepthCaptureLinuxOnly
}
