package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
)

// reasonCameraInUse is the ErrorInfo reason for a camera another process holds AND that
// could not be read through the PipeWire graph either -- reaching it means sharing was
// unavailable, not that a camera inherently allows one consumer.
const reasonCameraInUse = streamreason.CameraInUse

// errCameraInUse reports contention as a FailedPrecondition: the device is
// healthy, its state is wrong, and the caller can retry once the holder stops.
// When the agent can tell what is likely holding the node, the message and
// the "hint" metadata say so -- see cameraHolderHint.
func errCameraInUse(devicePath string) error {
	msg := fmt.Sprintf("camera %s is already in use by another application on this device", devicePath)
	metadata := map[string]string{"device": devicePath}
	if hint := cameraHolderHint(devicePath); hint != "" {
		msg += ". " + hint
		metadata["hint"] = hint
	}
	return streamreason.New(codes.FailedPrecondition, msg, reasonCameraInUse, metadata)
}

// cameraHolderHint names what is likely holding a node the agent could not
// claim, when the agent can tell. A RealSense's colour node is claimed by the
// calibrated-frame helper (wendy-realsense-source) for as long as a
// StreamCalibratedFrames subscriber runs, and nothing in this service
// coordinates with it -- the two cannot share a RealSense yet (see §14 of
// specs/2026-09-20-calibrated-frame-sensor-source-design.md). "Another
// application" would send an operator looking for a process that is the
// agent's own, so the refusal names it. A seam so it can be answered without
// a sysfs.
var cameraHolderHint = func(devicePath string) string {
	name, err := os.ReadFile(filepath.Join("/sys/class/video4linux", filepath.Base(devicePath), "name"))
	if err != nil {
		return ""
	}
	if !strings.Contains(strings.ToLower(string(name)), "realsense") {
		return ""
	}
	return "This is a RealSense node: a calibrated frame stream (StreamCalibratedFrames, `wendy device camera frames`) " +
		"holds it for as long as it runs, and a RealSense cannot yet be shared between that stream and StreamVideo"
}

// isCameraInUse reports whether err is the contention error above.
func isCameraInUse(err error) bool {
	return streamreason.Has(err, reasonCameraInUse)
}

// isBusyErrno reports whether a syscall failed because the device is claimed.
func isBusyErrno(err error) bool {
	return errors.Is(err, unix.EBUSY)
}

// retryWhileBusy re-runs op briefly while it reports EBUSY, covering the gap between a dying
// producer's hub closing and its deferred fd close running. One retry only: getOrCreateHub
// does the substantive waiting, and against another application retrying cannot succeed.
func retryWhileBusy(ctx context.Context, op func() unix.Errno) unix.Errno {
	const (
		retries = 1
		delay   = 150 * time.Millisecond
	)
	errno := op()
	for i := 0; i < retries && isBusyErrno(errno); i++ {
		select {
		case <-ctx.Done():
			// Not the stale EBUSY: a shutdown mid-retry must not read as contention.
			return unix.ECANCELED
		case <-time.After(delay):
		}
		errno = op()
	}
	return errno
}

// busyPhrases are how the capture elements word contention. GStreamer reports it
// only as prose on stderr, and each library words it differently.
var busyPhrases = []string{
	"resource busy",               // kernel EBUSY text
	"is busy",                     // v4l2src: Device '/dev/video0' is busy
	"already in use",              // libcamera, Argus
	"failed to acquire camera",    // libcamera
	"cannot create camera device", // nvargus, sensor already claimed
}

// captureElements name the sources that read a camera. An encoder returns EBUSY in the
// same wording, so a busy phrase alone would blame the camera for a contended encoder.
var captureElements = []string{"v4l2src", "libcamerasrc", "nvarguscamerasrc", "pipewiresrc", "argus"}

// isBusyStderr reports whether captured stderr describes camera contention. Per error
// record: gst splits one failure across its "from element" line and the debug block below,
// while a warning it recovered from must not lend its element name to a later failure.
func isBusyStderr(stderr, devicePath string) bool {
	device := strings.ToLower(devicePath)
	for _, record := range errorRecords(strings.ToLower(stderr)) {
		// A warning gst recovered from did not kill the pipeline; only marked errors and
		// marker-less records (often the only evidence there is) can convict.
		if strings.HasPrefix(strings.TrimSpace(record), "warn") {
			continue
		}
		if !containsAny(record, busyPhrases) {
			continue
		}
		if namesDevice(record, device) || containsAny(record, captureElements) {
			return true
		}
	}
	return false
}

// exitedOnError reports whether the process failed by itself rather than being killed
// during teardown. streamGStreamer always signals the pipeline, so a non-nil Wait error
// proves nothing on its own.
func exitedOnError(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return !ws.Signaled() || ws.Signal() != syscall.SIGKILL
}

// errorRecords splits already-lowercased gst stderr at each error or warning marker,
// keeping a message and the debug block that explains it together as one unit.
//
// An unprefixed line joins the record before it: gst's "Additional debug info" block carries
// the decisive text and has no marker of its own.
func errorRecords(stderr string) []string {
	var records []string
	var cur strings.Builder
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "error") || strings.HasPrefix(trimmed, "warn") {
			if cur.Len() > 0 {
				records = append(records, cur.String())
				cur.Reset()
			}
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if cur.Len() > 0 {
		records = append(records, cur.String())
	}
	return records
}

// namesDevice reports whether s mentions exactly devicePath. A plain substring test
// would let /dev/video1 match /dev/video11 — on a Pi that is the H.264 encoder, so a
// contended encoder would be blamed on the camera.
func namesDevice(s, devicePath string) bool {
	if devicePath == "" {
		return false
	}
	for i := 0; i+len(devicePath) <= len(s); {
		j := strings.Index(s[i:], devicePath)
		if j < 0 {
			return false
		}
		end := i + j + len(devicePath)
		if end == len(s) || s[end] < '0' || s[end] > '9' {
			return true
		}
		i = end
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
