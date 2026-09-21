package services

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
)

// Capture failures used to collapse into "GStreamer pipeline failed", which tells an
// operator nothing; the commonest cause is another process holding the camera.
func TestCameraInUseError_IsActionableAndMachineReadable(t *testing.T) {
	err := errCameraInUse("/dev/video0")

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status, got %v", err)
	}
	// FailedPrecondition, not Internal: the device is fine, its state is wrong.
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
	if !strings.Contains(st.Message(), "already in use") {
		t.Errorf("message must say what is wrong, got %q", st.Message())
	}

	info := streamreason.Info(err)
	if info == nil {
		t.Fatal("no ErrorInfo attached; clients cannot tell this apart from any other failure")
	}
	if info.GetReason() != reasonCameraInUse {
		t.Errorf("reason = %q, want %q", info.GetReason(), reasonCameraInUse)
	}
	// The device path is what the operator needs to find the process holding it.
	if got := info.GetMetadata()["device"]; got != "/dev/video0" {
		t.Errorf("metadata device = %q, want /dev/video0", got)
	}
}

func TestIsBusyErrno(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EBUSY", unix.EBUSY, true},
		{"wrapped EBUSY", errors.New("open: " + unix.EBUSY.Error()), false},
		{"EACCES", unix.EACCES, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBusyErrno(tt.err); got != tt.want {
				t.Errorf("isBusyErrno(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// GStreamer reports contention only in prose on stderr, and the wording differs
// per element. These are the strings observed from the elements the agent uses.
func TestIsBusyStderr(t *testing.T) {
	busy := []string{
		"ERROR: from element /GstPipeline:pipeline0/GstV4l2Src:v4l2src0: Device '/dev/video0' is busy",
		"v4l2src: Call to S_FMT failed for MJPG @ 640x480: Device or resource busy",
		"libcamerasrc: Failed to acquire camera",
		"nvarguscamerasrc: cannot create camera device",
		"/dev/video0 is already in use by another process",
	}
	for _, msg := range busy {
		if !isBusyStderr(msg, "/dev/video0") {
			t.Errorf("should be recognised as contention: %q", msg)
		}
	}

	notBusy := []string{
		"ERROR: could not link videoconvert0 to nvv4l2h264enc0",
		"WARNING: erroneous pipeline: no element \"pipewiresrc\"",
		"streaming stopped, reason not-negotiated (-4)",
		"",
	}
	for _, msg := range notBusy {
		if isBusyStderr(msg, "/dev/video0") {
			t.Errorf("must not be misreported as contention: %q", msg)
		}
	}
}

// An encoder can return EBUSY in the same words as the camera. Blaming the
// camera sends the operator hunting a process that does not hold it.
func TestIsBusyStderr_EncoderContentionIsNotTheCamera(t *testing.T) {
	msg := "ERROR: from element /GstPipeline:pipeline0/v4l2h264enc0: Device or resource busy"
	if isBusyStderr(msg, "/dev/video0") {
		t.Errorf("a busy encoder must not be reported as a busy camera: %q", msg)
	}
}

// On a Pi the H.264 encoder is /dev/video10-12, so a substring match on the
// camera path claims a busy encoder as a busy camera.
func TestIsBusyStderr_SiblingDeviceNodeIsNotTheCamera(t *testing.T) {
	msg := "ERROR: from element /GstPipeline:pipeline0/GstV4l2H264Enc:v4l2h264enc0: Device '/dev/video11' is busy"
	if isBusyStderr(msg, "/dev/video1") {
		t.Errorf("/dev/video11 must not match /dev/video1: %q", msg)
	}
	if !isBusyStderr("Device '/dev/video1' is busy", "/dev/video1") {
		t.Error("the camera's own node must still match")
	}
}

// streamGStreamer always signals the pipeline on the way out, so a non-nil Wait error
// cannot by itself tell a run that failed from one that was torn down.
func TestExitedOnError(t *testing.T) {
	if !exitedOnError(exec.Command("false").Run()) {
		t.Error("a process that exited non-zero by itself must count as failed")
	}
	if exitedOnError(exec.Command("true").Run()) {
		t.Error("a clean exit must not count as failed")
	}

	killed := exec.Command("sleep", "30")
	if err := killed.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := killed.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if exitedOnError(killed.Wait()) {
		t.Error("a pipeline killed during teardown must not count as failed")
	}
}

// GStreamer splits one failure across the "ERROR: from element" line and its
// "Additional debug info" block, so the phrase and the element land on different
// lines of the same record.
func TestIsBusyStderr_MatchesAcrossOneErrorRecord(t *testing.T) {
	msg := "ERROR: from element /GstPipeline:pipeline0/GstV4l2Src:v4l2src0: Could not read from resource.\n" +
		"Additional debug info:\n" +
		"gstv4l2bufferpool.c(848): gst_v4l2_buffer_pool_start (): /GstPipeline:pipeline0/GstV4l2Src:v4l2src0:\n" +
		"failed to activate bufferpool: Device or resource busy"
	if !isBusyStderr(msg, "/dev/video0") {
		t.Errorf("busy evidence in the debug block must still count: %q", msg)
	}
}

// The stderr buffer holds the whole run, so a busy warning gst recovered from at
// startup must not pair with a source element named much later.
func TestIsBusyStderr_MatchesWithinOneRecord(t *testing.T) {
	msg := "WARN: v4l2src0: reading modes\n" +
		"ERROR: from element /GstPipeline:pipeline0/nvv4l2h264enc0: Device or resource busy"
	if isBusyStderr(msg, "/dev/video0") {
		t.Errorf("a busy encoder must not borrow a capture element from another line: %q", msg)
	}
}

// Matching is case-insensitive because the wording comes from several different
// libraries, each with its own capitalisation.
func TestIsBusyStderr_CaseInsensitive(t *testing.T) {
	if !isBusyStderr("DEVICE '/DEV/VIDEO0' IS BUSY", "/dev/video0") {
		t.Error("uppercase stderr must still be recognised")
	}
}

func TestIsCameraInUse(t *testing.T) {
	if !isCameraInUse(errCameraInUse("/dev/video0")) {
		t.Error("the contention error must be recognised by its reason")
	}
	for _, err := range []error{nil, errors.New("plain"), status.Error(codes.Internal, "other")} {
		if isCameraInUse(err) {
			t.Errorf("must not match: %v", err)
		}
	}
}

// Which ioctl refuses a second consumer is driver- and timing-dependent, so every setup site
// must reach CAMERA_IN_USE, not just S_FMT.
func TestErrCaptureSetup_ClassifiesBusyAtEverySite(t *testing.T) {
	svc := NewVideoService(context.Background(), zap.NewNop(), nil)
	for _, ioctl := range []string{
		"VIDIOC_REQBUFS", "VIDIOC_QUERYBUF", "VIDIOC_QBUF", "VIDIOC_STREAMON", "VIDIOC_DQBUF",
	} {
		if err := svc.errCaptureSetup(ioctl, "/dev/video0", unix.EBUSY); !isCameraInUse(err) {
			t.Errorf("%s: EBUSY must be reported as CAMERA_IN_USE, got %v", ioctl, err)
		}
	}
}

func TestErrCaptureSetup_OtherErrnosStayInternal(t *testing.T) {
	svc := NewVideoService(context.Background(), zap.NewNop(), nil)
	for _, errno := range []unix.Errno{unix.EINVAL, unix.ENOMEM, unix.ENODEV} {
		err := svc.errCaptureSetup("VIDIOC_REQBUFS", "/dev/video0", errno)
		if isCameraInUse(err) {
			t.Errorf("%v must not be reported as contention", errno)
		}
		if got := status.Code(err); got != codes.Internal {
			t.Errorf("%v: want codes.Internal, got %v", errno, got)
		}
		// The errno must not reach the client: it lets a caller enumerate the system.
		if strings.Contains(err.Error(), errno.Error()) {
			t.Errorf("%v: errno text leaked into the response: %q", errno, err.Error())
		}
	}
}

// gst prints "Device or resource busy" while probing modes and carries on. Convicting on
// that sends the operator to stop an application that holds nothing.
func TestIsBusyStderr_RecoveredWarningIsNotContention(t *testing.T) {
	msg := "WARNING: from element /GstPipeline:pipeline0/GstV4l2Src:v4l2src0: Cannot set S_PARM: Device or resource busy\n" +
		"Additional debug info:\n" +
		"gstv4l2object.c(1234): gst_v4l2_object_set_format ():\n" +
		"ERROR: from element /GstPipeline:pipeline0/x264enc0: Could not negotiate format\n" +
		"Additional debug info:\n" +
		"gstvideoencoder.c(999): failed to negotiate"
	if isBusyStderr(msg, "/dev/video0") {
		t.Errorf("an encoder failure must not be reported as camera contention: %q", msg)
	}
}

// The counterpart: a genuine busy ERROR must still be recognised, including when the
// evidence sits in the debug block rather than the message line.
func TestIsBusyStderr_FatalBusyStillCounts(t *testing.T) {
	msg := "WARNING: from element /GstPipeline:pipeline0/GstV4l2Src:v4l2src0: Cannot set S_PARM: Device or resource busy\n" +
		"ERROR: from element /GstPipeline:pipeline0/GstV4l2Src:v4l2src0: Could not read from resource.\n" +
		"Additional debug info:\n" +
		"gstv4l2bufferpool.c(848): failed to activate bufferpool: Device or resource busy"
	if !isBusyStderr(msg, "/dev/video0") {
		t.Errorf("a fatal busy record must still be recognised: %q", msg)
	}
}

// The loop must run exactly one retry, and a cancellation must not be reported as EBUSY --
// otherwise a shutdown landing mid-retry manufactures a CAMERA_IN_USE verdict.
func TestRetryWhileBusy(t *testing.T) {
	calls := 0
	if got := retryWhileBusy(context.Background(), func() unix.Errno {
		calls++
		return unix.EBUSY
	}); got != unix.EBUSY {
		t.Errorf("want EBUSY passed through, got %v", got)
	}
	if calls != 2 {
		t.Errorf("want one initial call plus one retry, got %d calls", calls)
	}

	calls = 0
	if got := retryWhileBusy(context.Background(), func() unix.Errno {
		calls++
		return 0
	}); got != 0 || calls != 1 {
		t.Errorf("a free device must cost exactly one ioctl: errno=%v calls=%d", got, calls)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := retryWhileBusy(cancelled, func() unix.Errno { return unix.EBUSY }); isBusyErrno(got) {
		t.Errorf("a cancelled context must not report contention, got %v", got)
	}
}
