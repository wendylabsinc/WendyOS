//go:build !(linux && cgo && realsense)

package main

// The build without librealsense.
//
// The `realsense` build tag is deliberately opt-in: this binary is built and
// shipped only for images that carry librealsense2, and `go build ./...` on a
// developer machine or in PR CI must not need the C++ SDK to compile the
// package. What it must do is say so — a helper that started, produced nothing
// and exited 0 would be exactly the silent nothing this feature exists to
// remove, so the unsupported build refuses loudly instead.

import (
	"errors"
	"runtime"
)

// BuildTag is the tag that switches in the librealsense capture.
const BuildTag = "realsense"

func newCapturer() (capturer, error) {
	return nil, errors.New("this " + toolName + " was built without librealsense support" +
		" (GOOS=" + runtime.GOOS + ", tag `" + BuildTag + "` not set)." +
		" Rebuild it on a host with librealsense2 development headers:" +
		" CGO_ENABLED=1 go build -tags " + BuildTag + " ./cmd/" + toolName)
}
