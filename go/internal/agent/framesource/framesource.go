// Package framesource is the seam between the agent's calibrated-frame service
// and the things that can actually produce a calibrated frame.
//
// A calibrated frame is a measurement, not a picture: colour, depth aligned to
// those colour pixels, the scale that turns depth into metres, intrinsics with
// their provenance, and the settings the device actually applied — all under
// one frame id and one capture instant. Only something that OWNS the sensor can
// produce that, because alignment needs the module's factory extrinsics and the
// scale is a per-device constant only the vendor SDK reports.
//
// The service above this package does the parts that are the same whatever the
// camera is: refusing a consumer whose requirements a source cannot meet,
// fanning one capture out to many subscribers latest-wins, and ending a stream
// the moment a required property stops holding. A Source does the one part that
// is not: getting the pixels.
//
// Everything here speaks the generated proto types directly rather than a
// parallel set of Go structs. A second description of the same frame is a
// second place for the depth scale to go missing, and this package exists
// because a depth plane whose scale went missing is indistinguishable from a
// working one.
package framesource

import (
	"context"
	"fmt"
	"sort"
	"strings"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Options is the capture geometry a subscriber asked for. A zero field means
// "the source's default" — the same convention StreamVideoRequest uses, so a
// consumer that does not care about geometry writes nothing.
type Options struct {
	Width     uint32
	Height    uint32
	Framerate uint32
}

// IsDefault reports whether nothing was asked for at all. Such a request joins
// whatever the source is already producing rather than forcing a restart that
// would interrupt every other subscriber on the same camera.
func (o Options) IsDefault() bool { return o.Width == 0 && o.Height == 0 && o.Framerate == 0 }

// Source is one thing that can publish calibrated frames.
//
// Describe must be answerable WITHOUT opening a stream. That is the whole point:
// a consumer learns that this camera can never give it aligned depth before it
// writes the code that assumes it has depth, rather than after a field failure.
type Source interface {
	// Describe reports the source's identity and what it can promise. It is
	// called on every listing and before every subscribe, so it must be cheap
	// and must not disturb a capture already in progress.
	Describe(ctx context.Context) (*agentpbv2.CalibratedSource, error)
	// Open begins capture. The returned Stream is owned by the caller and must
	// be closed. A source that cannot be opened returns an error whose message
	// names the fix.
	Open(ctx context.Context, opts Options) (Stream, error)
}

// Stream is an open capture. Next blocks until the next frame arrives and
// returns io.EOF when the source ends normally.
//
// Close MUST be safe to call more than once and CONCURRENTLY with Next, and
// must unblock a Next that is waiting. That is not a nicety: a helper process
// blocked in a read does not notice a cancelled context, so closing the pipe
// underneath it is the only thing that ends the capture — and the closing
// happens on a different goroutine from the one waiting for the frame.
type Stream interface {
	Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error)
	Close() error
}

// --- the requirement vocabulary ---
//
// One set of slugs, used by the CLI flag, the refusal metadata and the log
// line. A requirement a consumer can name and a requirement a source can
// advertise are the same thing; spelling them separately is how the two drift.

const (
	SlugAlignedDepth       = "aligned-depth"
	SlugMeasuredIntrinsics = "measured-intrinsics"
	SlugCaptureSettings    = "capture-settings"
)

var requirementSlugs = map[agentpbv2.FrameRequirement]string{
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH:       SlugAlignedDepth,
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS: SlugMeasuredIntrinsics,
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS:    SlugCaptureSettings,
}

// Requirements lists every requirement a consumer may declare, in a stable
// order, so help text and listings agree.
var Requirements = []agentpbv2.FrameRequirement{
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH,
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS,
	agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS,
}

// Slug renders a requirement as the name a person types and a refusal prints.
func Slug(r agentpbv2.FrameRequirement) string {
	if s, ok := requirementSlugs[r]; ok {
		return s
	}
	return strings.ToLower(r.String())
}

// Slugs renders a list of requirements, comma separated.
func Slugs(rs []agentpbv2.FrameRequirement) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, Slug(r))
	}
	return strings.Join(out, ",")
}

// ParseRequirement maps a typed name onto a requirement. The error lists what
// is spelled correctly, because a mistyped requirement that silently parsed to
// UNSPECIFIED would be a requirement nobody checks.
func ParseRequirement(s string) (agentpbv2.FrameRequirement, error) {
	want := strings.ToLower(strings.TrimSpace(s))
	want = strings.ReplaceAll(want, "_", "-")
	for _, r := range Requirements {
		if Slug(r) == want {
			return r, nil
		}
	}
	return agentpbv2.FrameRequirement_FRAME_REQUIREMENT_UNSPECIFIED,
		fmt.Errorf("unknown requirement %q; known: %s", s, Slugs(Requirements))
}

// Normalise drops UNSPECIFIED and duplicates from a declared requirement list
// and returns it in the canonical order, so a refusal reads the same however
// the client ordered its flags.
func Normalise(rs []agentpbv2.FrameRequirement) []agentpbv2.FrameRequirement {
	seen := map[agentpbv2.FrameRequirement]bool{}
	for _, r := range rs {
		if r != agentpbv2.FrameRequirement_FRAME_REQUIREMENT_UNSPECIFIED {
			seen[r] = true
		}
	}
	out := make([]agentpbv2.FrameRequirement, 0, len(seen))
	for _, r := range Requirements {
		if seen[r] {
			out = append(out, r)
		}
	}
	// Anything not in the canonical list (a requirement added by a newer
	// client than this agent) is kept, so it is REFUSED by name rather than
	// dropped and silently treated as satisfied.
	var extra []agentpbv2.FrameRequirement
	for r := range seen {
		if _, known := requirementSlugs[r]; !known {
			extra = append(extra, r)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append(out, extra...)
}

// --- what a source provides, and what a frame carries ---

// MissingFromSource reports which required properties this source does not
// advertise. Checked before the first frame, so a consumer is refused up front
// rather than left waiting on a stream that will never carry what it needs.
func MissingFromSource(src *agentpbv2.CalibratedSource, require []agentpbv2.FrameRequirement) []agentpbv2.FrameRequirement {
	provides := map[agentpbv2.FrameRequirement]bool{}
	for _, r := range src.GetProvides() {
		provides[r] = true
	}
	var missing []agentpbv2.FrameRequirement
	for _, r := range Normalise(require) {
		if !provides[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

// MissingFromFrame reports which required properties this particular frame does
// not carry. A source that advertised aligned depth and then stops sending it —
// the depth sensor drops out, auto-exposure is taken over by another process —
// fails here, and the subscriber's stream ends. That is the single rule that
// turns a silent degradation into a visible outage.
func MissingFromFrame(f *agentpbv2.CalibratedFrame, require []agentpbv2.FrameRequirement) []agentpbv2.FrameRequirement {
	var missing []agentpbv2.FrameRequirement
	for _, r := range Normalise(require) {
		if !frameHas(f, r) {
			missing = append(missing, r)
		}
	}
	return missing
}

func frameHas(f *agentpbv2.CalibratedFrame, r agentpbv2.FrameRequirement) bool {
	switch r {
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH:
		d := f.GetDepth()
		return d != nil &&
			d.GetAlignment() == agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR &&
			d.GetScaleM() > 0
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS:
		in := f.GetIntrinsics()
		return in != nil &&
			in.GetProvenance() == agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED &&
			in.GetFx() > 0 && in.GetFy() > 0
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS:
		return f.GetSettings() != nil
	default:
		// A requirement this agent does not know cannot be satisfied by it.
		return false
	}
}

// WhySourceLacks explains, for the refusal metadata, why this source cannot
// provide a property. An operator reading "aligned-depth: this camera has no
// depth sensor" knows to stop looking for a setting; "the capture helper is not
// installed" knows to install one.
func WhySourceLacks(src *agentpbv2.CalibratedSource, r agentpbv2.FrameRequirement) string {
	if !src.GetAvailable() && src.GetUnavailableReason() != "" {
		return src.GetUnavailableReason()
	}
	switch r {
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH:
		if src.GetDepthWidth() == 0 || src.GetDepthHeight() == 0 {
			return "this source has no depth sensor"
		}
		return "this source's depth is in the depth sensor's own frame; it has no extrinsics to align it to the colour plane"
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS:
		return "this source reports no factory calibration, so its intrinsics would be assumed from a datasheet field of view"
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS:
		return "this source cannot read back the exposure and gain it applied"
	default:
		return "this agent does not know this requirement"
	}
}

// WhyFrameLacks is WhySourceLacks for a frame that stopped carrying a property
// the source advertised. The distinction matters to whoever reads the error: it
// is not a camera that never could, it is one that stopped.
func WhyFrameLacks(f *agentpbv2.CalibratedFrame, r agentpbv2.FrameRequirement) string {
	switch r {
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH:
		switch d := f.GetDepth(); {
		case d == nil:
			return "the source stopped sending a depth plane"
		case d.GetScaleM() <= 0:
			return "the source stopped reporting a depth scale, so its depth is in unknown units"
		default:
			return "the source stopped aligning depth to the colour plane"
		}
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS:
		return "the source stopped reporting measured intrinsics"
	case agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS:
		return "the source stopped reporting the settings it applied"
	default:
		return "this agent does not know this requirement"
	}
}

// --- the four rules the source enforces, enforced once more at the boundary ---

// Sanitise makes a frame honest before it is fanned out, and reports what it
// took away. It is applied once in the producer, so every subscriber sees the
// same frame and nobody re-checks.
//
// A plane that survives Sanitise obeys the rules in the proto: aligned depth
// matches the colour resolution exactly, depth has a positive scale, and each
// plane carries as many bytes as its geometry claims. Anything else is REMOVED
// rather than passed on, because a depth plane that is subtly wrong is worth
// less than no depth plane at all — a consumer can refuse the second one.
func Sanitise(f *agentpbv2.CalibratedFrame) []string {
	var dropped []string
	if d := f.GetDepth(); d != nil {
		if why := depthIsUnusable(f); why != "" {
			f.Depth = nil
			dropped = append(dropped, "depth: "+why)
		}
	}
	if in := f.GetIntrinsics(); in != nil {
		if in.GetFx() <= 0 || in.GetFy() <= 0 {
			f.Intrinsics = nil
			dropped = append(dropped, "intrinsics: focal length is not positive")
		} else if in.GetProvenance() == agentpbv2.CameraIntrinsics_PROVENANCE_UNSPECIFIED {
			// Unspecified provenance is not a third kind of calibration; it is
			// a source that did not say. Treat it as assumed rather than let a
			// MEASURED_INTRINSICS check fall through on a default value.
			in.Provenance = agentpbv2.CameraIntrinsics_PROVENANCE_ASSUMED
			dropped = append(dropped, "intrinsics: provenance unstated, recorded as assumed")
		}
	}
	return dropped
}

func depthIsUnusable(f *agentpbv2.CalibratedFrame) string {
	d := f.GetDepth()
	if d.GetScaleM() <= 0 {
		// Rule 2: depth in unknown units is not depth. A source that cannot
		// read its scale must refuse, not default to 1 mm.
		return "no depth scale, so its values are in unknown units"
	}
	if d.GetWidth() == 0 || d.GetHeight() == 0 {
		return "zero-sized plane"
	}
	minStride := d.GetWidth() * 2 // uint16 per pixel
	if d.GetBytesPerLine() < minStride {
		return fmt.Sprintf("stride %d is too short for %d 16-bit pixels", d.GetBytesPerLine(), d.GetWidth())
	}
	if want := int(d.GetBytesPerLine()) * int(d.GetHeight()); len(d.GetData()) != want {
		return fmt.Sprintf("carries %d bytes, not the %d its geometry claims", len(d.GetData()), want)
	}
	if d.GetAlignment() == agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR {
		// Rule 1: aligned depth is the colour plane's resolution, exactly. This
		// is the check a consumer runs today before it will trust a frame; the
		// source asserting it is what makes that check never fire.
		cf := f.GetColourFormat()
		if d.GetWidth() != cf.GetWidth() || d.GetHeight() != cf.GetHeight() {
			return fmt.Sprintf("claims alignment to colour but is %dx%d against a %dx%d colour plane",
				d.GetWidth(), d.GetHeight(), cf.GetWidth(), cf.GetHeight())
		}
	}
	return ""
}

// FrameBytes is an upper bound on one serialized CalibratedFrame for a source
// of this geometry: both planes plus room for the scalar fields and proto
// framing. It is what ListCalibratedSources reports and what the subscribe-time
// size refusal compares against, so a client can size its receive limit from a
// listing instead of discovering the limit on its first Recv.
func FrameBytes(colourBytesPerLine, colourHeight, depthBytesPerLine, depthHeight uint32) uint64 {
	const scalarOverhead = 4096 // intrinsics, settings, ids, field tags, source name
	return uint64(colourBytesPerLine)*uint64(colourHeight) +
		uint64(depthBytesPerLine)*uint64(depthHeight) + scalarOverhead
}

// FrameBytesFor scales a source's default-geometry frame size by the pixel
// count a subscriber actually asked for. CalibratedSource.max_frame_bytes
// describes the source's DEFAULT mode, which is the only one a listing can
// describe without enumerating every mode the device has — so a subscriber
// asking for 1280x720 from a source that defaults to 640x480 gets four times
// the pixels and must be told so before it subscribes, not on its first Recv.
//
// Both the agent's subscribe-time refusal and the CLI's receive-limit sizing go
// through this, so the number the client raises its limit to and the number the
// agent compares against are the same number.
func FrameBytesFor(desc *agentpbv2.CalibratedSource, opts Options) uint64 {
	base := desc.GetMaxFrameBytes()
	defaultPixels := uint64(desc.GetColourWidth()) * uint64(desc.GetColourHeight())
	requestedPixels := uint64(opts.Width) * uint64(opts.Height)
	if base == 0 || defaultPixels == 0 || requestedPixels == 0 || requestedPixels == defaultPixels {
		return base
	}
	return base * requestedPixels / defaultPixels
}
