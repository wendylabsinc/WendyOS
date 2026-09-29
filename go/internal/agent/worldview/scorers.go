package worldview

import (
	"math"
	"strconv"
	"time"
)

// DefaultAssociationWindow is how far apart, in boot time, class evidence and a
// proposal may be and still be compared by ScoreClass.
const DefaultAssociationWindow = 500 * time.Millisecond

// DefaultColourTolerance is the CIE76 delta E at which a colour entry stops
// matching when ColourExpect.Tolerance is not positive.
const DefaultColourTolerance = 20.0

// ClassIoUMin is the Intersection over Union (IoU) a piece of class evidence
// must reach with a proposal's box to vouch for it.
const ClassIoUMin = 0.5

// OtherSilhouetteScore is the primitive term for a silhouette the worker could
// not classify: weak evidence against the expected solid, not proof.
const OtherSilhouetteScore = 0.25

// ShapeExpect is the expected shape. Primitive is a solid (one of the Solid
// constants) or empty to accept any silhouette; a nil Aspect accepts any
// aspect ratio. Aspect is [min, max] of height / width and is reordered when
// given reversed.
type ShapeExpect struct {
	Primitive string
	Aspect    *[2]float64
}

// SizeExpect is the expected metric extent in metres, each a [min, max] range.
// A range of [0, 0] leaves that dimension unconstrained. DepthM constrains the
// measured distance from the camera (Metric.DistanceM), because the worker
// measures no object depth extent.
type SizeExpect struct {
	WidthM, HeightM [2]float64
	DepthM          *[2]float64
}

// ColourExpect is the expected palette and the delta E at which an entry stops
// matching.
type ColourExpect struct {
	Palette   []PaletteEntry
	Tolerance float64
}

// ClassExpect is the expected label from an app model. An empty Label accepts
// any label; an empty Model accepts any model.
type ClassExpect struct {
	Label string
	Model string
}

// ClassEvidence is one detection from an app prediction record. Box is x, y,
// w, h in pixels of the same frame geometry as the proposal. SourceID names
// the camera the prediction came from.
type ClassEvidence struct {
	SourceID   string
	Label      string
	Model      string
	Confidence float64
	Box        [4]float64
	BootNanos  int64
}

// IoU returns the Intersection over Union of two x, y, w, h boxes. Boxes with
// a non-positive width or height have no area, so their IoU is 0.
func IoU(a, b [4]float64) float64 {
	if a[2] <= 0 || a[3] <= 0 || b[2] <= 0 || b[3] <= 0 {
		return 0
	}
	iw := math.Min(a[0]+a[2], b[0]+b[2]) - math.Max(a[0], b[0])
	ih := math.Min(a[1]+a[3], b[1]+b[3]) - math.Max(a[1], b[1])
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	union := a[2]*a[3] + b[2]*b[3] - inter
	if !(union > 0) {
		return 0
	}
	return inter / union
}

// DeltaE76 returns the CIE76 colour difference: the Euclidean distance between
// two CIELAB colours.
func DeltaE76(a, b [3]float64) float64 {
	dl, da, db := a[0]-b[0], a[1]-b[1], a[2]-b[2]
	return math.Sqrt(dl*dl + da*da + db*db)
}

// linearFit is 1 inside [lo, hi] and falls linearly to 0 at below under lo or
// above over hi. A non-positive falloff width, or a non-finite value, fits 0
// outside the range.
func linearFit(v, lo, hi, below, above float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v >= lo && v <= hi {
		return 1
	}
	d, w := lo-v, below
	if v > hi {
		d, w = v-hi, above
	}
	if !(w > 0) {
		return 0
	}
	return math.Max(0, 1-d/w)
}

// clamp01 limits v to [0, 1] and maps NaN to 0.
func clamp01(v float64) float64 {
	if !(v > 0) {
		return 0
	}
	return math.Min(1, v)
}

func ordered(r [2]float64) (float64, float64) {
	if r[0] > r[1] {
		return r[1], r[0]
	}
	return r[0], r[1]
}

// sizeFit scores v against a metric range with the ScoreSize falloff: 0 at 25
// percent beyond the violated bound. constrained is false for a [0, 0] range.
func sizeFit(v float64, r [2]float64) (fit float64, constrained bool) {
	lo, hi := ordered(r)
	if lo == 0 && hi == 0 {
		return 1, false
	}
	return linearFit(v, lo, hi, 0.25*lo, 0.25*hi), true
}

// aspectFit scores an aspect ratio with the ScoreShape falloff: 0 at 50 percent
// of the range width beyond the range, or 50 percent of the value when the
// range is a single point.
func aspectFit(v float64, r [2]float64) float64 {
	lo, hi := ordered(r)
	w := 0.5 * (hi - lo)
	if lo == hi {
		w = 0.5 * math.Abs(lo)
	}
	return linearFit(v, lo, hi, w, w)
}

// ScoreShape grades the proposal's silhouette against the expected solid. It
// is available whenever the silhouette primitive is non-empty, except when the
// expected primitive is not a known solid. The primitive term is 1 for a
// silhouette the solid can cast (see SilhouettesFor), OtherSilhouetteScore for
// "other" or any silhouette name the table does not know, and 0 for a known
// silhouette the solid cannot cast. The score is the primitive term times the
// aspect fit when an aspect range is expected.
func ScoreShape(p Proposal, expect ShapeExpect) Evidence {
	ev := Evidence{Attribute: AttributeShape, Detail: map[string]any{
		"primitive": p.Silhouette.Primitive,
		"aspect":    p.Silhouette.Aspect,
	}}
	if p.Silhouette.Primitive == "" {
		ev.Detail["reason"] = "no silhouette primitive"
		return ev
	}
	term := 1.0
	if expect.Primitive != "" {
		compatible, known := casts(expect.Primitive, p.Silhouette.Primitive)
		if !known {
			ev.Detail["reason"] = "expected primitive " + strconv.Quote(expect.Primitive) + " is not a known solid"
			return ev
		}
		switch {
		case compatible:
			ev.Detail["primitive_match"] = "compatible"
		case knownSilhouette(p.Silhouette.Primitive) && p.Silhouette.Primitive != SilhouetteOther:
			ev.Detail["primitive_match"] = "incompatible"
			term = 0
		default:
			ev.Detail["primitive_match"] = "unclassified"
			term = OtherSilhouetteScore
		}
	}
	ev.Available = true
	ev.Score = term
	if expect.Aspect != nil && term > 0 {
		fit := aspectFit(p.Silhouette.Aspect, *expect.Aspect)
		ev.Detail["aspect_fit"] = fit
		ev.Score = term * fit
	}
	return ev
}

func knownSilhouette(name string) bool {
	switch name {
	case SilhouetteDisc, SilhouetteRect, SilhouetteTrapezoid, SilhouetteOther:
		return true
	}
	return false
}

// ScoreSize grades the proposal's metric extent. It is unavailable without a
// paired depth measurement or when no dimension is constrained. Each
// constrained dimension fits 1 inside its range and falls linearly to 0 at 25
// percent beyond the violated bound; the score is the minimum fit.
func ScoreSize(p Proposal, expect SizeExpect) Evidence {
	ev := Evidence{Attribute: AttributeSize, Detail: map[string]any{}}
	if p.Metric == nil {
		ev.Detail["reason"] = "no depth paired"
		return ev
	}
	ev.Detail["width_m"] = p.Metric.WidthM
	ev.Detail["height_m"] = p.Metric.HeightM
	ev.Detail["distance_m"] = p.Metric.DistanceM
	score, constrainedAny := 1.0, false
	check := func(name string, v float64, r [2]float64) {
		fit, constrained := sizeFit(v, r)
		if !constrained {
			return
		}
		constrainedAny = true
		ev.Detail[name+"_fit"] = fit
		score = math.Min(score, fit)
	}
	check("width", p.Metric.WidthM, expect.WidthM)
	check("height", p.Metric.HeightM, expect.HeightM)
	if expect.DepthM != nil {
		check("distance", p.Metric.DistanceM, *expect.DepthM)
	}
	if !constrainedAny {
		ev.Detail["reason"] = "no constrained dimension"
		return ev
	}
	ev.Available, ev.Score = true, score
	return ev
}

// ScoreColour grades the proposal's palette. For each expected entry it finds
// the closest observed entry by CIE76 delta E; the entry's match is
// max(0, 1 - dE / tolerance) times min(1, observedShare / expectedShare), and
// the score is the share-weighted mean over expected entries. It is unavailable
// when either palette is empty.
func ScoreColour(p Proposal, expect ColourExpect) Evidence {
	ev := Evidence{Attribute: AttributeColour, Detail: map[string]any{}}
	if len(p.Palette) == 0 {
		ev.Detail["reason"] = "no observed palette"
		return ev
	}
	if len(expect.Palette) == 0 {
		ev.Detail["reason"] = "no expected palette"
		return ev
	}
	tolerance := expect.Tolerance
	if !(tolerance > 0) {
		tolerance = DefaultColourTolerance
	}
	totalShare := 0.0
	for _, e := range expect.Palette {
		if e.Share > 0 {
			totalShare += e.Share
		}
	}
	entries := make([]map[string]any, 0, len(expect.Palette))
	sum, weights := 0.0, 0.0
	for _, e := range expect.Palette {
		best, bestDE := 0, math.Inf(1)
		for i, o := range p.Palette {
			if d := DeltaE76(e.Lab, o.Lab); d < bestDE {
				best, bestDE = i, d
			}
		}
		observed := p.Palette[best].Share
		shareFit := 1.0
		if e.Share > 0 {
			shareFit = math.Min(1, math.Max(0, observed)/e.Share)
		}
		match := math.Max(0, 1-bestDE/tolerance) * shareFit
		weight := 1.0
		if totalShare > 0 {
			weight = math.Max(0, e.Share)
		}
		sum += weight * match
		weights += weight
		entries = append(entries, map[string]any{"observed_index": best, "delta_e": bestDE, "observed_share": observed, "match": match})
	}
	ev.Detail["entries"] = entries
	ev.Detail["tolerance"] = tolerance
	ev.Available = true
	if weights > 0 {
		ev.Score = sum / weights
	}
	return ev
}

// ScoreClass grades the proposal against app model detections. Evidence counts
// only when it comes from the proposal's source and lies within window of the
// proposal's boot time (DefaultAssociationWindow when window is not positive).
// With no such evidence the attribute is unavailable. Otherwise the evidence
// with a matching label (and model, when expected) and the best IoU of at
// least ClassIoUMin yields its confidence; ties on IoU go to the higher
// confidence, then the earlier entry. Evidence that exists but does not
// overlap scores 0.
func ScoreClass(p Proposal, expect ClassExpect, evidence []ClassEvidence, window time.Duration) Evidence {
	if window <= 0 {
		window = DefaultAssociationWindow
	}
	ev := Evidence{Attribute: AttributeClass, Detail: map[string]any{}}
	considered, best, bestIoU := 0, -1, 0.0
	for i, c := range evidence {
		if c.SourceID != p.SourceID {
			continue
		}
		gap := c.BootNanos - p.BootNanos
		if gap < 0 {
			gap = -gap
		}
		if gap > int64(window) {
			continue
		}
		considered++
		if expect.Label != "" && c.Label != expect.Label {
			continue
		}
		if expect.Model != "" && c.Model != expect.Model {
			continue
		}
		iou := IoU(p.Box, c.Box)
		if iou < ClassIoUMin {
			continue
		}
		if best < 0 || iou > bestIoU || (iou == bestIoU && c.Confidence > evidence[best].Confidence) {
			best, bestIoU = i, iou
		}
	}
	ev.Detail["considered"] = considered
	if considered == 0 {
		ev.Detail["reason"] = "no class evidence in window"
		return ev
	}
	ev.Available = true
	if best >= 0 {
		c := evidence[best]
		ev.Score = clamp01(c.Confidence)
		ev.Detail["label"] = c.Label
		ev.Detail["model"] = c.Model
		ev.Detail["iou"] = bestIoU
		ev.Detail["confidence"] = c.Confidence
	}
	return ev
}
