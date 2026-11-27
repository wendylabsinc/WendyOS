package geography

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// These tests use geometric examples and independently perturbed rigid
// transforms. They do not use the production uncertainty propagation helpers.
func orientationReviewAngle(v float64) *world.AngularError {
	return &world.AngularError{Known: true, MaxAngleRadians: &v}
}
func orientationReviewRadius(v float64) PositionError {
	return PositionError{Known: true, MaxDistanceMetres: &v}
}
func orientationReviewEdge(id string, parent world.FrameRef, translation [3]float64, angle *world.AngularError, radius float64) world.LocalFrame {
	return world.LocalFrame{Ref: world.FrameRef{ID: id, Revision: "review-v2"}, Parent: parent,
		Translation: translation, RotationXYZW: [4]float64{0, 0, 0, 1}, RotationError: angle,
		UncertaintyMetres: radius, ValidFrom: frameTestTime.Add(-time.Hour), ValidUntil: frameTestTime.Add(time.Hour)}
}
func orientationReviewBound(t *testing.T, got PositionError) float64 {
	t.Helper()
	if !got.Known || got.MaxDistanceMetres == nil || math.IsNaN(*got.MaxDistanceMetres) || math.IsInf(*got.MaxDistanceMetres, 0) || *got.MaxDistanceMetres < 0 {
		t.Fatalf("expected finite known position error: %+v", got)
	}
	return *got.MaxDistanceMetres
}
func orientationReviewUnknown(t *testing.T, got PositionError) {
	t.Helper()
	if got.Known || got.MaxDistanceMetres != nil {
		t.Fatalf("unknown error invented a radius: %+v", got)
	}
}
func orientationReviewNear(t *testing.T, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("got %.17g, want %.17g ± %.3g", got, want, tolerance)
	}
}
func orientationReviewTransformError(t *testing.T, result TransformResult) PositionError {
	t.Helper()
	if result.PositionError == nil {
		t.Fatal("TransformWithError omitted its tagged bound")
	}
	return *result.PositionError
}
func orientationReviewDistance(a, b [3]float64) float64 {
	return math.Hypot(math.Hypot(a[0]-b[0], a[1]-b[1]), a[2]-b[2])
}

func TestOrientationReviewQuarterAndHalfTurnsAtTranslatedOrigin(t *testing.T) {
	for _, angle := range []float64{math.Pi / 2, math.Pi} {
		t.Run(map[bool]string{true: "quarter", false: "half"}[angle == math.Pi/2], func(t *testing.T) {
			f := orientationReviewEdge("sensor", frameTestRoot, [3]float64{1000, -200, 70}, orientationReviewAngle(angle), 0)
			g := mustFrameGraph(t, []world.LocalFrame{f})
			local := [3]float64{3, 4, 0}
			parent := [3]float64{1003, -196, 70}
			// A quarter turn maps (3,4,0) to (-4,3,0); a half turn to
			// (-3,-4,0). The translation does not enlarge the lever arm.
			turned := [3]float64{-4, 3, 0}
			if angle == math.Pi {
				turned = [3]float64{-3, -4, 0}
			}
			want := orientationReviewDistance(local, turned)
			forward, err := g.TransformWithError(local, f.Ref, frameTestRoot, frameTestTime, orientationReviewRadius(0))
			if err != nil {
				t.Fatal(err)
			}
			frameNear(t, forward.Position, parent)
			orientationReviewNear(t, orientationReviewBound(t, orientationReviewTransformError(t, forward)), want, 1e-12)
			inverse, err := g.TransformWithError(parent, frameTestRoot, f.Ref, frameTestTime, orientationReviewRadius(0))
			if err != nil {
				t.Fatal(err)
			}
			frameNear(t, inverse.Position, local)
			orientationReviewNear(t, orientationReviewBound(t, orientationReviewTransformError(t, inverse)), want, 1e-12)
		})
	}
}

type orientationReviewRigid struct {
	r [3][3]float64
	t [3]float64
}

// Rodrigues' construction is used only by the reference perturbation oracle.
func orientationReviewRotation(axis [3]float64, angle float64) [3][3]float64 {
	n := math.Hypot(math.Hypot(axis[0], axis[1]), axis[2])
	x, y, z := axis[0]/n, axis[1]/n, axis[2]/n
	c, s := math.Cos(angle), math.Sin(angle)
	return [3][3]float64{
		{c + x*x*(1-c), x*y*(1-c) - z*s, x*z*(1-c) + y*s},
		{y*x*(1-c) + z*s, c + y*y*(1-c), y*z*(1-c) - x*s},
		{z*x*(1-c) - y*s, z*y*(1-c) + x*s, c + z*z*(1-c)},
	}
}
func (f orientationReviewRigid) apply(p [3]float64, inverse bool) [3]float64 {
	var out [3]float64
	if inverse {
		for i := range p {
			p[i] -= f.t[i]
		}
	}
	for i := range out {
		for j := range p {
			if inverse {
				out[i] += f.r[j][i] * p[j]
			} else {
				out[i] += f.r[i][j] * p[j]
			}
		}
		if !inverse {
			out[i] += f.t[i]
		}
	}
	return out
}

func TestOrientationReviewPerturbedChainContainsIndependentPositions(t *testing.T) {
	parent := orientationReviewEdge("parent", frameTestRoot, [3]float64{4, -2, 8}, orientationReviewAngle(.7), .3)
	child := orientationReviewEdge("child", parent.Ref, [3]float64{-3, 5, 1}, orientationReviewAngle(.3), .2)
	target := orientationReviewEdge("target", frameTestRoot, [3]float64{-2, 3, 6}, orientationReviewAngle(1.2), .4)
	// Distinct right-angle nominal rotations make later lever arms depend on
	// the earlier rotated translations. These matrices are explicit geometric
	// mappings, independent of the production quaternion-to-matrix conversion.
	half := math.Sqrt(.5)
	parent.RotationXYZW = [4]float64{0, 0, half, half}
	child.RotationXYZW = [4]float64{half, 0, 0, half}
	target.RotationXYZW = [4]float64{0, half, 0, half}
	nominal := [][3][3]float64{
		{{1, 0, 0}, {0, 0, -1}, {0, 1, 0}},
		{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}},
		{{0, 0, 1}, {0, 1, 0}, {-1, 0, 0}},
	}
	frames := []world.LocalFrame{child, parent, target}
	g := mustFrameGraph(t, frames)
	point := [3]float64{2, -3, 5}
	result, err := g.TransformWithError(point, child.Ref, target.Ref, frameTestTime, orientationReviewRadius(.6))
	if err != nil {
		t.Fatal(err)
	}
	bound := orientationReviewBound(t, orientationReviewTransformError(t, result))
	rng := rand.New(rand.NewSource(4195))
	vector := func(radius float64) [3]float64 {
		v := [3]float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
		n := math.Hypot(math.Hypot(v[0], v[1]), v[2])
		for i := range v {
			v[i] *= radius / n
		}
		return v
	}
	for trial := range 1000 {
		p := point
		d := vector(.6)
		for i := range p {
			p[i] += d[i]
		}
		for i, f := range frames {
			// Exercise boundary rotations and translations, with different axes
			// at each edge and an inverse target edge. Correlation is unrestricted.
			perturbation := orientationReviewRotation(vector(1), *f.RotationError.MaxAngleRadians)
			actual := orientationReviewRigid{t: f.Translation}
			for row := range 3 {
				for col := range 3 {
					for k := range 3 {
						actual.r[row][col] += perturbation[row][k] * nominal[i][k][col]
					}
				}
			}
			d := vector(f.UncertaintyMetres)
			for j := range actual.t {
				actual.t[j] += d[j]
			}
			p = actual.apply(p, i == 2)
		}
		if distance := orientationReviewDistance(p, result.Position); distance > bound+1e-10 {
			t.Fatalf("perturbation %d escaped reported radius: %.17g > %.17g", trial, distance, bound)
		}
	}
}

func TestOrientationReviewUnknownZeroLeverAndOwnedBounds(t *testing.T) {
	f := orientationReviewEdge("unknown", frameTestRoot, [3]float64{10, 2, -3}, &world.AngularError{Known: false}, .2)
	g := mustFrameGraph(t, []world.LocalFrame{f})
	for _, point := range [][3]float64{{0, 0, 0}, {1, 0, 0}} {
		r, err := g.TransformWithError(point, f.Ref, frameTestRoot, frameTestTime, orientationReviewRadius(.1))
		if err != nil {
			t.Fatal(err)
		}
		if point == [3]float64{} {
			orientationReviewNear(t, orientationReviewBound(t, orientationReviewTransformError(t, r)), .3, 1e-12)
		} else {
			orientationReviewUnknown(t, orientationReviewTransformError(t, r))
		}
	}
	r, err := g.TransformWithError([3]float64{}, f.Ref, frameTestRoot, frameTestTime, PositionError{Known: false})
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewUnknown(t, orientationReviewTransformError(t, r))
	angle := orientationReviewAngle(0)
	f.RotationError = angle
	g = mustFrameGraph(t, []world.LocalFrame{f})
	*angle.MaxAngleRadians = math.Pi
	angle.Known = false
	r, err = g.TransformWithError([3]float64{100, 0, 0}, f.Ref, frameTestRoot, frameTestTime, orientationReviewRadius(.1))
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewNear(t, orientationReviewBound(t, orientationReviewTransformError(t, r)), .3, 1e-12)
	*r.PositionError.MaxDistanceMetres = 900
	r, err = g.TransformWithError([3]float64{100, 0, 0}, f.Ref, frameTestRoot, frameTestTime, orientationReviewRadius(.1))
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewNear(t, orientationReviewBound(t, orientationReviewTransformError(t, r)), .3, 1e-12)
}

func orientationReviewSite(t *testing.T, frames []world.LocalFrame, basis *world.AngularError) *Site {
	t.Helper()
	cfg := SiteConfig{SchemaVersion: SiteSchemaV2, Site: "orientation-review", Revision: "survey-assertion-v2",
		PackageSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Root: frameTestRoot,
		Anchor:         world.GeographicAnchor{LatitudeDegrees: 0, LongitudeDegrees: 0, AltitudeMetres: 0, AltitudeDatum: WGS84Ellipsoid, UncertaintyMetres: .25},
		RootBasisError: basis, Axes: [3]string{"east", "up", "south"}, Placement: "illustrative", PlacementSource: "independent synthetic geometry",
		ValidFrom: frameTestTime.Add(-time.Hour), ValidUntil: frameTestTime.Add(time.Hour), Frames: frames}
	s, err := NewSite(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOrientationReviewSharedAncestorAndGeographicBoundsStaySeparate(t *testing.T) {
	common := orientationReviewEdge("common", frameTestRoot, [3]float64{100, 0, 0}, &world.AngularError{Known: false}, 7)
	left := orientationReviewEdge("left", common.Ref, [3]float64{2, 0, 0}, orientationReviewAngle(0), .2)
	right := orientationReviewEdge("right", common.Ref, [3]float64{-3, 0, 0}, orientationReviewAngle(0), .3)
	s := orientationReviewSite(t, []world.LocalFrame{common, left, right}, orientationReviewAngle(0))
	req := Request{From: left.Ref, To: right.Ref, Position: [3]float64{1, 4, 2}, At: frameTestTime, UncertaintyMetres: .1}
	r, err := s.Convert(req)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{6, 4, 2})
	orientationReviewNear(t, orientationReviewBound(t, r.LocalPositionError), .6, 1e-12)
	orientationReviewUnknown(t, r.GeographicPositionError)
	// A canceled shared ancestor still owns validity; it is not an expiry bypass.
	common.ValidUntil = frameTestTime
	s = orientationReviewSite(t, []world.LocalFrame{common, left, right}, orientationReviewAngle(0))
	if _, err = s.Convert(req); err == nil {
		t.Fatal("expired common ancestry accepted")
	}
}

func TestOrientationReviewRootBasisAndGeographicInput(t *testing.T) {
	s := orientationReviewSite(t, nil, orientationReviewAngle(math.Pi/2))
	point := [3]float64{3, 4, 0}
	r, err := s.Convert(Request{From: frameTestRoot, To: frameTestRoot, Position: point, At: frameTestTime, UncertaintyMetres: .1})
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewNear(t, orientationReviewBound(t, r.LocalPositionError), .1, 1e-12)
	// The root basis rotates around the geographic anchor, not around ECEF zero.
	want := orientationReviewDistance(point, [3]float64{-4, 3, 0}) + .25 + .1
	orientationReviewNear(t, orientationReviewBound(t, r.GeographicPositionError), want, 1e-10)
	back, err := s.Geographic(GeoRequest{Coordinate: r.Geographic, To: frameTestRoot, At: frameTestTime, UncertaintyMetres: .1})
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewNear(t, orientationReviewBound(t, back.GeographicPositionError), .1, 1e-12)
	orientationReviewNear(t, orientationReviewBound(t, back.LocalPositionError), want, 1e-6)
	unknown := orientationReviewSite(t, nil, &world.AngularError{Known: false})
	back, err = unknown.Geographic(GeoRequest{Coordinate: r.Geographic, To: frameTestRoot, At: frameTestTime, UncertaintyMetres: .1})
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewUnknown(t, back.LocalPositionError)
	orientationReviewNear(t, orientationReviewBound(t, back.GeographicPositionError), .1, 1e-12)
}

func TestOrientationReviewStrictVersionsAndTaggedRoundTrip(t *testing.T) {
	v1, _ := fixture(t)
	var original map[string]any
	if err := json.Unmarshal(v1, &original); err != nil {
		t.Fatal(err)
	}
	v2Document := func() map[string]any {
		var d map[string]any
		_ = json.Unmarshal(v1, &d)
		d["schema_version"] = SiteSchemaV2
		d["root_basis_error"] = map[string]any{"known": true, "max_angle_radians": 0.0}
		for _, frame := range d["frames"].([]any) {
			frame.(map[string]any)["rotation_error"] = map[string]any{"known": true, "max_angle_radians": 0.0}
		}
		return d
	}
	for _, unknown := range []bool{false, true} {
		d := v2Document()
		if unknown {
			d["root_basis_error"] = map[string]any{"known": false}
			d["frames"].([]any)[0].(map[string]any)["rotation_error"] = map[string]any{"known": false}
		}
		raw, _ := json.Marshal(d)
		s, err := DecodeSite(raw)
		if err != nil {
			t.Fatal(err)
		}
		copyRaw, err := json.Marshal(s.Config())
		if err != nil {
			t.Fatal(err)
		}
		var round map[string]any
		_ = json.Unmarshal(copyRaw, &round)
		if !reflect.DeepEqual(d, round) {
			t.Fatal("v2 roundtrip changed known/unknown or exact zero tags")
		}
		if _, err = DecodeSite(copyRaw); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := DecodeSite(v1)
	if err != nil {
		t.Fatal(err)
	}
	legacyRaw, _ := json.Marshal(legacy.Config())
	var legacyRound map[string]any
	_ = json.Unmarshal(legacyRaw, &legacyRound)
	if !reflect.DeepEqual(original, legacyRound) {
		t.Fatal("v1 serialization added orientation assumptions")
	}
	legacyConfig := legacy.Config()
	legacyResult, err := legacy.Convert(Request{From: legacyConfig.Root, To: legacyConfig.Root, Position: [3]float64{1, 0, 0}, At: legacyConfig.ValidFrom, UncertaintyMetres: .1})
	if err != nil {
		t.Fatal(err)
	}
	orientationReviewNear(t, orientationReviewBound(t, legacyResult.LocalPositionError), .1, 1e-12)
	orientationReviewUnknown(t, legacyResult.GeographicPositionError)
	resultRaw, err := json.Marshal(legacyResult)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(resultRaw, []byte(`"geographic_position_error":{"known":false}`)) || !bytes.Contains(resultRaw, []byte(`"local_position_error":{"known":true,"max_distance_metres":0.1}`)) {
		t.Fatal("v1 result omitted known/unknown tags")
	}
	badTags := []any{nil, map[string]any{}, map[string]any{"known": true}, map[string]any{"known": false, "max_angle_radians": 0.0},
		map[string]any{"known": 1}, map[string]any{"known": true, "max_angle_radians": nil},
		map[string]any{"known": true, "max_angle_radians": -1.0}, map[string]any{"known": true, "max_angle_radians": math.Nextafter(math.Pi, math.Inf(1))},
		map[string]any{"known": true, "max_angle_radians": "0"}, map[string]any{"known": true, "max_angle_radians": 0.0, "extra": 0}}
	for _, location := range []string{"root", "edge"} {
		for i, tag := range badTags {
			d := v2Document()
			object, key := d, "root_basis_error"
			if location == "edge" {
				object, key = d["frames"].([]any)[0].(map[string]any), "rotation_error"
			}
			object[key] = tag
			raw, _ := json.Marshal(d)
			if _, err := DecodeSite(raw); err == nil {
				t.Fatalf("accepted %s invalid tag %d", location, i)
			}
		}
		for _, version := range []string{SiteSchema, SiteSchemaV2} {
			d := v2Document()
			d["schema_version"] = version
			if version == SiteSchemaV2 {
				if location == "root" {
					delete(d, "root_basis_error")
				} else {
					delete(d["frames"].([]any)[0].(map[string]any), "rotation_error")
				}
			}
			raw, _ := json.Marshal(d)
			if _, err := DecodeSite(raw); err == nil {
				t.Fatalf("accepted incompatible %s %s fields", version, location)
			}
		}
	}
	raw, _ := json.Marshal(v2Document())
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"known":true`), []byte(`"known":true,"known":false`), 1),
		bytes.Replace(raw, []byte(`"known":true`), []byte(`"known":true,"kno\u0077n":false`), 1),
		bytes.Replace(raw, []byte(`"max_angle_radians":0`), []byte(`"max_angle_radians":1e999`), 1),
		append(append([]byte{}, raw...), []byte(` {}`)...),
	} {
		if _, err := DecodeSite(invalid); err == nil {
			t.Fatal("invalid tagged JSON accepted")
		}
	}
}

func TestOrientationReviewSiteConfigOwnership(t *testing.T) {
	f := orientationReviewEdge("owned", frameTestRoot, [3]float64{1, 2, 3}, orientationReviewAngle(.1), .2)
	s := orientationReviewSite(t, []world.LocalFrame{f}, orientationReviewAngle(.3))
	req := Request{From: f.Ref, To: frameTestRoot, Position: [3]float64{2, 3, 4}, At: frameTestTime, UncertaintyMetres: .1}
	before, err := s.Convert(req)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	*cfg.RootBasisError.MaxAngleRadians = math.Pi
	*cfg.Frames[0].RotationError.MaxAngleRadians = math.Pi
	cfg.Frames[0].Translation = [3]float64{20, 30, 40}
	after, err := s.Convert(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("changing returned config changed immutable site")
	}
	again := s.Config()
	orientationReviewNear(t, *again.RootBasisError.MaxAngleRadians, .3, 0)
	orientationReviewNear(t, *again.Frames[0].RotationError.MaxAngleRadians, .1, 0)
}
