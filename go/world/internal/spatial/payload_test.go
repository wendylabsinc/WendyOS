package spatial

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func sampleTile() MapTile {
	t := MapTile{Floor: "ground", IX: -2, IZ: 3, Side: 4, CellSizeM: 0.05, OriginXZ: [2]float64{-0.4, 0.6}, LogOddsScale: 0.1,
		Cells: make([]byte, 16), Versions: map[string]uint64{"robot-1": 7, "robot-2": 3}}
	t.Cells[5] = byte(40)
	t.Cells[6] = 0xd8 // int8 −40: free
	t.Seal(0)
	return t
}

func samplePortal() Portal {
	a, b := [3]float64{10, 0, 4}, [3]float64{13, 3, 4}
	return Portal{ID: "stairs-1", Kind: PortalStairs, State: PortalOpen, WidthM: 1.2, Confidence: 0.9,
		A: PortalEnd{Floor: "ground", Region: "hall", PositionM: a}, B: PortalEnd{Floor: "upper", PositionM: b},
		Location: Enclose(1.5, geom.Vec2{X: 10, Z: 4}, geom.Vec2{X: 13, Z: 4})}
}

// samplePayloads returns valid payloads covering every component type; the
// second Calibration is the mounted variant, which has no site location.
func samplePayloads() []Payload {
	camPose, err := geom.LookAt(geom.Vec3{X: 1, Y: 2.6, Z: 2}, geom.Vec3{X: 4, Y: 0, Z: 5}, geom.Vec3{Y: 1})
	if err != nil {
		panic(err)
	}
	fixed := At(camPose.T, 0.03)
	polygon := []geom.Vec2{{X: 0, Z: 0}, {X: 5, Z: 0}, {X: 5, Z: 4}, {X: 0, Z: 4}}
	manifest := sampleMeasurements()[9].(SensorManifest)
	scan, err := Compact(sampleMeasurements()[0].(Scan))
	if err != nil {
		panic(err)
	}
	from, to := [3]float64{2, 0, 2}, [3]float64{3, 0, 2.5}
	return []Payload{
		Calibration{Device: "cam-unit-1", Sensor: "cam", Kind: KindCamera, Intrinsics: &geom.Intrinsics{FX: 600, FY: 601, CX: 320, CY: 240, K1: -0.2, K2: 0.05, Width: 640, Height: 480},
			SensorToParent: camPose, Parent: ParentSite, TranslationSigmaM: 0.02, RotationSigmaRad: 0.004, ReprojectionRMSE: 0.6,
			Residuals: ResidualStats{Count: 240, RMSE: 0.6, Median: 0.5, Max: 1.9}, ResidualUnit: "px", Method: "fiducial-resection",
			Revision: "r3", State: CalibrationValid, TimeOffset: 35 * time.Millisecond, TimeOffsetSigma: 4 * time.Millisecond, Location: &fixed},
		Calibration{Device: "robot-1", Sensor: "lidar", Kind: KindLidar2D, SensorToParent: geom.Pose3FromPose2(geom.Pose2{X: 0.12, Z: -0.03, Yaw: 0.05}, 0.3),
			Parent: ParentDeviceBase, ParentDevice: "robot-1", TranslationSigmaM: 0.01, RotationSigmaRad: 0.01, Residuals: ResidualStats{Count: 80, RMSE: 0.02, Median: 0.015, Max: 0.06},
			ResidualUnit: "m", Method: "hand-eye", Revision: "r1", State: CalibrationEstimating, TimeOffset: -20 * time.Millisecond},
		DevicePose{Device: "robot-1", Floor: "ground", FrameID: "site", Location: At(geom.Vec3{X: 3, Y: 0, Z: 4}, 0.08), Yaw: 1.2,
			Covariance: geom.Mat3{0.001, 0.0002, 0, 0.0002, 0.002, 0, 0, 0, 0.0005}, Method: "scan-to-map", Score: 0.93},
		SensorRegistration{Manifest: manifest, State: RegistrationRegistered, CalibrationRevision: "r1"},
		Anchor{ID: "charging-dock", Name: "Charging dock", Floor: "ground", Location: At(geom.Vec3{X: 1, Y: 0, Z: 1}, 0.05), Yaw: -0.5,
			Signature: []byte{1, 2, 3, 250}, Keyframes: []KeyframeRef{{Device: "robot-1", Sequence: 12, Offset: geom.Pose2{X: 0.5, Z: 0.1, Yaw: 0.2}}}, Revision: "a2"},
		Track{ID: "t-17", Class: "person", ClassConfidence: 0.8, Floor: "ground", Location: At(geom.Vec3{X: 2, Y: 0, Z: 3}, 0.15), VelocityMPS: geom.Vec2{X: 0.8, Z: -0.1},
			Covariance: [16]float64{0.01, 0, 0, 0, 0, 0.01, 0, 0, 0, 0, 0.05, 0, 0, 0, 0, 0.05}, State: TrackConfirmed, FirstSeen: t0, LastSeen: t0.Add(4 * time.Second),
			Sensors: []SensorRef{{Device: "cam-unit-1", Sensor: "cam"}, {Device: "robot-1", Sensor: "lidar"}}, RadiusM: 0.25},
		sampleTile(),
		Region{ID: "room-2", Label: "kitchen", LabelConfidence: 0.85, Floor: "ground", Polygon: polygon, AreaM2: 20,
			CellCounts: map[string]int{"free": 7000, "wall": 600, "furniture": 400}, Doors: []string{"door-3"}, Location: Enclose(0, polygon...)},
		ChangeEvent{ID: "c-4", Kind: ChangeMoved, Floor: "ground", Location: At(geom.Vec3{X: 3, Y: 0, Z: 2.5}, 0.1), Extent: Extent{RadiusM: 0.4}, Subject: "chair",
			FromM: &from, ToM: &to, Confidence: 0.92, FirstObserved: t0, LastObserved: t0.Add(time.Minute)},
		FloorInfo{Floor: "ground", Name: "Ground floor", ElevationM: 0, HeightM: 3, Portals: []Portal{samplePortal()}},
		samplePortal(),
		GeoReference{Origin: world.GeographicAnchor{LatitudeDegrees: 52.37, LongitudeDegrees: 4.89, AltitudeMetres: 2, AltitudeDatum: "WGS84-ellipsoid", UncertaintyMetres: 0.8},
			SiteYaw: 0.4, YawSigmaRad: 0.02, Method: "gnss-odometry", Fixes: 60, Location: At(geom.Vec3{}, 0.8)},
		Keyframe{Device: "robot-1", Sequence: 12, Floor: "ground", Time: t0, Local: geom.Pose2{X: 4, Z: 1, Yaw: 0.3},
			Covariance: geom.Mat3{0.01, 0, 0, 0, 0.01, 0, 0, 0, 0.001}, SensorToBase: geom.Pose2{X: 0.12, Z: -0.03, Yaw: 0.05}, Scan: scan},
		Landmark{ID: "wall-7", Floor: "ground", TagToSite: geom.Pose3{R: geom.RotY(-math.Pi / 2), T: geom.Vec3{X: 0, Y: 1.5, Z: 2}},
			Location: At(geom.Vec3{X: 0, Y: 1.5, Z: 2}, 0.02), RotationSigmaRad: 0.01, Observations: 40, Revision: "l1"},
		Object{ID: "o-9", Class: "table", Confidence: 0.7, Floor: "ground", Location: At(geom.Vec3{X: 2.5, Y: 0, Z: 1.5}, 0.1),
			Extent: Extent{Polygon: []geom.Vec2{{X: 2, Z: 1}, {X: 3, Z: 1}, {X: 3, Z: 2}, {X: 2, Z: 2}}}, FirstSeen: t0, LastSeen: t0.Add(time.Hour)},
		FrameAlignment{ChildFrameID: "map/robot-1", ParentFrameID: "site", ChildToParent: geom.Pose2{X: 12, Z: -3, Yaw: 1.1}, ElevationM: 0.02,
			Covariance: geom.Mat3{0.004, 0.001, 0, 0.001, 0.003, 0, 0, 0, 0.0002}, Inliers: 14, RMSE: 0.04, Revision: "f3"},
		sampleLayer(true),
		sampleLayer(false),
	}
}

// sampleLayer returns a valid gas-field tile or a valid semantic class tile.
func sampleLayer(scalar bool) LayerTile {
	t := LayerTile{Floor: "ground", Layer: "semantic", IX: 1, IZ: -1, Side: 4, CellSizeM: 0.05, OriginXZ: TileOrigin(1, -1, 4, 0.05),
		Cells: make([]byte, 16), ClassNames: []string{"free", "wall", "furniture"}}
	t.Cells[5], t.Cells[6] = 2, 3
	if scalar {
		t.Layer, t.ClassNames, t.Unit, t.ValueOffset, t.ValueStep, t.ValueSigma = "gas.co2", nil, "ppm", 400, 4, 15
		t.Cells[5], t.Cells[6] = t.ValueCode(612), t.ValueCode(5000)
	}
	t.Seal(0)
	return t
}

func TestTypesRegistry(t *testing.T) {
	types := Types()
	seen := map[string]bool{}
	for i, typ := range types {
		if !strings.HasPrefix(typ, "wendy.spatial.") || !strings.HasSuffix(typ, ".v1") || seen[typ] {
			t.Fatalf("type %q is malformed or duplicated", typ)
		}
		if i > 0 && typ <= types[i-1] {
			t.Fatal("Types must be sorted")
		}
		seen[typ] = true
	}
	covered := map[string]bool{}
	for _, p := range samplePayloads() {
		if !seen[p.Type()] {
			t.Fatalf("%T has unregistered type %q", p, p.Type())
		}
		covered[p.Type()] = true
	}
	if len(covered) != len(types) {
		t.Fatalf("%d of %d types have a sample payload", len(covered), len(types))
	}
}

// Every payload must round trip through JSON unchanged, and one with a place
// must expose it the way selection.Region.Classify reads it: a three-number
// "position_m" and a non-null "uncertainty_m".
func TestPayloadJSON(t *testing.T) {
	located := map[string]bool{TypeDevicePose: true, TypeAnchor: true, TypeTrack: true, TypeMapTile: true, TypeRegion: true,
		TypeChangeEvent: true, TypePortal: true, TypeGeoReference: true, TypeLandmark: true, TypeObject: true, TypeLayerTile: true}
	for i, p := range samplePayloads() {
		if err := p.Validate(); err != nil {
			t.Fatalf("%T: %v", p, err)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		back := reflect.New(reflect.TypeOf(p))
		if err := json.Unmarshal(raw, back.Interface()); err != nil {
			t.Fatalf("%T: %v", p, err)
		}
		again, err := json.Marshal(back.Elem().Interface())
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatalf("%T does not round trip:\n%s\n%s", p, raw, again)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		_, hasPosition := obj["position_m"]
		_, hasUncertainty := obj["uncertainty_m"]
		want := located[p.Type()] || i == 0 // the site-fixed calibration is located too
		if hasPosition != want || hasUncertainty != want {
			t.Fatalf("%T: position_m present=%v uncertainty_m present=%v, want %v", p, hasPosition, hasUncertainty, want)
		}
		if !want {
			continue
		}
		var position []float64
		var radius float64
		if err := json.Unmarshal(obj["position_m"], &position); err != nil || len(position) != 3 {
			t.Fatalf("%T: position_m is not [x, y, z]: %s", p, obj["position_m"])
		}
		if err := json.Unmarshal(obj["uncertainty_m"], &radius); err != nil || radius < 0 || bytes.Equal(obj["uncertainty_m"], []byte("null")) {
			t.Fatalf("%T: uncertainty_m: %s", p, obj["uncertainty_m"])
		}
	}
}

func TestLocationHelpers(t *testing.T) {
	l := At(geom.Vec3{X: 1, Y: 2, Z: 3}, 0.5)
	if l.PositionM != [3]float64{1, 2, 3} || l.Point() != (geom.Vec3{X: 1, Y: 2, Z: 3}) || l.UncertaintyM != 0.5 {
		t.Fatalf("At: %+v", l)
	}
	e := Enclose(1.5, geom.Vec2{X: 0, Z: 0}, geom.Vec2{X: 6, Z: 8})
	if e.PositionM != [3]float64{3, 1.5, 4} || math.Abs(e.UncertaintyM-5) > 1e-12 {
		t.Fatalf("Enclose: %+v", e)
	}
	if got := Enclose(2); got.PositionM != [3]float64{0, 2, 0} || got.UncertaintyM != 0 {
		t.Fatalf("Enclose of nothing: %+v", got)
	}
	for _, bad := range []Location{{PositionM: [3]float64{math.NaN(), 0, 0}}, {UncertaintyM: -1}, {PositionM: [3]float64{2e6, 0, 0}}, {UncertaintyM: math.Inf(1)}} {
		if bad.check() == nil {
			t.Fatalf("invalid location accepted: %+v", bad)
		}
	}
}

func TestPayloadValidation(t *testing.T) {
	s := samplePayloads()
	fixed, mounted, pose, reg, anchor := s[0].(Calibration), s[1].(Calibration), s[2].(DevicePose), s[3].(SensorRegistration), s[4].(Anchor)
	track, tile, region, change, floor := s[5].(Track), s[6].(MapTile), s[7].(Region), s[8].(ChangeEvent), s[9].(FloorInfo)
	portal, geo, keyframe, landmark, object := s[10].(Portal), s[11].(GeoReference), s[12].(Keyframe), s[13].(Landmark), s[14].(Object)
	alignment, gas, classes := s[15].(FrameAlignment), s[16].(LayerTile), s[17].(LayerTile)
	elsewhere := At(geom.Vec3{X: 9, Y: 9, Z: 9}, 0.1)
	for name, p := range map[string]Payload{
		"calibration without location":      mutated(fixed, func(c *Calibration) { c.Location = nil }),
		"calibration location off":          mutated(fixed, func(c *Calibration) { c.Location = &elsewhere }),
		"calibration skewed rotation":       mutated(fixed, func(c *Calibration) { c.SensorToParent.R[0] += 0.01 }),
		"valid camera without intrinsics":   mutated(fixed, func(c *Calibration) { c.Intrinsics = nil }),
		"calibration state":                 mutated(fixed, func(c *Calibration) { c.State = "ok" }),
		"calibration revision":              mutated(fixed, func(c *Calibration) { c.Revision = "" }),
		"calibration negative rmse":         mutated(fixed, func(c *Calibration) { c.ReprojectionRMSE = -1 }),
		"site calibration with parent":      mutated(fixed, func(c *Calibration) { c.ParentDevice = "robot-1" }),
		"mounted calibration located":       mutated(mounted, func(c *Calibration) { c.Location = &elsewhere }),
		"mounted calibration no carrier":    mutated(mounted, func(c *Calibration) { c.ParentDevice = "" }),
		"lidar with intrinsics":             mutated(mounted, func(c *Calibration) { c.Intrinsics = fixed.Intrinsics }),
		"calibration parent":                mutated(mounted, func(c *Calibration) { c.Parent = "" }),
		"pose without frame":                mutated(pose, func(p *DevicePose) { p.FrameID = "" }),
		"pose asymmetric covariance":        mutated(pose, func(p *DevicePose) { p.Covariance[1] = 0.5 }),
		"pose negative variance":            mutated(pose, func(p *DevicePose) { p.Covariance[8] = -1 }),
		"pose score":                        mutated(pose, func(p *DevicePose) { p.Score = 1.2 }),
		"pose NaN yaw":                      mutated(pose, func(p *DevicePose) { p.Yaw = math.NaN() }),
		"pose negative uncertainty":         mutated(pose, func(p *DevicePose) { p.UncertaintyM = -0.1 }),
		"registered without revision":       mutated(reg, func(r *SensorRegistration) { r.CalibrationRevision = "" }),
		"registration state":                mutated(reg, func(r *SensorRegistration) { r.State = "done" }),
		"registration manifest":             mutated(reg, func(r *SensorRegistration) { r.Manifest.Model = "" }),
		"anchor id":                         mutated(anchor, func(a *Anchor) { a.ID = "dock one" }),
		"anchor signature too large":        mutated(anchor, func(a *Anchor) { a.Signature = make([]byte, MaxSignatureBytes+1) }),
		"anchor binding":                    mutated(anchor, func(a *Anchor) { a.Keyframes = []KeyframeRef{{}} }),
		"track state":                       mutated(track, func(t *Track) { t.State = "" }),
		"track times reversed":              mutated(track, func(t *Track) { t.LastSeen = t.FirstSeen.Add(-time.Second) }),
		"track without first seen":          mutated(track, func(t *Track) { t.FirstSeen = time.Time{} }),
		"track covariance":                  mutated(track, func(t *Track) { t.Covariance[0] = math.NaN() }),
		"track sensor":                      mutated(track, func(t *Track) { t.Sensors = []SensorRef{{Device: "d"}} }),
		"tile cell count":                   mutated(tile, func(t *MapTile) { t.Cells = t.Cells[:15] }),
		"tile too large":                    mutated(tile, func(t *MapTile) { t.Side = MaxTileSide + 1; t.Cells = make([]byte, t.Side*t.Side); t.Seal(0) }),
		"tile stale digest":                 mutated(tile, func(t *MapTile) { t.Cells = append([]byte(nil), t.Cells...); t.Cells[0] = 9 }),
		"tile no versions":                  mutated(tile, func(t *MapTile) { t.Versions = nil; t.Seal(0) }),
		"tile zero version":                 mutated(tile, func(t *MapTile) { t.Versions = map[string]uint64{"robot-1": 0}; t.Seal(0) }),
		"tile radius too small":             mutated(tile, func(t *MapTile) { t.UncertaintyM = 0.01 }),
		"tile cell size":                    mutated(tile, func(t *MapTile) { t.CellSizeM = 0; t.Seal(0) }),
		"region two vertices":               mutated(region, func(r *Region) { r.Polygon = r.Polygon[:2] }),
		"region not covered":                mutated(region, func(r *Region) { r.UncertaintyM = 1 }),
		"region label confidence":           mutated(region, func(r *Region) { r.LabelConfidence = -0.1 }),
		"region negative count":             mutated(region, func(r *Region) { r.CellCounts = map[string]int{"free": -1} }),
		"region door id":                    mutated(region, func(r *Region) { r.Doors = []string{""} }),
		"move without endpoints":            mutated(change, func(c *ChangeEvent) { c.FromM = nil }),
		"appearance with endpoints":         mutated(change, func(c *ChangeEvent) { c.Kind = ChangeAppeared }),
		"change kind":                       mutated(change, func(c *ChangeEvent) { c.Kind = "vanished" }),
		"change extent radius and polygon":  mutated(change, func(c *ChangeEvent) { c.Extent.Polygon = region.Polygon }),
		"change times":                      mutated(change, func(c *ChangeEvent) { c.LastObserved = time.Time{} }),
		"floor with foreign portal":         mutated(floor, func(f *FloorInfo) { f.Floor = "roof" }),
		"floor with invalid portal":         mutated(floor, func(f *FloorInfo) { f.Portals = []Portal{{ID: "p"}} }),
		"floor negative height":             mutated(floor, func(f *FloorInfo) { f.HeightM = -1 }),
		"portal kind":                       mutated(portal, func(p *Portal) { p.Kind = "window" }),
		"portal state":                      mutated(portal, func(p *Portal) { p.State = "" }),
		"portal end floor":                  mutated(portal, func(p *Portal) { p.B.Floor = "" }),
		"portal ends not covered":           mutated(portal, func(p *Portal) { p.UncertaintyM = 0.5 }),
		"geo reference latitude":            mutated(geo, func(g *GeoReference) { g.Origin.LatitudeDegrees = 120 }),
		"geo reference without datum":       mutated(geo, func(g *GeoReference) { g.Origin.AltitudeDatum = "" }),
		"geo reference method":              mutated(geo, func(g *GeoReference) { g.Method = "" }),
		"keyframe without time":             mutated(keyframe, func(k *Keyframe) { k.Time = time.Time{} }),
		"keyframe without scan":             mutated(keyframe, func(k *Keyframe) { k.Scan.RangesMM = nil }),
		"keyframe scan range":               mutated(keyframe, func(k *Keyframe) { k.Scan.RangeMaxM = 100 }),
		"keyframe NaN pose":                 mutated(keyframe, func(k *Keyframe) { k.Local.X = math.NaN() }),
		"landmark revision":                 mutated(landmark, func(l *Landmark) { l.Revision = "" }),
		"landmark negative observations":    mutated(landmark, func(l *Landmark) { l.Observations = -1 }),
		"landmark without rotation":         mutated(landmark, func(l *Landmark) { l.TagToSite.R = geom.Mat3{} }),
		"landmark pose off position":        mutated(landmark, func(l *Landmark) { l.TagToSite.T.X += 0.01 }),
		"landmark rotation sigma":           mutated(landmark, func(l *Landmark) { l.RotationSigmaRad = -1 }),
		"tile off the tiling":               mutated(tile, func(t *MapTile) { t.OriginXZ[0] += 0.01; t.Seal(0) }),
		"tile index against origin":         mutated(tile, func(t *MapTile) { t.IX++; t.Seal(0) }),
		"pose indefinite covariance":        mutated(pose, func(p *DevicePose) { p.Covariance = geom.Mat3{1, 5, 0, 5, 1, 0, 0, 0, 1} }),
		"alignment onto itself":             mutated(alignment, func(a *FrameAlignment) { a.ChildFrameID = a.ParentFrameID }),
		"alignment frame with a space":      mutated(alignment, func(a *FrameAlignment) { a.ChildFrameID = "map robot" }),
		"alignment covariance":              mutated(alignment, func(a *FrameAlignment) { a.Covariance[1] = 0.5 }),
		"alignment NaN yaw":                 mutated(alignment, func(a *FrameAlignment) { a.ChildToParent.Yaw = math.NaN() }),
		"alignment revision":                mutated(alignment, func(a *FrameAlignment) { a.Revision = "" }),
		"layer off the tiling":              mutated(gas, func(t *LayerTile) { t.OriginXZ[1] += 0.01; t.Seal(0) }),
		"layer cell count":                  mutated(gas, func(t *LayerTile) { t.Cells = t.Cells[:15] }),
		"layer name":                        mutated(gas, func(t *LayerTile) { t.Layer = "gas/co2" }),
		"scalar layer without unit":         mutated(gas, func(t *LayerTile) { t.Unit = "" }),
		"scalar layer without step":         mutated(gas, func(t *LayerTile) { t.ValueStep = 0 }),
		"layer not covered":                 mutated(gas, func(t *LayerTile) { t.UncertaintyM = 0.01 }),
		"class layer with a unit":           mutated(classes, func(t *LayerTile) { t.Unit = "ppm" }),
		"class layer unnamed class":         mutated(classes, func(t *LayerTile) { t.Cells = append([]byte(nil), t.Cells...); t.Cells[0] = 4 }),
		"class layer bad name":              mutated(classes, func(t *LayerTile) { t.ClassNames = []string{"free", "", "x"} }),
		"object without class":              mutated(object, func(o *Object) { o.Class = "" }),
		"object confidence":                 mutated(object, func(o *Object) { o.Confidence = 2 }),
		"object position out of bounds":     mutated(object, func(o *Object) { o.PositionM[0] = 2e6 }),
		"object extent polygon two corners": mutated(object, func(o *Object) { o.Extent.Polygon = o.Extent.Polygon[:2] }),
	} {
		if p.Validate() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestMapTile(t *testing.T) {
	tile := sampleTile()
	if tile.Point().Dist(geom.Vec3{X: -0.3, Z: 0.7}) > 1e-12 {
		t.Fatalf("tile centre: %v", tile.PositionM)
	}
	if want := 0.1 * math.Sqrt2; math.Abs(tile.UncertaintyM-want) > 1e-12 {
		t.Fatalf("tile radius %g, want the half diagonal %g", tile.UncertaintyM, want)
	}
	if tile.EntityKey() != "tile/ground/-2/3" {
		t.Fatalf("tile key %q", tile.EntityKey())
	}
	// The digest covers content, not where the tile is published from, and
	// does not depend on map insertion order.
	other := sampleTile()
	other.Versions = map[string]uint64{"robot-2": 3, "robot-1": 7}
	other.Seal(2.5)
	if other.Digest != tile.Digest {
		t.Fatal("digest must not depend on elevation or map order")
	}
	other.Versions["robot-2"] = 4
	if other.ContentDigest() == tile.Digest {
		t.Fatal("digest must cover the version vector")
	}
	other = sampleTile()
	other.Cells[0] = 1
	if other.ContentDigest() == tile.Digest {
		t.Fatal("digest must cover the cells")
	}
}

// The largest legal tile, with incompressible content and a full version
// vector, must stay comfortably inside the payload bound.
func TestMapTileSizeBound(t *testing.T) {
	const worstCaseBytes = 32 << 10
	r := rand.New(rand.NewPCG(1, 2))
	const ix, iz, cell = -78125, -414063, 0.0123456789012345 // origins near −123456 m and −654321 m
	tile := MapTile{Floor: FloorID(strings.Repeat("f", MaxIDLength)), IX: ix, IZ: iz, Side: MaxTileSide, CellSizeM: cell,
		OriginXZ: TileOrigin(ix, iz, MaxTileSide, cell), LogOddsScale: 0.0123456789012345, Cells: make([]byte, MaxTileSide*MaxTileSide), Versions: map[string]uint64{}}
	for i := range tile.Cells {
		tile.Cells[i] = byte(r.UintN(256))
	}
	for i := 0; i < MaxTileSources; i++ {
		tile.Versions[strings.Repeat("s", MaxIDLength-2)+string(rune('A'+i/26))+string(rune('a'+i%26))] = math.MaxUint64
	}
	tile.Seal(-1234.56789)
	if err := tile.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tile)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > worstCaseBytes || worstCaseBytes > MaxPayloadBytes || MaxPayloadBytes >= 64<<10 {
		t.Fatalf("worst-case tile is %d bytes; bound %d, payload limit %d", len(raw), worstCaseBytes, MaxPayloadBytes)
	}
}

func TestCompactScan(t *testing.T) {
	const quantisationM = 0.0005
	s := Scan{Header: header("lidar"), AngleMin: -1, AngleIncrement: 0.01, RangeMin: 0.1, RangeMax: 80, RangeSigma: 0.015,
		Ranges: []float64{1.23456, RangeNoReturn, RangeInvalid, 0.05, 70, 65.534, 0.1, 81}}
	c, err := Compact(s)
	if err != nil {
		t.Fatal(err)
	}
	if c.RangeMaxM != 65.534 {
		t.Fatalf("range max must be clamped to the encodable range: %g", c.RangeMaxM)
	}
	back := c.Expand(s.Header, s.RangeSigma)
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []BeamState{BeamHit, BeamNoReturn, BeamInvalid, BeamInvalid, BeamNoReturn, BeamHit, BeamHit, BeamNoReturn}
	for i, w := range want {
		angle, r, state := back.Beam(i)
		if state != w {
			t.Fatalf("beam %d: state %d, want %d", i, state, w)
		}
		if angle != s.Angle(i) {
			t.Fatalf("beam %d: angle changed", i)
		}
		if w == BeamHit && math.Abs(r-s.Ranges[i]) > quantisationM+1e-12 {
			t.Fatalf("beam %d: range %g, want %g within half a millimetre", i, r, s.Ranges[i])
		}
	}
	// A hit just inside a RangeMax that is not a whole millimetre must not
	// round up past it: as a no-return it would clear cells through the wall.
	edge := s
	edge.RangeMax, edge.Ranges = 12.3456, []float64{12.3455, 12.3456, 12.3457}
	c, err = Compact(edge)
	if err != nil {
		t.Fatal(err)
	}
	back = c.Expand(edge.Header, edge.RangeSigma)
	for i, w := range []BeamState{BeamHit, BeamHit, BeamNoReturn} {
		if _, r, state := back.Beam(i); state != w || (w == BeamHit && math.Abs(r-edge.Ranges[i]) > 2*quantisationM) {
			t.Fatalf("beam %d at the range limit: state %d range %g, want state %d", i, state, r, w)
		}
	}
	// Any valid scan can be a keyframe, and the largest fits a component.
	full := s
	full.Ranges = make([]float64, MaxScanBeams)
	for i := range full.Ranges {
		full.Ranges[i] = 65.5
	}
	c, err = Compact(full)
	if err != nil || MaxKeyframeBeams != MaxScanBeams {
		t.Fatalf("a full scan must compact: %v", err)
	}
	keyframe := Keyframe{Device: "robot-1", Sequence: math.MaxUint64, Floor: "ground", Time: t0, Scan: c}
	if raw, err := json.Marshal(keyframe); err != nil || keyframe.Validate() != nil || len(raw) > MaxPayloadBytes*2/3 {
		t.Fatalf("largest keyframe is %d bytes (%v), payload limit %d", len(raw), err, MaxPayloadBytes)
	}
	big := s
	big.Ranges = make([]float64, MaxScanBeams+1)
	if _, err := Compact(big); err == nil {
		t.Fatal("oversized scan compacted")
	}
	big.Ranges = nil
	if _, err := Compact(big); err == nil {
		t.Fatal("invalid scan compacted")
	}
}

// SiteYaw is documented against geography.Tangent (x east, y up, z south).
// Check the claim against that package: north and east are taken from where
// it places geographic points slightly north and east of the origin.
func TestGeoReferenceHeading(t *testing.T) {
	const bearingTol = 1e-6 // radians
	for _, yaw := range []float64{0, 0.4, -math.Pi / 2, 2.5} {
		g := samplePayloads()[11].(GeoReference)
		g.SiteYaw = yaw
		g.Location = At(geom.Vec3{X: 20, Y: 0, Z: -7}, 0.5) // the anchored point is not the site origin
		tangent, err := geography.NewTangent(g.Origin)
		if err != nil {
			t.Fatal(err)
		}
		axis := func(dLat, dLon float64) geom.Vec2 {
			local, err := tangent.ToLocal(geography.Coordinate{LatitudeDegrees: g.Origin.LatitudeDegrees + dLat, LongitudeDegrees: g.Origin.LongitudeDegrees + dLon, AltitudeMetres: g.Origin.AltitudeMetres, AltitudeDatum: g.Origin.AltitudeDatum})
			if err != nil {
				t.Fatal(err)
			}
			return geom.Vec2{X: local[0], Z: local[2]}.Unit()
		}
		north, east := axis(1e-5, 0), axis(0, 1e-5)
		if north.Dist(geom.Vec2{Z: -1}) > 1e-6 || east.Dist(geom.Vec2{X: 1}) > 1e-6 {
			t.Fatalf("geography tangent is not x east, z south: north %+v east %+v", north, east)
		}
		toTangent := g.SiteToTangent()
		if at := toTangent.Apply(g.Point().XZ()); at.Norm() > 1e-9 {
			t.Fatalf("the anchored site point must map to the tangent origin: %+v", at)
		}
		bearing := func(siteDir geom.Vec2) float64 { // compass bearing, clockwise from north
			v := toTangent.Rotate(siteDir)
			return math.Atan2(v.Dot(east), v.Dot(north))
		}
		if d := geom.AngleDiff(bearing(geom.Vec2{X: 1}), math.Pi/2+yaw); math.Abs(d) > bearingTol {
			t.Fatalf("yaw %g: site +X bearing off by %g", yaw, d)
		}
		if d := geom.AngleDiff(bearing(geom.Vec2{Z: 1}), math.Pi+yaw); math.Abs(d) > bearingTol {
			t.Fatalf("yaw %g: site +Z bearing off by %g", yaw, d)
		}
	}
}

func TestEntitiesProjection(t *testing.T) {
	want := map[string][]string{ // type -> entity keys
		TypeDevicePose: {"device/robot-1"}, TypeAnchor: {"anchor/charging-dock"}, TypeTrack: {"track/t-17"}, TypeRegion: {"region/room-2"},
		TypeChangeEvent: {"change/c-4"}, TypePortal: {"portal/stairs-1"}, TypeLandmark: {"landmark/wall-7"}, TypeObject: {"object/o-9"},
	}
	owner := map[string]string{} // entity key -> the one type that projects it
	for i, p := range samplePayloads() {
		projector, ok := p.(interface{ Entities() []Entity })
		var got []string
		if ok {
			for _, e := range projector.Entities() {
				if err := e.Validate(); err != nil {
					t.Fatalf("%T entity: %v", p, err)
				}
				got = append(got, e.Key())
				if by, dup := owner[e.Key()]; dup && by != p.Type() {
					t.Fatalf("entity %s is projected by both %s and %s", e.Key(), by, p.Type())
				}
				owner[e.Key()] = p.Type()
			}
		}
		expect := want[p.Type()]
		if i == 0 {
			expect = []string{"sensor/cam-unit-1/cam"}
		}
		if !reflect.DeepEqual(got, expect) {
			t.Fatalf("%T entities %v, want %v", p, got, expect)
		}
	}
	s := samplePayloads()
	pose := s[2].(DevicePose).Entities()[0]
	if p, ok := pose.Pose2(); !ok || p != s[2].(DevicePose).Pose2() || pose.Position.Y != 0 || pose.Confidence != 0.93 {
		t.Fatalf("device entity: %+v", pose)
	}
	cam := s[0].(Calibration).Entities()[0]
	// The sample camera looks from (1, 2.6, 2) toward (4, 0, 5): its heading is
	// the planar view direction, a yaw of 45°.
	if !cam.HasYaw || math.Abs(geom.AngleDiff(cam.Yaw, math.Pi/4)) > 1e-9 || cam.Confidence != 1 || cam.Attributes["revision"] != "r3" {
		t.Fatalf("sensor entity: %+v", cam)
	}
	// A registration may carry the sensor's place, but the sensor entity
	// belongs to the Calibration alone.
	reg := s[3].(SensorRegistration)
	reg.Manifest.Attachment = Attachment{Mode: AttachFixed}
	where := At(geom.Vec3{X: 6, Y: 2.4, Z: 1}, 0.04)
	reg.Location = &where
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, projects := Payload(reg).(interface{ Entities() []Entity }); projects {
		t.Fatal("a sensor registration must not project the sensor a calibration already projects")
	}
	bad := where
	bad.UncertaintyM = -1
	reg.Location = &bad
	if reg.Validate() == nil {
		t.Fatal("registration with an invalid location accepted")
	}
	// The sample tag's +Z (out of the wall) points along site −X.
	tag := s[13].(Landmark).Entities()[0]
	if !tag.HasYaw || math.Abs(geom.AngleDiff(tag.Yaw, math.Pi)) > 1e-9 || tag.Validate() != nil {
		t.Fatalf("landmark entity: %+v", tag)
	}
	track := s[5].(Track).Entities()[0]
	if !track.ObservedAt.Equal(t0.Add(4*time.Second)) || track.Extent.RadiusM != 0.25 || track.Attributes["vx"] != "0.8" || track.Class != "person" {
		t.Fatalf("track entity: %+v", track)
	}
	region := s[7].(Region).Entities()[0]
	if len(region.Extent.Polygon) != 4 || region.Label != "kitchen" {
		t.Fatalf("region entity: %+v", region)
	}
	if a := s[4].(Anchor); a.Pose2() != (geom.Pose2{X: 1, Z: 1, Yaw: -0.5}) {
		t.Fatalf("anchor pose: %+v", a.Pose2())
	}
	if c := s[5].(Track).PositionCov(); c != (geom.Cov2{0.01, 0, 0, 0.01}) {
		t.Fatalf("track position covariance: %v", c)
	}
}

func TestEntityOrderingIsDeterministic(t *testing.T) {
	var all []Entity
	for _, p := range samplePayloads() {
		if projector, ok := p.(interface{ Entities() []Entity }); ok {
			all = append(all, projector.Entities()...)
		}
	}
	later := all[0]
	later.ObservedAt = t0.Add(time.Hour)
	all = append(all, later)
	want := append([]Entity(nil), all...)
	SortEntities(want)
	for i := 1; i < len(want); i++ {
		if CompareEntities(want[i-1], want[i]) > 0 {
			t.Fatal("sorted entities are out of order")
		}
	}
	r := rand.New(rand.NewPCG(7, 7))
	for trial := 0; trial < 50; trial++ {
		shuffled := append([]Entity(nil), all...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		SortEntities(shuffled)
		for i := range want {
			// The two portal projections are identical, so compare by order key.
			if CompareEntities(shuffled[i], want[i]) != 0 {
				t.Fatalf("trial %d: order differs at %d: %s vs %s", trial, i, shuffled[i].Key(), want[i].Key())
			}
		}
	}
	if CompareEntities(Entity{Kind: EntityAnchor, ID: "b"}, Entity{Kind: EntityDevice, ID: "a"}) >= 0 {
		t.Fatal("kind orders before id")
	}
}

func TestEntityValidation(t *testing.T) {
	good := Entity{Kind: EntityObject, ID: "o-1", Floor: "ground", Position: geom.Vec3{X: 1, Z: 2}, Confidence: 0.5, UncertaintyM: 0.1, ObservedAt: t0, ValidUntil: t0.Add(time.Second), Attributes: map[string]string{"colour": "red"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := good.Pose2(); ok {
		t.Fatal("an entity without a yaw has no pose")
	}
	if len(EntityKinds()) != 9 || EntityKind("room").Valid() {
		t.Fatal("entity kind registry")
	}
	for name, e := range map[string]Entity{
		"kind":              mutated(good, func(e *Entity) { e.Kind = "room" }),
		"empty id":          mutated(good, func(e *Entity) { e.ID = "" }),
		"NaN position":      mutated(good, func(e *Entity) { e.Position.Y = math.NaN() }),
		"yaw without flag":  mutated(good, func(e *Entity) { e.Yaw = 1 }),
		"confidence":        mutated(good, func(e *Entity) { e.Confidence = 1.1 }),
		"uncertainty":       mutated(good, func(e *Entity) { e.UncertaintyM = -1 }),
		"validity reversed": mutated(good, func(e *Entity) { e.ValidUntil = t0.Add(-time.Second) }),
		"attribute key":     mutated(good, func(e *Entity) { e.Attributes = map[string]string{"a b": "c"} }),
		"extent":            mutated(good, func(e *Entity) { e.Extent.RadiusM = -1 }),
		"floor":             mutated(good, func(e *Entity) { e.Floor = "a/b" }),
	} {
		if e.Validate() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
