package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
)

// Annex-B NAL header bytes (forbidden_zero | nal_ref_idc | nal_unit_type).
const (
	nalHdrSPS = 0x67 // sequence parameter set (type 7)
	nalHdrPPS = 0x68 // picture parameter set (type 8)
	nalHdrIDR = 0x65 // IDR slice (type 5) — a keyframe
	nalHdrP   = 0x41 // non-IDR slice (type 1)
)

// h264NAL builds an Annex-B NAL unit: a 4-byte start code, a header byte, then payload.
func h264NAL(header byte, payload ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x00, 0x01, header}, payload...)
}

// joinBytes concatenates byte slices.
func joinBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type mockVideoStream struct {
	frames      []*agentpb.VideoFrame
	idx         int
	err         error
	errReturned bool
}

func (m *mockVideoStream) Recv() (*agentpb.VideoFrame, error) {
	if m.err != nil && !m.errReturned {
		m.errReturned = true
		return nil, m.err
	}
	if m.idx >= len(m.frames) {
		return nil, io.EOF
	}
	f := m.frames[m.idx]
	m.idx++
	return f, nil
}

func TestPipeVideoToStdout_WritesAllFrames(t *testing.T) {
	stream := &mockVideoStream{
		frames: []*agentpb.VideoFrame{
			{Data: []byte{0x00, 0x00, 0x00, 0x01}},
			{Data: []byte{0x41, 0x42, 0x43}},
		},
	}
	var buf bytes.Buffer
	if err := pipeVideoToStdout(stream, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x42, 0x43}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("got %v, want %v", buf.Bytes(), expected)
	}
}

func TestPipeVideoToStdout_EmptyStream(t *testing.T) {
	stream := &mockVideoStream{}
	var buf bytes.Buffer
	if err := pipeVideoToStdout(stream, &buf); err != nil {
		t.Fatalf("unexpected error for empty stream: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output, got %d bytes", buf.Len())
	}
}

func TestPlaybackPipelineArgs_H264UsesTypefindNotBareCaps(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_H264)
	joined := strings.Join(args, " ")

	// Regression: a bare "video/x-h264" capsfilter directly after fdsrc cannot
	// fixate caps onto fdsrc's untyped buffers and fails to preroll with
	// "Output caps are unfixed". typefind must classify the stream instead.
	if !strings.Contains(joined, "fdsrc fd=0 ! typefind ! h264parse") {
		t.Errorf("H264 pipeline must route fdsrc through typefind into h264parse, got: %v", args)
	}
	if strings.Contains(joined, "! video/x-h264 !") {
		t.Errorf("H264 pipeline must not use a bare video/x-h264 capsfilter after fdsrc, got: %v", args)
	}
}

func TestPlaybackPipelineArgs_VP8UsesMatroskademux(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_VP8)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "fdsrc fd=0 ! matroskademux") {
		t.Errorf("VP8 pipeline must demux the WebM container via matroskademux, got: %v", args)
	}
	if !strings.Contains(joined, "! vp8dec !") {
		t.Errorf("VP8 pipeline must decode via vp8dec, got: %v", args)
	}
}

func TestPlaybackPipelineArgs_H264DecodesSingleThreaded(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_H264)
	joined := strings.Join(args, " ")
	// Frame-based multithreading in avdec_h264 delays output by ~thread-count
	// frames; max-threads=1 removes that constant latency.
	if !strings.Contains(joined, "avdec_h264 max-threads=1") {
		t.Errorf("H264 pipeline must decode single-threaded to avoid frame-threading latency, got: %v", args)
	}
}

func TestPlaybackPipelineArgs_H264LeakyQueueBeforeDecoder(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_H264)
	joined := strings.Join(args, " ")
	// A leaky queue between h264parse and the decoder drops whole access units
	// when decode falls behind, so an encoded-side backlog drains by dropping
	// rather than playing through.
	if !strings.Contains(joined, "h264parse ! queue max-size-buffers=2 leaky=downstream ! avdec_h264") {
		t.Errorf("H264 pipeline must have a leaky queue before the decoder, got: %v", args)
	}
}

func TestPlaybackPipelineArgs_VP8LeakyQueueBeforeDecoder(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_VP8)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "matroskademux ! queue max-size-buffers=2 leaky=downstream ! vp8dec") {
		t.Errorf("VP8 pipeline must have a leaky queue before the decoder, got: %v", args)
	}
}

func TestPlayVideoWithGStreamer_MissingGStreamer(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no executables on PATH
	stubGSTFallback(t, nil)       // no install-location fallbacks either
	stubNonInteractive(t)         // no install prompt for the missing binary

	stream := &mockVideoStream{frames: []*agentpb.VideoFrame{{Data: []byte{0x00}, Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}}}
	err := playVideoWithGStreamer(context.Background(), stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestPlayVideoWithGStreamer_RemoteStreamErrorPrecedesMissingGStreamer(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no executables on PATH
	stubGSTFallback(t, nil)       // no install-location fallbacks either
	stubNonInteractive(t)         // no install prompt for the missing binary

	remoteErr := status.Error(codes.Unimplemented, "Camera streaming is currently not supported by Wendy Agent for Mac.")
	stream := &mockVideoStream{err: remoteErr}
	err := playVideoWithGStreamer(context.Background(), stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Camera streaming is currently not supported by Wendy Agent for Mac.") {
		t.Fatalf("expected remote unsupported error, got: %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("remote unsupported error should not be masked by missing GStreamer: %v", err)
	}
}

func firmwareMismatchError(t *testing.T) error {
	t.Helper()
	st := status.New(codes.FailedPrecondition, "firmware mismatch")
	with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: streamreason.TegraFirmwareMismatch, Metadata: map[string]string{
		"rootfs_l4t": "R38.2.0", "boot_firmware_l4t": "R36.4.3",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return with.Err()
}

func TestCameraFirmwareDiagnostic(t *testing.T) {
	err := cameraStreamDiagnostic(firmwareMismatchError(t))
	for _, want := range []string{"R38.2.0", "R36.4.3", "wendy os install", "do not use --rootfs-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q missing %q", err, want)
		}
	}
}

func TestCameraFirmwareDiagnosticOnFirstRecv(t *testing.T) {
	stream := &cameraDiagnosticStream{videoStream: &mockVideoStream{err: firmwareMismatchError(t)}}
	_, err := stream.Recv()
	if err == nil || !strings.Contains(err.Error(), "full USB recovery") {
		t.Fatalf("first Recv diagnostic = %v", err)
	}
}

// fakeCameraTester is a stub cameraTester: tests set resp or err directly
// rather than driving a real gRPC connection.
type fakeCameraTester struct {
	resp *agentpb.TestCameraCredentialsResponse
	err  error
}

func (f *fakeCameraTester) TestCameraCredentials(context.Context, *agentpb.TestCameraCredentialsRequest, ...grpc.CallOption) (*agentpb.TestCameraCredentialsResponse, error) {
	return f.resp, f.err
}

func TestRunCameraTest_OKOutput(t *testing.T) {
	client := &fakeCameraTester{resp: &agentpb.TestCameraCredentialsResponse{
		Result:  agentpb.TestCameraCredentialsResponse_RESULT_OK,
		Address: "10.98.0.50",
		Detail:  "camera 200 at 10.98.0.50 accepted the credentials",
	}}
	var buf bytes.Buffer
	if err := runCameraTest(context.Background(), client, 200, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "credentials accepted") {
		t.Fatalf("output = %q, want it to report accepted credentials", buf.String())
	}
}

func TestRunCameraTest_OKShowsDetail(t *testing.T) {
	client := &fakeCameraTester{resp: &agentpb.TestCameraCredentialsResponse{
		Result:  agentpb.TestCameraCredentialsResponse_RESULT_OK,
		Address: "10.98.0.50",
		Detail:  "camera 200 at 10.98.0.50 did not request authentication; the stream path returned RTSP 404 and may be wrong",
	}}
	var buf bytes.Buffer
	if err := runCameraTest(context.Background(), client, 200, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "did not request authentication") {
		t.Fatalf("output = %q, want the probe's detail surfaced on success", buf.String())
	}
}

func TestRunCameraTest_OKNoDetailOmitsColon(t *testing.T) {
	client := &fakeCameraTester{resp: &agentpb.TestCameraCredentialsResponse{
		Result:  agentpb.TestCameraCredentialsResponse_RESULT_OK,
		Address: "10.98.0.50",
	}}
	var buf bytes.Buffer
	if err := runCameraTest(context.Background(), client, 200, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := buf.String(), "Camera 200: credentials accepted (10.98.0.50).\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunCameraTest_AuthFailedNamesLoginFix(t *testing.T) {
	client := &fakeCameraTester{resp: &agentpb.TestCameraCredentialsResponse{
		Result:  agentpb.TestCameraCredentialsResponse_RESULT_AUTH_FAILED,
		Address: "10.98.0.50",
		Detail:  "camera 200 at 10.98.0.50 rejected the credentials (RTSP 401)",
	}}
	var buf bytes.Buffer
	err := runCameraTest(context.Background(), client, 200, &buf)
	if err == nil {
		t.Fatal("expected an error for rejected credentials")
	}
	if !strings.Contains(err.Error(), "wendy device camera login 200") {
		t.Fatalf("error = %v, want it to name the login fix", err)
	}
}

// The agent returns "no stored credentials" as a gRPC error (FailedPrecondition
// + ErrorInfo{IP_CAMERA_NO_CREDENTIALS}), the same as StreamVideo. runCameraTest
// must route it through the existing cameraStreamDiagnostic so it prints the
// established login hint with zero new mapping.
func TestRunCameraTest_NoCredentialsGetsLoginHint(t *testing.T) {
	st := status.New(codes.FailedPrecondition, "camera 200 has no stored credentials")
	with, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   "IP_CAMERA_NO_CREDENTIALS",
		Metadata: map[string]string{"device_id": "200"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeCameraTester{err: with.Err()}
	var buf bytes.Buffer
	testErr := runCameraTest(context.Background(), client, 200, &buf)
	if testErr == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(testErr.Error(), "wendy device camera login 200") {
		t.Fatalf("error = %v, want the established login hint", testErr)
	}
}

func TestRunCameraTest_UnreachableShowsDetail(t *testing.T) {
	client := &fakeCameraTester{resp: &agentpb.TestCameraCredentialsResponse{
		Result:  agentpb.TestCameraCredentialsResponse_RESULT_UNREACHABLE,
		Address: "10.98.0.50",
		Detail:  "camera 200 at 10.98.0.50 is unreachable",
	}}
	var buf bytes.Buffer
	err := runCameraTest(context.Background(), client, 200, &buf)
	if err == nil {
		t.Fatal("expected an error for an unreachable camera")
	}
	if !strings.Contains(err.Error(), "camera 200 at 10.98.0.50 is unreachable") {
		t.Fatalf("error = %v, want the probe's detail", err)
	}
}

// stubGSTFallback overrides the platform-specific fallback path list for the
// duration of the test.
func stubGSTFallback(t *testing.T, paths []string) {
	t.Helper()
	prev := gstLaunchFallbackPathsFn
	gstLaunchFallbackPathsFn = func() []string { return paths }
	t.Cleanup(func() { gstLaunchFallbackPathsFn = prev })
}

// stubNonInteractive forces isInteractiveTerminalFn to report a non-TTY
// session for the duration of the test, so tests exercising a missing
// gst-launch-1.0 never trigger the macOS install prompt.
func stubNonInteractive(t *testing.T) {
	t.Helper()
	prev := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = prev })
}

func TestEnsureGSTLaunch_FoundSkipsPrompt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, gstLaunchName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	stubGSTFallback(t, []string{bin})
	stubConfirmFn(t, func(string) bool {
		t.Fatal("must not prompt when gst-launch-1.0 is already resolvable")
		return false
	})

	got, err := ensureGSTLaunchForHostOS(context.Background(), "darwin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

func TestEnsureGSTLaunch_NonDarwinSkipsPrompt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubConfirmFn(t, func(string) bool {
		t.Fatal("must not prompt on a non-macOS host")
		return false
	})

	if _, err := ensureGSTLaunchForHostOS(context.Background(), "linux"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestEnsureGSTLaunch_NonInteractiveSkipsPrompt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubNonInteractive(t)
	stubConfirmFn(t, func(string) bool {
		t.Fatal("must not prompt in a non-interactive session")
		return false
	})

	if _, err := ensureGSTLaunchForHostOS(context.Background(), "darwin"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestEnsureGSTLaunch_NoBrewSkipsPrompt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubInteractive(t)
	stubBrewLookPath(t, func(string) (string, error) { return "", errors.New("brew not found") })
	stubConfirmFn(t, func(string) bool {
		t.Fatal("must not prompt when Homebrew is not installed")
		return false
	})

	if _, err := ensureGSTLaunchForHostOS(context.Background(), "darwin"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestEnsureGSTLaunch_DeclinedInstallReturnsNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubInteractive(t)
	stubBrewLookPath(t, func(string) (string, error) { return "/opt/homebrew/bin/brew", nil })
	stubConfirmFn(t, func(string) bool { return false })
	stubInstallGStreamer(t, func(context.Context) error {
		t.Fatal("must not install when the user declines")
		return nil
	})

	if _, err := ensureGSTLaunchForHostOS(context.Background(), "darwin"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestEnsureGSTLaunch_AcceptedInstallResolvesAfterwards(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, gstLaunchName)
	stubGSTFallback(t, []string{bin})
	stubInteractive(t)
	stubBrewLookPath(t, func(string) (string, error) { return "/opt/homebrew/bin/brew", nil })
	stubConfirmFn(t, func(string) bool { return true })
	stubInstallGStreamer(t, func(context.Context) error {
		// Simulate a successful `brew install gstreamer` placing the binary.
		return os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	})

	got, err := ensureGSTLaunchForHostOS(context.Background(), "darwin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

func TestEnsureGSTLaunch_InstallFailureIsWrapped(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubGSTFallback(t, nil)
	stubInteractive(t)
	stubBrewLookPath(t, func(string) (string, error) { return "/opt/homebrew/bin/brew", nil })
	stubConfirmFn(t, func(string) bool { return true })
	stubInstallGStreamer(t, func(context.Context) error { return errors.New("brew install failed") })

	_, err := ensureGSTLaunchForHostOS(context.Background(), "darwin")
	if err == nil || !strings.Contains(err.Error(), "installing GStreamer via Homebrew") {
		t.Fatalf("expected wrapped install error, got: %v", err)
	}
}

// stubInteractive forces isInteractiveTerminalFn to report an interactive TTY
// session for the duration of the test.
func stubInteractive(t *testing.T) {
	t.Helper()
	prev := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveTerminalFn = prev })
}

func stubConfirmFn(t *testing.T, fn func(string) bool) {
	t.Helper()
	prev := confirmFn
	confirmFn = fn
	t.Cleanup(func() { confirmFn = prev })
}

func stubBrewLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := brewLookPathFn
	brewLookPathFn = fn
	t.Cleanup(func() { brewLookPathFn = prev })
}

func stubInstallGStreamer(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	prev := installGStreamerFn
	installGStreamerFn = fn
	t.Cleanup(func() { installGStreamerFn = prev })
}

func TestResolveGSTLaunch_FoundViaFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // not on PATH

	dir := t.TempDir()
	bin := filepath.Join(dir, gstLaunchName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	stubGSTFallback(t, []string{filepath.Join(dir, "missing"), bin})

	got, err := resolveGSTLaunch()
	if err != nil {
		t.Fatalf("expected to resolve via fallback, got error: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

func TestResolveGSTLaunch_IgnoresDirectories(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	dir := t.TempDir()
	// A directory named like the binary must not be treated as a match.
	if err := os.Mkdir(filepath.Join(dir, gstLaunchName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stubGSTFallback(t, []string{filepath.Join(dir, gstLaunchName)})

	if _, err := resolveGSTLaunch(); err == nil {
		t.Fatal("expected error when only a directory matches, got nil")
	}
}

func TestLastKeyframeOffset_NoKeyframe(t *testing.T) {
	data := joinBytes(h264NAL(nalHdrP, 0xAA), h264NAL(nalHdrP, 0xBB))
	if off, ok := lastKeyframeOffset(data); ok {
		t.Errorf("expected no keyframe in a P-slice-only stream, got offset %d", off)
	}
}

func TestLastKeyframeOffset_SPSBeforeIDR(t *testing.T) {
	data := joinBytes(h264NAL(nalHdrSPS, 0x01), h264NAL(nalHdrPPS, 0x02), h264NAL(nalHdrIDR, 0x03))
	off, ok := lastKeyframeOffset(data)
	if !ok || off != 0 {
		t.Errorf("keyframe should start at the SPS (offset 0), got off=%d ok=%v", off, ok)
	}
}

func TestLastKeyframeOffset_PicksMostRecent(t *testing.T) {
	leading := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR), h264NAL(nalHdrP))
	data := joinBytes(leading, h264NAL(nalHdrSPS), h264NAL(nalHdrPPS), h264NAL(nalHdrIDR))
	off, ok := lastKeyframeOffset(data)
	if !ok || off != len(leading) {
		t.Errorf("expected the most recent keyframe at offset %d, got off=%d ok=%v", len(leading), off, ok)
	}
}

func TestLastKeyframeOffset_IDRWithoutSPS(t *testing.T) {
	leading := h264NAL(nalHdrP)
	data := joinBytes(leading, h264NAL(nalHdrIDR))
	off, ok := lastKeyframeOffset(data)
	if !ok || off != len(leading) {
		t.Errorf("a bare IDR should be reported at its own start code (offset %d), got off=%d ok=%v", len(leading), off, ok)
	}
}

func TestLastKeyframeOffset_ThreeByteStartCode(t *testing.T) {
	data := []byte{0x00, 0x00, 0x01, nalHdrSPS, 0x00, 0x00, 0x01, nalHdrIDR}
	off, ok := lastKeyframeOffset(data)
	if !ok || off != 0 {
		t.Errorf("expected keyframe at offset 0 with 3-byte start codes, got off=%d ok=%v", off, ok)
	}
}

func TestLastKeyframeOffset_Empty(t *testing.T) {
	if _, ok := lastKeyframeOffset(nil); ok {
		t.Error("expected no keyframe in empty data")
	}
}

func TestLastKeyframeOffset_MultiSliceKeyframe(t *testing.T) {
	// A keyframe picture is often coded as several IDR slices (e.g. x264enc
	// tune=zerolatency uses sliced threads). They are one access unit, which
	// begins at the SPS — not at the last slice.
	data := joinBytes(
		h264NAL(nalHdrSPS), h264NAL(nalHdrPPS),
		h264NAL(nalHdrIDR, 0x01), h264NAL(nalHdrIDR, 0x02), h264NAL(nalHdrIDR, 0x03),
	)
	off, ok := lastKeyframeOffset(data)
	if !ok || off != 0 {
		t.Errorf("multi-slice keyframe must start at the SPS (offset 0), got off=%d ok=%v", off, ok)
	}
}

func TestLastKeyframeOffset_PicksMostRecentMultiSlice(t *testing.T) {
	leading := joinBytes(
		h264NAL(nalHdrSPS), h264NAL(nalHdrPPS),
		h264NAL(nalHdrIDR, 0x01), h264NAL(nalHdrIDR, 0x02),
	)
	data := joinBytes(leading,
		h264NAL(nalHdrSPS), h264NAL(nalHdrPPS),
		h264NAL(nalHdrIDR, 0x03), h264NAL(nalHdrIDR, 0x04),
	)
	off, ok := lastKeyframeOffset(data)
	if !ok || off != len(leading) {
		t.Errorf("expected the second multi-slice keyframe at offset %d, got off=%d ok=%v", len(leading), off, ok)
	}
}

func TestLastKeyframeOffset_MultiSliceIDRWithoutSPS(t *testing.T) {
	// IDR slices with no SPS in this data: the keyframe begins at the first
	// slice, not the last.
	leading := h264NAL(nalHdrP, 0x09)
	data := joinBytes(leading, h264NAL(nalHdrIDR, 0x01), h264NAL(nalHdrIDR, 0x02))
	off, ok := lastKeyframeOffset(data)
	if !ok || off != len(leading) {
		t.Errorf("expected the keyframe at the first IDR slice (offset %d), got off=%d ok=%v", len(leading), off, ok)
	}
}

func TestH264FeedBuffer_RetainsSPSofMultiSliceKeyframes(t *testing.T) {
	feed := newH264FeedBuffer()
	keyframe := func(tag byte) []byte {
		return joinBytes(
			h264NAL(nalHdrSPS), h264NAL(nalHdrPPS),
			h264NAL(nalHdrIDR, tag, 0x01), h264NAL(nalHdrIDR, tag, 0x02),
		)
	}
	// Writer is behind: two multi-slice keyframes pile up before a take.
	feed.push(keyframe(0xA1))
	feed.push(keyframe(0xB2))

	data, _, _ := feed.take(context.Background())
	if len(data) < 5 || data[4]&0x1F != h264NalTypeSPS {
		t.Fatalf("taken bytes must start with an SPS, not a bare IDR slice")
	}
	if off, ok := lastKeyframeOffset(data); !ok || off != 0 {
		t.Errorf("taken bytes must begin at a keyframe, got off=%d ok=%v", off, ok)
	}
}

func TestH264FeedBuffer_PushTakeRoundTrips(t *testing.T) {
	feed := newH264FeedBuffer()
	idr := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR, 0x11))
	feed.push(idr)

	data, err, done := feed.take(context.Background())
	if err != nil || done {
		t.Fatalf("take: err=%v done=%v", err, done)
	}
	if !bytes.Equal(data, idr) {
		t.Errorf("take returned %v, want %v", data, idr)
	}
}

func TestH264FeedBuffer_DropsBacklogAheadOfKeyframe(t *testing.T) {
	feed := newH264FeedBuffer()
	idr1 := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR, 0x11))
	p1 := h264NAL(nalHdrP, 0x22)
	idr2 := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR, 0x33))
	p2 := h264NAL(nalHdrP, 0x44)

	// The writer never takes between pushes — gst-launch still starting up.
	feed.push(idr1)
	feed.push(p1)
	feed.push(idr2)
	feed.push(p2)

	data, _, _ := feed.take(context.Background())
	want := joinBytes(idr2, p2)
	if !bytes.Equal(data, want) {
		t.Errorf("take returned %v, want %v (backlog before the latest keyframe dropped)", data, want)
	}
}

func TestH264FeedBuffer_KeepsContiguousBytesWhenWriterKeepsUp(t *testing.T) {
	feed := newH264FeedBuffer()
	idr := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR))
	p1 := h264NAL(nalHdrP, 0x01)
	p2 := h264NAL(nalHdrP, 0x02)

	feed.push(idr)
	if d, _, _ := feed.take(context.Background()); !bytes.Equal(d, idr) {
		t.Fatalf("first take = %v, want %v", d, idr)
	}
	// P-frames with no newer keyframe must not be dropped when taken promptly.
	feed.push(p1)
	if d, _, _ := feed.take(context.Background()); !bytes.Equal(d, p1) {
		t.Errorf("take = %v, want %v", d, p1)
	}
	feed.push(p2)
	if d, _, _ := feed.take(context.Background()); !bytes.Equal(d, p2) {
		t.Errorf("take = %v, want %v", d, p2)
	}
}

func TestH264FeedBuffer_TakeDrainsBufferedBytesBeforeReportingEnd(t *testing.T) {
	feed := newH264FeedBuffer()
	idr := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR))
	feed.push(idr)
	feed.close(io.EOF)

	data, _, done := feed.take(context.Background())
	if done || !bytes.Equal(data, idr) {
		t.Fatalf("first take = %v done=%v, want the buffered bytes", data, done)
	}
	if _, _, done = feed.take(context.Background()); !done {
		t.Error("expected done on the second take")
	}
}

func TestH264FeedBuffer_TakePropagatesError(t *testing.T) {
	feed := newH264FeedBuffer()
	feed.close(errors.New("boom"))

	_, err, done := feed.take(context.Background())
	if !done || err == nil {
		t.Fatalf("expected a terminal error, got err=%v done=%v", err, done)
	}
}

func TestH264FeedBuffer_TakeUnblocksOnContextCancel(t *testing.T) {
	feed := newH264FeedBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, done := feed.take(ctx); !done {
		t.Error("expected take to report done when the context is cancelled")
	}
}

func TestH264FeedBuffer_TakeBlocksUntilPush(t *testing.T) {
	feed := newH264FeedBuffer()
	idr := joinBytes(h264NAL(nalHdrSPS), h264NAL(nalHdrIDR))
	go feed.push(idr)

	data, err, done := feed.take(context.Background())
	if err != nil || done {
		t.Fatalf("take: err=%v done=%v", err, done)
	}
	if !bytes.Equal(data, idr) {
		t.Errorf("take = %v, want %v", data, idr)
	}
}

// producerRestartedError is the status the agent returns to a parameter-less
// viewer whose camera an episode capture took over.
func producerRestartedError(t *testing.T) error {
	t.Helper()
	return streamreason.New(codes.Unavailable,
		"video stream ended: episode capture restarted the camera producer at the campaign's requested parameters; reconnect to join the new stream",
		streamreason.CameraProducerRestarted, nil)
}

// Capture takeover ends a parameter-less viewer's stream on purpose: a new
// producer means a new sequence parameter set, and splicing both into one
// timeline would corrupt the decode. `camera view` is parameter-less by
// default, so it must rejoin rather than leave the operator with a dead
// window.
func TestCameraViewRejoinsAfterAProducerRestart(t *testing.T) {
	opens := 0
	open := func() (videoStream, error) {
		opens++
		if opens == 1 {
			return &mockVideoStream{err: producerRestartedError(t)}, nil
		}
		return &mockVideoStream{frames: []*agentpb.VideoFrame{{Data: []byte("frame")}}}, nil
	}
	var played [][]byte
	play := func(stream videoStream) error {
		for {
			frame, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				// The real playback paths wrap the receive error; the rejoin
				// decision must see through that wrapping.
				return fmt.Errorf("receiving video: %w", err)
			}
			played = append(played, frame.GetData())
		}
	}

	if err := streamCameraRejoiningRestarts(context.Background(), open, play); err != nil {
		t.Fatalf("streamCameraRejoiningRestarts: %v", err)
	}
	if opens != 2 {
		t.Fatalf("streams opened = %d, want 2 (the original and the rejoin)", opens)
	}
	if len(played) != 1 || string(played[0]) != "frame" {
		t.Fatalf("frames played = %q, want the one frame from the replacement stream", played)
	}
}

// Any other error ends the viewer as before: only the restart is recoverable.
func TestCameraViewDoesNotRejoinOnOtherErrors(t *testing.T) {
	opens := 0
	open := func() (videoStream, error) {
		opens++
		return &mockVideoStream{err: status.Error(codes.Unavailable, "device gone")}, nil
	}
	play := func(stream videoStream) error {
		_, err := stream.Recv()
		return err
	}
	if err := streamCameraRejoiningRestarts(context.Background(), open, play); err == nil {
		t.Fatal("a plain Unavailable was swallowed")
	}
	if opens != 1 {
		t.Fatalf("streams opened = %d, want 1: only a producer restart is rejoined", opens)
	}
}

// A device that keeps restarting its producer is busy recording. Say so rather
// than reconnecting forever.
func TestCameraViewGivesUpAfterRepeatedRestarts(t *testing.T) {
	opens := 0
	open := func() (videoStream, error) {
		opens++
		return &mockVideoStream{err: producerRestartedError(t)}, nil
	}
	play := func(stream videoStream) error {
		_, err := stream.Recv()
		return err
	}
	err := streamCameraRejoiningRestarts(context.Background(), open, play)
	if err == nil {
		t.Fatal("the viewer rejoined forever instead of reporting a busy device")
	}
	if !strings.Contains(err.Error(), "busy recording") {
		t.Fatalf("error does not say the device is busy recording: %v", err)
	}
	if opens != cameraRejoinAttempts {
		t.Fatalf("streams opened = %d, want %d", opens, cameraRejoinAttempts)
	}
}
