package framesource

import (
	"math"
	"strings"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	alignedDepth       = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH
	measuredIntrinsics = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS
	captureSettings    = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS
)

// goodFrame is a complete frame: aligned millimetre depth at the colour
// resolution, measured intrinsics, applied settings.
func goodFrame() *agentpbv2.CalibratedFrame {
	const w, h = 8, 4
	return &agentpbv2.CalibratedFrame{
		FrameId:      7,
		CapturedAtNs: 1234,
		Colour:       make([]byte, w*3*h),
		ColourFormat: &agentpb.RawFormat{Width: w, Height: h, Fourcc: "BGR3", BytesPerLine: w * 3},
		Depth: &agentpbv2.DepthPlane{
			Data: make([]byte, w*2*h), Width: w, Height: h, BytesPerLine: w * 2,
			ScaleM: 0.001, Alignment: agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR,
		},
		Intrinsics: &agentpbv2.CameraIntrinsics{
			Fx: 100, Fy: 100, Cx: 4, Cy: 2, Width: w, Height: h,
			Provenance: agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED,
		},
		Settings: &agentpbv2.CaptureSettings{ExposureRaw: 10, Gain: 4},
	}
}

// --- the requirement vocabulary ---

func TestParseRequirement_AcceptsTheNamesItPrints(t *testing.T) {
	for _, r := range Requirements {
		got, err := ParseRequirement(Slug(r))
		if err != nil || got != r {
			t.Errorf("ParseRequirement(%q) = %v, %v; want %v", Slug(r), got, err, r)
		}
	}
}

// A requirement that parsed to UNSPECIFIED would be dropped by Normalise and
// then silently treated as satisfied — a --require flag nobody checks, which is
// worse than no flag at all.
func TestParseRequirement_RejectsATypoAndListsWhatIsSpelledRight(t *testing.T) {
	_, err := ParseRequirement("alligned-depth")
	if err == nil {
		t.Fatal("a misspelled requirement was accepted")
	}
	if !strings.Contains(err.Error(), SlugAlignedDepth) {
		t.Errorf("error does not list the correct spelling: %v", err)
	}
}

func TestNormalise_IsOrderIndependentAndDropsDuplicates(t *testing.T) {
	got := Normalise([]agentpbv2.FrameRequirement{captureSettings, alignedDepth, alignedDepth,
		agentpbv2.FrameRequirement_FRAME_REQUIREMENT_UNSPECIFIED})
	want := []agentpbv2.FrameRequirement{alignedDepth, captureSettings}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Normalise = %v, want %v", got, want)
	}
}

// A requirement this agent has never heard of comes from a NEWER client. It
// must survive normalisation so it can be refused by name; dropping it would
// tell that client its requirement was met.
func TestNormalise_KeepsARequirementThisAgentDoesNotKnow(t *testing.T) {
	future := agentpbv2.FrameRequirement(999)
	got := Normalise([]agentpbv2.FrameRequirement{future})
	if len(got) != 1 || got[0] != future {
		t.Fatalf("Normalise dropped an unknown requirement: %v", got)
	}
	if MissingFromFrame(goodFrame(), got) == nil {
		t.Error("an unknown requirement was treated as satisfied by a frame")
	}
}

// --- what a frame carries ---

func TestMissingFromFrame_CompleteFrameSatisfiesEverything(t *testing.T) {
	if missing := MissingFromFrame(goodFrame(), Requirements); len(missing) != 0 {
		t.Errorf("a complete frame is missing %v", Slugs(missing))
	}
}

func TestMissingFromFrame_EachDegradation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*agentpbv2.CalibratedFrame)
		missing agentpbv2.FrameRequirement
	}{
		{"depth plane gone", func(f *agentpbv2.CalibratedFrame) { f.Depth = nil }, alignedDepth},
		{"depth scale lost", func(f *agentpbv2.CalibratedFrame) { f.Depth.ScaleM = 0 }, alignedDepth},
		{"depth no longer aligned", func(f *agentpbv2.CalibratedFrame) {
			f.Depth.Alignment = agentpbv2.DepthPlane_ALIGNMENT_SENSOR_NATIVE
		}, alignedDepth},
		{"intrinsics became assumed", func(f *agentpbv2.CalibratedFrame) {
			f.Intrinsics.Provenance = agentpbv2.CameraIntrinsics_PROVENANCE_ASSUMED
		}, measuredIntrinsics},
		{"settings gone", func(f *agentpbv2.CalibratedFrame) { f.Settings = nil }, captureSettings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFrame()
			tc.mutate(f)
			missing := MissingFromFrame(f, Requirements)
			if len(missing) != 1 || missing[0] != tc.missing {
				t.Errorf("missing = %v, want exactly %s", Slugs(missing), Slug(tc.missing))
			}
			if why := WhyFrameLacks(f, tc.missing); why == "" || !strings.Contains(why, "stopped") {
				t.Errorf("WhyFrameLacks = %q; a mid-stream loss must read as a loss", why)
			}
		})
	}
}

// --- Sanitise: the four rules, enforced once at the boundary ---

func TestSanitise_DropsDepthThatHasNoScale(t *testing.T) {
	f := goodFrame()
	f.Depth.ScaleM = 0
	dropped := Sanitise(f)
	if f.Depth != nil {
		t.Error("a depth plane in unknown units was passed on; guessing 1 mm is the failure this exists to prevent")
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "scale") {
		t.Errorf("dropped = %v", dropped)
	}
}

func TestSanitise_DropsAlignedDepthThatIsNotTheColourResolution(t *testing.T) {
	f := goodFrame()
	f.Depth.Width, f.Depth.Height = 4, 2
	f.Depth.BytesPerLine = 8
	f.Depth.Data = make([]byte, 8*2)
	if dropped := Sanitise(f); len(dropped) == 0 {
		t.Fatal("misaligned depth survived")
	}
	if f.Depth != nil {
		t.Error("depth claiming alignment at another resolution was passed on")
	}
}

func TestSanitise_DropsAPlaneThatDoesNotCarryTheBytesItClaims(t *testing.T) {
	f := goodFrame()
	f.Depth.Data = f.Depth.Data[:4] // truncated
	Sanitise(f)
	if f.Depth != nil {
		t.Error("a short depth plane was passed on; a consumer would index past its end")
	}
}

func TestSanitise_KeepsSensorNativeDepthWhoseGeometryDiffers(t *testing.T) {
	// Sensor-native depth is allowed to be another resolution; it is simply not
	// interchangeable with colour, which is what the alignment field says.
	f := goodFrame()
	f.Depth.Alignment = agentpbv2.DepthPlane_ALIGNMENT_SENSOR_NATIVE
	f.Depth.Width, f.Depth.Height = 4, 2
	f.Depth.BytesPerLine = 8
	f.Depth.Data = make([]byte, 8*2)
	if dropped := Sanitise(f); len(dropped) != 0 {
		t.Fatalf("sensor-native depth was dropped: %v", dropped)
	}
	if f.Depth == nil {
		t.Fatal("sensor-native depth was removed")
	}
	if missing := MissingFromFrame(f, []agentpbv2.FrameRequirement{alignedDepth}); len(missing) != 1 {
		t.Error("sensor-native depth satisfied an ALIGNED_DEPTH requirement")
	}
}

// Unstated provenance is a source that did not say, not a third kind of
// calibration. Recording it as assumed keeps a MEASURED_INTRINSICS check from
// passing on a default zero value.
func TestSanitise_UnstatedIntrinsicProvenanceBecomesAssumed(t *testing.T) {
	f := goodFrame()
	f.Intrinsics.Provenance = agentpbv2.CameraIntrinsics_PROVENANCE_UNSPECIFIED
	Sanitise(f)
	if f.GetIntrinsics().GetProvenance() != agentpbv2.CameraIntrinsics_PROVENANCE_ASSUMED {
		t.Errorf("provenance = %v", f.GetIntrinsics().GetProvenance())
	}
	if missing := MissingFromFrame(f, []agentpbv2.FrameRequirement{measuredIntrinsics}); len(missing) != 1 {
		t.Error("unstated provenance satisfied a MEASURED_INTRINSICS requirement")
	}
}

func TestSanitise_LeavesAGoodFrameAlone(t *testing.T) {
	f := goodFrame()
	if dropped := Sanitise(f); len(dropped) != 0 {
		t.Fatalf("a valid frame lost %v", dropped)
	}
	if f.Depth == nil || f.Intrinsics == nil || f.Settings == nil {
		t.Error("a valid frame was stripped")
	}
}

// --- sizing ---

func TestFrameBytesFor_ScalesWithTheRequestedPixelCount(t *testing.T) {
	desc := &agentpbv2.CalibratedSource{
		ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: FrameBytes(640*3, 480, 640*2, 480),
	}
	base := FrameBytesFor(desc, Options{})
	if base != desc.GetMaxFrameBytes() {
		t.Errorf("default geometry = %d, want %d", base, desc.GetMaxFrameBytes())
	}
	// 1280x720 is three times the pixels of 640x480.
	got := FrameBytesFor(desc, Options{Width: 1280, Height: 720})
	if want := base * 3; got != want {
		t.Errorf("1280x720 = %d, want %d", got, want)
	}
}

func TestWhySourceLacks_NamesTheReasonNotJustTheProperty(t *testing.T) {
	colourOnly := &agentpbv2.CalibratedSource{Source: "webcam", Available: true}
	why := WhySourceLacks(colourOnly, alignedDepth)
	if !strings.Contains(why, "no depth sensor") {
		t.Errorf("WhySourceLacks = %q; an operator needs to know to stop looking for a setting", why)
	}
	// An unavailable source's own reason wins: "the helper is not installed" is
	// more actionable than "this source has no depth sensor", and is true where
	// the other is a guess.
	unavailable := &agentpbv2.CalibratedSource{Source: "realsense", UnavailableReason: "helper not installed"}
	if got := WhySourceLacks(unavailable, alignedDepth); got != "helper not installed" {
		t.Errorf("WhySourceLacks = %q", got)
	}
}

// --- the colour plane: the one every consumer relies on ---

// Depth was checked against its geometry; colour was not. A short colour
// buffer passed through and a consumer slicing it by bytes_per_line * height
// read past its end -- the same class of gap on the plane that matters most.
func TestColourIsUnusable_EachWayAColourPlaneCanLie(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(f *agentpbv2.CalibratedFrame)
		want string
	}{
		{"truncated", func(f *agentpbv2.CalibratedFrame) { f.Colour = f.Colour[:len(f.Colour)-1] }, "bytes"},
		{"oversized", func(f *agentpbv2.CalibratedFrame) { f.Colour = append(f.Colour, 0) }, "bytes"},
		{"stride too short", func(f *agentpbv2.CalibratedFrame) { f.ColourFormat.BytesPerLine = f.ColourFormat.Width }, "stride"},
		{"no geometry", func(f *agentpbv2.CalibratedFrame) { f.ColourFormat = nil }, "geometry"},
		{"zero height", func(f *agentpbv2.CalibratedFrame) { f.ColourFormat.Height = 0 }, "geometry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFrame()
			tc.mut(f)
			why := ColourIsUnusable(f)
			if why == "" {
				t.Fatal("a colour plane that does not match its geometry was accepted")
			}
			if !strings.Contains(why, tc.want) {
				t.Errorf("reason %q does not mention %q", why, tc.want)
			}
		})
	}
}

func TestColourIsUnusable_AcceptsAPlaneThatMatchesItsGeometry(t *testing.T) {
	if why := ColourIsUnusable(goodFrame()); why != "" {
		t.Errorf("a valid colour plane was refused: %s", why)
	}
	// A stride wider than the pixels is padding, not a lie; the byte count
	// still has to match it.
	f := goodFrame()
	f.ColourFormat.BytesPerLine = f.ColourFormat.Width*3 + 8
	f.Colour = make([]byte, int(f.ColourFormat.BytesPerLine)*int(f.ColourFormat.Height))
	if why := ColourIsUnusable(f); why != "" {
		t.Errorf("a padded stride was refused: %s", why)
	}
}

// The request's geometry is client-chosen. A product that wraps 64 bits is a
// small number, and the size gate would admit the stream it exists to refuse.
func TestFrameBytesFor_FailsClosedWhenTheEstimateOverflows(t *testing.T) {
	desc := &agentpbv2.CalibratedSource{
		ColourWidth: 640, ColourHeight: 480,
		MaxFrameBytes: FrameBytes(640*3, 480, 640*2, 480),
	}
	absurd := Options{Width: math.MaxUint32, Height: math.MaxUint32}
	if got := FrameBytesFor(desc, absurd); got != math.MaxUint64 {
		t.Errorf("FrameBytesFor(absurd) = %d; want the largest size there is, so the gate refuses", got)
	}
	// A large but honest request is still scaled exactly, not refused.
	big := Options{Width: 4096, Height: 4096}
	want := desc.GetMaxFrameBytes() * (4096 * 4096) / (640 * 480)
	if got := FrameBytesFor(desc, big); got != want {
		t.Errorf("FrameBytesFor(4096x4096) = %d, want %d", got, want)
	}
}
