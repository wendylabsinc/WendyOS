package commands

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// --- --require ---

func TestParseRequirements_AcceptsRepeatedAndCommaSeparatedNames(t *testing.T) {
	got, err := parseRequirements([]string{"aligned-depth", "measured-intrinsics,capture-settings"})
	if err != nil {
		t.Fatalf("parseRequirements: %v", err)
	}
	if framesource.Slugs(got) != "aligned-depth,measured-intrinsics,capture-settings" {
		t.Errorf("parsed = %v", framesource.Slugs(got))
	}
}

// The flag exists so an app can say what it cannot work without. A typo that
// parsed into nothing would produce a stream that looks required-and-satisfied
// and is neither.
func TestParseRequirements_RefusesATypoRatherThanIgnoringIt(t *testing.T) {
	if _, err := parseRequirements([]string{"aligned_depth", "meashured-intrinsics"}); err == nil {
		t.Fatal("a misspelled requirement was accepted")
	}
	// Underscores are a spelling of the same thing, not a typo.
	if _, err := parseRequirements([]string{"aligned_depth"}); err != nil {
		t.Errorf("aligned_depth was rejected: %v", err)
	}
}

// --- receive limit ---

// A request that names no geometry joins whatever is running, and a listing
// cannot say what that is: another consumer may have the camera pinned at a
// larger mode. Sizing from the listing's default would fail on the first Recv.
func TestRecvLimitFor_ADefaultGeometryRequestAcceptsWhateverIsRunning(t *testing.T) {
	sources := []*agentpbv2.CalibratedSource{{
		Source: "realsense:1", ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: framesource.FrameBytes(640*3, 480, 640*2, 480),
	}}
	got := recvLimitFor(sources, "realsense:1", framesource.Options{})
	// At least the largest frame the agent will ever relay from a helper.
	if got < framesource.MaxRecordBytes {
		t.Errorf("limit = %d; a bare request joining a 1080p capture must be able to receive it", got)
	}
	// And the same figure is what the agent's size gate is told, so it cannot
	// refuse a frame the client would have accepted.
	if uint64(got) < framesource.FrameBytes(1920*3, 1080, 1920*2, 1080) {
		t.Errorf("limit = %d does not cover a 1080p RGB-D frame", got)
	}
}

func TestRecvLimitFor_LeavesTheDefaultAloneForSmallExplicitFrames(t *testing.T) {
	sources := []*agentpbv2.CalibratedSource{{
		Source: "realsense:1", ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: framesource.FrameBytes(640*3, 480, 640*2, 480),
	}}
	if got := recvLimitFor(sources, "realsense:1", framesource.Options{Width: 320, Height: 240}); got != defaultClientRecvBytes {
		t.Errorf("limit = %d, want the grpc default %d", got, defaultClientRecvBytes)
	}
}

// The agent's estimate fails closed on an absurd geometry; the client's limit
// must not wrap on top of it.
func TestRecvLimitFor_DoesNotWrapOnAnAbsurdGeometry(t *testing.T) {
	sources := []*agentpbv2.CalibratedSource{{
		Source: "realsense:1", ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: framesource.FrameBytes(640*3, 480, 640*2, 480),
	}}
	got := recvLimitFor(sources, "realsense:1", framesource.Options{Width: 1 << 31, Height: 1 << 31})
	if got != math.MaxInt32 {
		t.Errorf("limit = %d, want the ceiling %d", got, math.MaxInt32)
	}
}

// One frame is one gRPC message, and a 1080p colour plane alone exceeds the
// 4 MiB default. A client that does not raise its limit cannot receive the
// stream it just asked for.
func TestRecvLimitFor_RaisesTheLimitForALargeCapture(t *testing.T) {
	sources := []*agentpbv2.CalibratedSource{{
		Source: "realsense:1", ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: framesource.FrameBytes(640*3, 480, 640*2, 480),
	}}
	got := recvLimitFor(sources, "realsense:1", framesource.Options{Width: 1920, Height: 1080})
	if got <= defaultClientRecvBytes {
		t.Fatalf("limit = %d; a 1920x1080 RGB-D frame does not fit the default", got)
	}
	// And it is at least as large as the figure the agent will compare against,
	// so the two sides cannot disagree about whether the stream is servable.
	want := framesource.FrameBytesFor(sources[0], framesource.Options{Width: 1920, Height: 1080})
	if uint64(got) < want {
		t.Errorf("limit %d is below the agent's own estimate %d", got, want)
	}
}

func TestRecvLimitFor_UnknownSourceFallsBackToTheDefault(t *testing.T) {
	if got := recvLimitFor(nil, "nothing", framesource.Options{Width: 64, Height: 48}); got != defaultClientRecvBytes {
		t.Errorf("limit = %d", got)
	}
}

// --- the operator's view ---

func frameWithCentreDepth(raw uint16) *agentpbv2.CalibratedFrame {
	const w, h = 4, 2
	depth := make([]byte, w*2*h)
	// centre pixel is (w/2, h/2) = (2, 1)
	binary.LittleEndian.PutUint16(depth[1*(w*2)+2*2:], raw)
	return &agentpbv2.CalibratedFrame{
		FrameId:      12,
		CapturedAtNs: 99,
		ColourFormat: &agentpb.RawFormat{Width: w, Height: h, Fourcc: "BGR3", BytesPerLine: w * 3},
		Colour:       make([]byte, w*3*h),
		Depth: &agentpbv2.DepthPlane{
			Data: depth, Width: w, Height: h, BytesPerLine: w * 2,
			ScaleM: 0.001, Alignment: agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR,
		},
		Intrinsics: &agentpbv2.CameraIntrinsics{
			Fx: 642.3, Fy: 642.3, Cx: 2, Cy: 1, Width: w, Height: h,
			Provenance: agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED,
		},
		Settings: &agentpbv2.CaptureSettings{ExposureRaw: 156, Gain: 64},
	}
}

// The cheapest possible proof that the depth is real, aligned and in metres —
// which is the question that had no answer before this command existed.
func TestDescribeCalibratedFrame_ReportsTheCentreDistanceInMetres(t *testing.T) {
	line := describeCalibratedFrame(frameWithCentreDepth(1340))
	for _, want := range []string{"frame 12", "aligned-to-colour", "centre 1.340 m", "measured"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q does not contain %q", line, want)
		}
	}
}

// RealSense writes 0 where it has no measurement. Reporting that as "0 metres"
// would be a distance, and it is not one.
func TestCentreDistanceMetres_ZeroIsNoMeasurementNotZeroMetres(t *testing.T) {
	if _, ok := centreDistanceMetres(frameWithCentreDepth(0).GetDepth()); ok {
		t.Error("a zero depth value was reported as a distance")
	}
	if !strings.Contains(describeCalibratedFrame(frameWithCentreDepth(0)), "centre n/a") {
		t.Error("the summary does not say the centre has no measurement")
	}
}

func TestDescribeCalibratedFrame_SaysWhenDepthIsAbsent(t *testing.T) {
	f := frameWithCentreDepth(1000)
	f.Depth = nil
	f.Intrinsics = nil
	f.Settings = nil
	line := describeCalibratedFrame(f)
	for _, want := range []string{"depth none", "intrinsics none", "settings none"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q does not contain %q", line, want)
		}
	}
}

// An exposure converted from a unit the device never stated reads like a
// measurement and is not one.
func TestDescribeCaptureSettings_OnlyPrintsMicrosecondsWhenTheUnitIsKnown(t *testing.T) {
	unknown := &agentpbv2.CaptureSettings{ExposureRaw: 156, Gain: 64}
	if got := describeCaptureSettings(unknown); !strings.Contains(got, "driver units") {
		t.Errorf("settings = %q; an unknown unit must say so", got)
	}
	known := &agentpbv2.CaptureSettings{ExposureRaw: 156, ExposureUs: 15600, ExposureUnitUs: 100, Gain: 64}
	if got := describeCaptureSettings(known); !strings.Contains(got, "15600 us") {
		t.Errorf("settings = %q", got)
	}
}

// --- the bytes for a program ---

func TestPipeCalibratedFramesToStdout_WritesColourThenDepth(t *testing.T) {
	f := frameWithCentreDepth(1340)
	for i := range f.Colour {
		f.Colour[i] = 0xAA
	}
	stream := &stubCalibratedStream{frames: []*agentpbv2.CalibratedFrame{f}}
	var out bytes.Buffer
	if err := pipeCalibratedFramesToStdout(stream, &out, 0); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	want := len(f.GetColour()) + len(f.GetDepth().GetData())
	if out.Len() != want {
		t.Fatalf("wrote %d bytes, want %d (colour plane then depth plane)", out.Len(), want)
	}
	if !bytes.Equal(out.Bytes()[:len(f.GetColour())], f.GetColour()) {
		t.Error("the colour plane is not the first thing on stdout")
	}
}

// The layout is announced once. A consumer that did not require depth is
// allowed to keep its colour when depth drops -- but a reader of --stdout was
// told every frame is C bytes then D bytes, and would silently read frame
// N+1's colour as frame N's depth from then on. That is a silent downgrade,
// so the stream ends with an error instead.
func TestPipeCalibratedFramesToStdout_EndsWithAnErrorWhenTheLayoutChanges(t *testing.T) {
	full := frameWithCentreDepth(1000)
	depthless := frameWithCentreDepth(1000)
	depthless.FrameId = 13
	depthless.Depth = nil
	stream := &stubCalibratedStream{frames: []*agentpbv2.CalibratedFrame{full, depthless, full}}
	var out bytes.Buffer
	err := pipeCalibratedFramesToStdout(stream, &out, 0)
	if err == nil {
		t.Fatal("a frame that changed the announced layout was written silently")
	}
	for _, want := range []string{"frame 13", "layout", "--require aligned-depth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if want := len(full.GetColour()) + len(full.GetDepth().GetData()); out.Len() != want {
		t.Errorf("wrote %d bytes, want exactly the one frame that matched the announced layout (%d)", out.Len(), want)
	}
}

func TestPipeCalibratedFramesToStdout_StopsAfterTheRequestedCount(t *testing.T) {
	f := frameWithCentreDepth(1000)
	stream := &stubCalibratedStream{frames: []*agentpbv2.CalibratedFrame{f, f, f}}
	var out bytes.Buffer
	if err := pipeCalibratedFramesToStdout(stream, &out, 2); err != nil {
		t.Fatal(err)
	}
	if stream.read != 2 {
		t.Errorf("read %d frames, want 2", stream.read)
	}
}

type stubCalibratedStream struct {
	frames []*agentpbv2.CalibratedFrame
	read   int
}

func (s *stubCalibratedStream) Recv() (*agentpbv2.CalibratedFrame, error) {
	if s.read >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.read]
	s.read++
	return f, nil
}

// --- the listing ---

func TestRenderCalibratedSources_PrintsTheUnavailableReasonInFull(t *testing.T) {
	const reason = "Intel(R) RealSense(TM) Depth Camera 435i is attached but the wendy-realsense-source capture helper is not installed"
	var out bytes.Buffer
	err := renderCalibratedSources(&out, []*agentpbv2.CalibratedSource{{
		Source: "realsense", Kind: "realsense", Description: "D435i",
		Available: false, UnavailableReason: reason,
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Truncated into a table column, the sentence loses exactly the part that
	// names the fix.
	if !strings.Contains(out.String(), reason) {
		t.Errorf("listing does not carry the reason in full:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "unavailable") {
		t.Errorf("listing does not mark the source unavailable:\n%s", out.String())
	}
}

func TestRenderCalibratedSources_EmptyListSaysWhyThereMightBeNone(t *testing.T) {
	var out bytes.Buffer
	if err := renderCalibratedSources(&out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No calibrated frame sources") {
		t.Errorf("output = %q", out.String())
	}
}

// --- diagnostics ---

func TestCameraStreamDiagnostic_RequirementUnmetNamesTheFlagToDrop(t *testing.T) {
	err := streamreason.New(codes.FailedPrecondition,
		"calibrated frame source realsense:1 cannot provide: aligned-depth (this source has no depth sensor)",
		streamreason.RequirementUnmet,
		map[string]string{"source": "realsense:1", "missing": "aligned-depth"})

	got := cameraStreamDiagnostic(err).Error()
	if !strings.Contains(got, "no depth sensor") {
		t.Errorf("diagnostic loses the agent's reason: %q", got)
	}
	if !strings.Contains(got, "--require aligned-depth") {
		t.Errorf("diagnostic does not name the flag to drop: %q", got)
	}
}

func TestCameraStreamDiagnostic_MissingHelperSaysToInstallIt(t *testing.T) {
	err := streamreason.New(codes.FailedPrecondition,
		"calibrated frame source realsense is not available: the helper is not installed",
		streamreason.CalibratedSourceUnavailable,
		map[string]string{"source": "realsense", "helper": "wendy-realsense-source"})

	got := cameraStreamDiagnostic(err).Error()
	if !strings.Contains(got, "wendy-realsense-source") || !strings.Contains(got, "Install") {
		t.Errorf("diagnostic = %q", got)
	}
}

func TestCameraStreamDiagnostic_LeavesUnknownReasonsAlone(t *testing.T) {
	err := streamreason.New(codes.Internal, "something else", "SOME_OTHER_REASON", nil)
	if got := cameraStreamDiagnostic(err); got.Error() != err.Error() {
		t.Errorf("diagnostic rewrote an unrelated error: %q", got)
	}
}
