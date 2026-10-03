// Package worldview turns per-frame object proposals into scored, fused and
// tracked matches for the world view feature. A campaign describes an object
// as attributes (shape, size, colour, an app model's class); the Python worker
// measures each segmented candidate as a Proposal; the scorers here grade each
// proposal per attribute, Fuse combines the grades into one confidence, and
// the Tracker turns per-frame matches into lifecycle events.
//
// The package is pure: no I/O, no clocks (callers pass time in), no goroutines,
// and deterministic output for identical input.
//
// Every box in this package is x, y, w, h in pixels, where x, y is the top left
// corner. The existing inference worker returns the boxes of Hugging Face
// post_process_object_detection, which are corner boxes (x1, y1, x2, y2), so
// callers turning those detections into ClassEvidence must convert them.
package worldview

// Attribute names written into Evidence.Attribute by the scorers.
const (
	AttributeShape  = "shape"
	AttributeSize   = "size"
	AttributeColour = "colour"
	AttributeClass  = "class"
)

// Silhouette primitives reported by the worker.
const (
	SilhouetteDisc      = "disc"
	SilhouetteRect      = "rect"
	SilhouetteTrapezoid = "trapezoid"
	SilhouetteOther     = "other"
)

// Solid primitives a campaign may expect.
const (
	SolidCylinder = "cylinder"
	SolidBox      = "box"
	SolidSphere   = "sphere"
	SolidCone     = "cone"
)

// silhouettesFor maps each solid to the silhouettes it can cast.
var silhouettesFor = map[string][]string{
	SolidCylinder: {SilhouetteRect, SilhouetteDisc},
	SolidBox:      {SilhouetteRect},
	SolidSphere:   {SilhouetteDisc},
	SolidCone:     {SilhouetteTrapezoid, SilhouetteDisc},
}

// SilhouettesFor returns the silhouettes a solid primitive is compatible with,
// or nil for a primitive that is not a known solid. The result is a copy.
func SilhouettesFor(primitive string) []string {
	return append([]string(nil), silhouettesFor[primitive]...)
}

// casts reports whether solid can cast silhouette, and whether solid is known.
func casts(solid, silhouette string) (compatible, known bool) {
	options, known := silhouettesFor[solid]
	for _, o := range options {
		if o == silhouette {
			return true, true
		}
	}
	return false, known
}

// Proposal is one segmented candidate in one frame, as measured by the worker.
type Proposal struct {
	SourceID       string
	SampleID       uint64
	BootNanos      int64
	Box            [4]float64 // x, y, w, h in pixels of the scored frame
	FrameW, FrameH int
	Palette        []PaletteEntry // CIELAB, shares sum to <= 1
	Silhouette     Silhouette
	Metric         *Metric // nil when no depth was paired
}

// PaletteEntry is one colour cluster: a CIELAB colour and the share of the
// proposal's pixels it covers.
type PaletteEntry struct {
	Lab   [3]float64
	Share float64
}

// Silhouette is the worker's two dimensional primitive guess for a proposal.
// Primitive is one of the Silhouette constants; a single camera view cannot
// tell a cylinder from a box, so the worker never reports a solid.
type Silhouette struct {
	Primitive string
	Aspect    float64 // height / width
}

// Metric holds depth-derived measurements in metres and degrees.
type Metric struct {
	WidthM, HeightM, DistanceM, BearingDeg float64
}

// Evidence is one scorer's verdict on one proposal for one attribute.
type Evidence struct {
	Attribute string
	Score     float64        // 0..1, meaningful only when Available
	Available bool           // false means the scorer had no input, NOT that it scored zero
	Detail    map[string]any // what the scorer measured, for the detection record
}
