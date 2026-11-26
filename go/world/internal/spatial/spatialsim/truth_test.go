package spatialsim

import (
	"math"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

func TestSiteLayoutTruth(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	labels := map[spatial.FloorID][]string{}
	shafts := map[spatial.FloorID][]string{}
	ids := map[string]bool{}
	for _, r := range tr.Rooms() {
		if ids[r.ID] || geom.PolygonArea(r.Polygon) < 3 {
			t.Errorf("room %q is duplicated or degenerate", r.ID)
		}
		ids[r.ID] = true
		if r.Portal != "" {
			shafts[r.Floor] = append(shafts[r.Floor], r.Portal)
		} else {
			labels[r.Floor] = append(labels[r.Floor], r.Label)
		}
		c, err := geom.PolygonCentroid(r.Polygon)
		if got, ok := tr.RoomAt(r.Floor, c); err != nil || !ok || got.ID != r.ID {
			t.Errorf("RoomAt the centroid of %q returns %q", r.ID, got.ID)
		}
	}
	for floor, want := range map[spatial.FloorID][]string{
		FloorGround: {"corridor", "kitchen", "lab", "lobby", "office", "office", "storage"},
		FloorUpper:  {"corridor", "lab", "lounge", "meeting", "office", "storage"},
		FloorYard:   {"yard"},
	} {
		sort.Strings(labels[floor])
		if !slices.Equal(labels[floor], want) {
			t.Errorf("%s rooms are %v; want %v", floor, labels[floor], want)
		}
	}
	for _, floor := range []spatial.FloorID{FloorGround, FloorUpper} {
		sort.Strings(shafts[floor])
		if !slices.Equal(shafts[floor], []string{"lift", "stairs"}) {
			t.Errorf("%s has portal footprints %v; want a lift and stairs", floor, shafts[floor])
		}
	}

	floors := tr.Floors(s.At(0))
	if len(floors) != 3 {
		t.Fatalf("%d floors", len(floors))
	}
	elevation := map[spatial.FloorID]float64{}
	for _, f := range floors {
		if err := f.Validate(); err != nil {
			t.Errorf("floor %s: %v", f.Floor, err)
		}
		elevation[f.Floor] = f.ElevationM
		if f.Outdoor != (f.Floor == FloorYard) || len(f.Portals) == 0 {
			t.Errorf("floor %s: outdoor=%v with %d portals", f.Floor, f.Outdoor, len(f.Portals))
		}
	}
	if elevation[FloorUpper]-elevation[FloorGround] != StoreyHeight || elevation[FloorYard] != elevation[FloorGround] {
		t.Errorf("floor elevations %v", elevation)
	}

	kinds := map[spatial.PortalKind]int{}
	for _, p := range tr.Portals(s.At(0)) {
		if err := p.Validate(); err != nil {
			t.Errorf("portal %s: %v", p.ID, err)
		}
		kinds[p.Kind]++
		rise := p.B.PositionM[1] - p.A.PositionM[1]
		switch p.Kind {
		case spatial.PortalStairs, spatial.PortalLift:
			if p.A.Floor != FloorGround || p.B.Floor != FloorUpper || rise != StoreyHeight || !ids[p.A.Region] || !ids[p.B.Region] {
				t.Errorf("vertical portal %s joins %+v and %+v", p.ID, p.A, p.B)
			}
		default:
			if rise != 0 || p.WidthM < 0.9 || (p.A.Region == "" && p.B.Region == "") {
				t.Errorf("door %s: %+v", p.ID, p)
			}
		}
		if p.ID == "g-exterior-door" && (p.A.Floor == p.B.Floor || (p.A.Floor != FloorYard && p.B.Floor != FloorYard)) {
			t.Errorf("the exterior door must join the ground floor and the yard: %+v", p)
		}
	}
	if kinds[spatial.PortalStairs] != 1 || kinds[spatial.PortalLift] != 1 || kinds[spatial.PortalDoor] < 10 || kinds[spatial.PortalOpening] < 3 {
		t.Errorf("portal kinds %v", kinds)
	}

	ref := tr.GeoReference()
	if err := ref.Validate(); err != nil || ref.SiteYaw == 0 || ref.Origin.LatitudeDegrees == 0 {
		t.Errorf("geo reference %+v: %v", ref, err)
	}
	names := map[string]bool{}
	for _, p := range tr.Points() {
		if names[p.Name] {
			t.Errorf("named point %q is duplicated", p.Name)
		}
		names[p.Name] = true
		if p.Position.Y != elevation[p.Floor] {
			t.Errorf("named point %q is not on its floor", p.Name)
		}
	}
	if !names["g-kitchen"] || !names["g-kitchen-door"] || len(names) < 30 {
		t.Errorf("only %d named points", len(names))
	}
	if len(tr.Beacons()) < 8 || len(tr.SoundSources()) != 2 || len(tr.GasSources()) != 1 {
		t.Error("beacons, sound sources or gas source missing")
	}
	classes := map[string]bool{}
	for _, o := range tr.Objects(s.At(0)) {
		classes[o.Class] = true
	}
	for _, want := range []string{"desk", "shelf", "stove", "fridge", "sofa", "crate"} {
		if !classes[want] {
			t.Errorf("no furniture of class %q", want)
		}
	}
}

func TestChangeScheduleTruth(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	changes := tr.Changes()
	kinds := map[spatial.ChangeKind]int{}
	present := func(id string, at time.Time) (Object, bool) {
		for _, o := range tr.Objects(at) {
			if o.ID == id {
				return o, true
			}
		}
		return Object{}, false
	}
	last := time.Time{}
	for _, c := range changes {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", c.ID, err)
		}
		at := c.FirstObserved
		if at.Before(last) || at.Before(s.At(OfficeChangesFrom)) || at.After(s.At(OfficeChangesUntil)) || !at.Equal(c.LastObserved) {
			t.Errorf("%s at %v is out of order or outside the change window", c.ID, at)
		}
		last = at
		kinds[c.Kind]++
		before, after := at.Add(-time.Millisecond), at
		switch c.Kind {
		case spatial.ChangeAppeared, spatial.ChangeDisappeared:
			_, was := present(c.Subject, before)
			now, is := present(c.Subject, after)
			if was == is || is != (c.Kind == spatial.ChangeAppeared) {
				t.Errorf("%s: %s present before=%v after=%v", c.ID, c.Subject, was, is)
			}
			if is && now.Shape.Centre.Dist(c.Point().XZ()) > exact {
				t.Errorf("%s is not located at the object", c.ID)
			}
		case spatial.ChangeMoved:
			was, _ := present(c.Subject, before)
			now, _ := present(c.Subject, after)
			if was.Shape.Centre.Dist(geom.Vec3FromArray(*c.FromM).XZ()) > exact || now.Shape.Centre.Dist(geom.Vec3FromArray(*c.ToM).XZ()) > exact || was.Shape.Centre.Dist(now.Shape.Centre) < 1 {
				t.Errorf("%s: moved from %v to %v, event says %v to %v", c.ID, was.Shape.Centre, now.Shape.Centre, *c.FromM, *c.ToM)
			}
		default:
			was, ok := tr.DoorOpen(c.Subject, before)
			is, _ := tr.DoorOpen(c.Subject, after)
			if !ok || was == is || is != (c.Kind == spatial.ChangeDoorOpened) {
				t.Errorf("%s: door %s open before=%v after=%v", c.ID, c.Subject, was, is)
			}
		}
	}
	for _, kind := range []spatial.ChangeKind{spatial.ChangeAppeared, spatial.ChangeDisappeared, spatial.ChangeMoved, spatial.ChangeDoorOpened, spatial.ChangeDoorClosed} {
		if kinds[kind] == 0 {
			t.Errorf("no %q change is scheduled; have %v", kind, kinds)
		}
	}
	for _, p := range tr.Portals(s.At(OfficeChangesUntil)) {
		if want := spatial.PortalOpen; p.ID == "g-entrance" && p.State != want {
			t.Errorf("entrance is %s after it opened", p.State)
		}
		if want := spatial.PortalClosed; p.ID == "g-lab-east-door" && p.State != want {
			t.Errorf("lab east door is %s after it closed", p.State)
		}
	}
}

func TestOccupancyTruth(t *testing.T) {
	const cell = 0.1
	quiet := DefaultConfig().Noiseless()
	s := mustSite(t, Office, 1, Options{Config: &quiet, NoActors: true})
	tr := s.Truth()
	for _, floor := range []spatial.FloorID{FloorGround, FloorUpper, FloorYard} {
		r, err := tr.Occupancy(floor, s.At(0), cell)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[Cell]int{}
		for _, c := range r.Cells {
			counts[c]++
		}
		if counts[CellFree] < 5*counts[CellOccupied] || counts[CellOccupied] < 500 || counts[CellUnknown] == 0 {
			t.Errorf("%s raster has %v cells", floor, counts)
		}
		for _, w := range tr.Walls(floor) {
			for _, p := range []geom.Vec2{w.A, w.Midpoint(), w.B} {
				if got := r.At(p); got == CellFree {
					t.Fatalf("%s: wall point %v is free", floor, p)
				}
			}
		}
		for _, room := range tr.Rooms() {
			c, _ := geom.PolygonCentroid(room.Polygon)
			if room.Floor == floor && r.At(c) != CellFree {
				t.Errorf("%s: the centre of %s is %d, want free", floor, room.ID, r.At(c))
			}
		}
		if r.At(geom.Vec2{X: -50, Z: -50}) != CellUnknown || r.At(r.Origin.Add(geom.Vec2{X: cell / 2, Z: cell / 2})) != CellUnknown {
			t.Errorf("%s: cells outside the building must be unknown", floor)
		}
	}
	if _, err := tr.Occupancy("nowhere", s.At(0), cell); err == nil {
		t.Error("unknown floor accepted")
	}
	if _, err := tr.Occupancy(FloorGround, s.At(0), 0); err == nil {
		t.Error("zero cell size accepted")
	}

	// The raster follows the schedule.
	before, _ := tr.Occupancy(FloorGround, s.At(OfficeChangesFrom), cell)
	after, _ := tr.Occupancy(FloorGround, s.At(OfficeChangesUntil), cell)
	for _, c := range []struct {
		name          string
		p             geom.Vec2
		before, after Cell
	}{
		{"lab crate appears", geom.Vec2{X: 10.5, Z: 15.6}, CellFree, CellOccupied},
		{"storage crate disappears", geom.Vec2{X: 19, Z: 19}, CellOccupied, CellFree},
		{"table leaves", geom.Vec2{X: 14.3, Z: 6.8}, CellOccupied, CellFree},
		{"table arrives", geom.Vec2{X: 9.5, Z: 7}, CellFree, CellOccupied},
		{"lab east door closes", geom.Vec2{X: 13, Z: 11.5}, CellFree, CellOccupied},
		{"entrance opens", geom.Vec2{X: 0, Z: 4.2}, CellOccupied, CellFree},
	} {
		if before.At(c.p) != c.before || after.At(c.p) != c.after {
			t.Errorf("%s: cell is %d then %d; want %d then %d", c.name, before.At(c.p), after.At(c.p), c.before, c.after)
		}
	}

	// What each cell is made of.
	for _, c := range []struct {
		name  string
		p     geom.Vec2
		class CellClass
	}{
		{"a lobby wall", geom.Vec2{X: 8, Z: 4}, ClassWall},
		{"the kitchen stove", geom.Vec2{X: 9, Z: 0.5}, ClassFurniture},
		{"the middle of the lab", geom.Vec2{X: 8, Z: 16}, ClassFree},
		{"the closed lab east door", geom.Vec2{X: 13, Z: 11.5}, ClassDoor},
		{"outside the building", geom.Vec2{X: -3, Z: -3}, ClassNone},
	} {
		if got := after.ClassAt(c.p); got != c.class {
			t.Errorf("%s is %q; want %q", c.name, got, c.class)
		}
	}
	if before.ClassAt(geom.Vec2{X: 13, Z: 11.5}) != ClassFree {
		t.Error("an open doorway must be free")
	}
	kinds := map[CellClass]int{}
	for i, class := range after.Classes {
		kinds[class]++
		if want := map[CellClass]Cell{ClassNone: CellUnknown, ClassFree: CellFree}[class]; class <= ClassFree && after.Cells[i] != want || class > ClassFree && after.Cells[i] != CellOccupied {
			t.Fatalf("cell %d has class %q and state %d", i, class, after.Cells[i])
		}
	}
	if kinds[ClassWall] < 1000 || kinds[ClassFurniture] < 300 || kinds[ClassDoor] < 10 || ClassFurniture.String() != "furniture" {
		t.Errorf("ground raster classes: %v", kinds)
	}

	// Lidar and raster agree: without noise or actors, a beam ends on an
	// occupied cell unless it hit another robot. On the way there it crosses
	// free cells, except where it grazes along a wall inside the wall's cells.
	const minOnOccupied, maxThroughOccupied = 0.97, 0.01
	yard, _ := tr.Occupancy(FloorYard, s.At(OfficeChangesFrom), cell)
	level := func(p geom.Vec2) Cell { // the ground floor and the yard share a level
		return max(before.At(p), yard.At(p))
	}
	ends, onOccupied, steps, through := 0, 0, 0, 0
	for at := time.Duration(0); at < 80*time.Second; at += 2 * time.Second {
		scan, _ := s.Scan(Rover1, s.At(at))
		pose := sensorPose(t, s, Rover1, s.At(at))
		for i := range scan.Ranges {
			angle, r, state := scan.Beam(i)
			if state != spatial.BeamHit {
				continue
			}
			ends++
			// The hit lies on a surface, so look half a cell either side of it.
			near := level(pose.Apply(geom.Heading(angle).Scale(r+cell/2))) == CellOccupied || level(pose.Apply(geom.Heading(angle).Scale(r-cell/2))) == CellOccupied
			if near {
				onOccupied++
			}
			for d := cell; d < r-2*cell; d += cell {
				switch steps++; level(pose.Apply(geom.Heading(angle).Scale(d))) {
				case CellOccupied:
					through++
				case CellUnknown:
					t.Fatalf("beam %d at %v leaves the mapped rooms on its way to %.2f m", i, at, r)
				}
			}
		}
	}
	if frac := float64(onOccupied) / float64(ends); ends < 5000 || frac < minOnOccupied {
		t.Errorf("%.3f of %d beam ends lie on occupied cells; want %.2f", frac, ends, minOnOccupied)
	}
	if frac := float64(through) / float64(steps); frac > maxThroughOccupied {
		t.Errorf("%.4f of %d beam steps cross occupied cells; want under %.2f", frac, steps, maxThroughOccupied)
	}
}

func TestEntitiesTruth(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	count := func(at time.Duration) map[spatial.EntityKind]int {
		es := tr.Entities(s.At(at))
		kinds := map[spatial.EntityKind]int{}
		keys := map[string]bool{}
		for i, e := range es {
			if err := e.Validate(); err != nil {
				t.Fatalf("entity %s: %v", e.Key(), err)
			}
			if keys[e.Key()] || !e.ObservedAt.Equal(s.At(at)) || (i > 0 && spatial.CompareEntities(es[i-1], e) > 0) {
				t.Fatalf("entity %s is duplicated, unstamped or out of order", e.Key())
			}
			keys[e.Key()] = true
			kinds[e.Kind]++
		}
		return kinds
	}
	classes := map[string]int{}
	for _, m := range tr.Movers(s.At(OfficeChangesUntil)) {
		classes[m.Class]++
	}
	// Three walkers plus the handheld's carrier, one cart, six robots.
	if classes["person"] != 4 || classes["cart"] != 1 || classes["robot"] != 6 {
		t.Errorf("movers by class: %v", classes)
	}
	start, end := count(0), count(OfficeChangesUntil)
	for _, kind := range []spatial.EntityKind{spatial.EntityDevice, spatial.EntityTrack, spatial.EntityObject, spatial.EntityLandmark, spatial.EntityRegion, spatial.EntityPortal} {
		if start[kind] == 0 {
			t.Errorf("no %s entities", kind)
		}
	}
	// Two devices start late; three crates appear and one disappears.
	if end[spatial.EntityDevice] != start[spatial.EntityDevice]+2 || end[spatial.EntityObject] != start[spatial.EntityObject]+2 {
		t.Errorf("entities do not evolve with the schedule: %v then %v", start, end)
	}
}

func TestDeclaredKnowledge(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	manifests := s.Manifests()
	kinds := map[spatial.SensorKind]int{}
	seen := map[string]bool{}
	for _, m := range manifests {
		if err := m.Validate(); err != nil {
			t.Fatalf("manifest %s/%s: %v", m.Device, m.Sensor, err)
		}
		kinds[m.Kind()]++
		seen[string(m.Device)+"/"+string(m.Sensor)] = true
		off, ok := tr.ClockOffset(m.Device, m.Sensor)
		abs := off.Abs()
		if m.Kind() == spatial.KindOdometry {
			abs += 30 * time.Millisecond // odometry is the reference clock
			if off != 0 {
				t.Errorf("%s odometry clock offset %v; want 0", m.Device, off)
			}
		}
		if !ok || abs < 10*time.Millisecond || abs > 60*time.Millisecond {
			t.Errorf("%s/%s clock offset %v; want tens of milliseconds", m.Device, m.Sensor, off)
		}
		if m.Kind() != spatial.KindCamera {
			continue
		}
		truth, _, ok := tr.Camera(m.Device, s.At(OfficeDuration))
		_, parent, _ := tr.CameraMount(m.Device, s.At(0))
		switch {
		case !ok || m.Intrinsics == nil || truth.Validate() != nil:
			t.Errorf("%s has no usable intrinsics", m.Device)
		case parent == spatial.ParentDeviceBase && (*m.Intrinsics != truth || m.Attachment.Mode != spatial.AttachMounted):
			t.Errorf("%s: a mobile camera declares its true intrinsics and its carrier", m.Device)
		case parent == spatial.ParentSite && (m.Intrinsics.K1 != 0 || truth.K1 == 0 || truth.K2 == 0 || math.Abs(truth.FX/m.Intrinsics.FX-1) > 0.03 || truth.FX == m.Intrinsics.FX || m.Attachment.Mode != spatial.AttachFixed):
			t.Errorf("%s: a fixed camera declares a nominal lens near the distorted truth: %+v vs %+v", m.Device, *m.Intrinsics, truth)
		}
	}
	if len(seen) != len(manifests) {
		t.Error("duplicate manifests")
	}
	for _, kind := range spatial.SensorKinds() {
		if kinds[kind] == 0 {
			t.Errorf("no sensor of kind %s", kind)
		}
	}
	if kinds[spatial.KindCamera] != 16 || kinds[spatial.KindLidar2D] != 6 || kinds[spatial.KindMic] != 5 {
		t.Errorf("sensor counts %v", kinds)
	}

	landmarks := map[string]bool{}
	for _, l := range tr.Landmarks() {
		landmarks[l.ID] = true
		if room, ok := tr.RoomAt(l.Floor, l.Pose.T.XZ()); !ok || !strings.HasPrefix(l.ID, "tag-"+room.ID+"-") || !l.Pose.R.IsRotation(1e-12) {
			t.Errorf("landmark %s is not inside the room it is named after", l.ID)
		}
	}
	markers, wall := 0, 0
	for _, g := range s.Fiducials() {
		if err := g.Validate(); err != nil {
			t.Fatalf("fiducial %s: %v", g.ID, err)
		}
		if g.Carrier == "" {
			if wall++; !landmarks[g.ID] || len(g.Points) != 4 {
				t.Errorf("wall fiducial %s has no landmark or not four corners", g.ID)
			}
			continue
		}
		markers++
		heights := map[float64]bool{}
		for _, p := range g.Points {
			heights[p.Y] = true
		}
		if g.ID != MarkerID(g.Carrier) || len(heights) < 2 || len(g.Points) < 6 {
			t.Errorf("marker %s needs points at two heights: %v", g.ID, g.Points)
		}
	}
	if markers != 6 || wall != len(landmarks) || wall < 100 {
		t.Errorf("%d robot markers and %d wall fiducials for %d landmarks", markers, wall, len(landmarks))
	}
	if err := s.Target().Validate(); err != nil {
		t.Error(err)
	}

	// Mounts are not trivial and odometry frames hide the start poses.
	starts := map[geom.Pose2]bool{}
	for _, dev := range []spatial.DeviceID{Rover1, Rover2, Rover3, Climber, Ranger} {
		lidar, _ := tr.LidarMount(dev, s.At(0))
		cam, parent, _ := tr.CameraMount(dev, s.At(0))
		if lidar.Pos().Norm() < 0.08 || math.Abs(lidar.Yaw) < 0.05 || parent != spatial.ParentDeviceBase || cam.T.Norm() < 0.3 || !cam.R.IsRotation(1e-12) {
			t.Errorf("%s mounts are trivial: lidar %+v camera %+v", dev, lidar, cam)
		}
		odom, _ := s.Odometry(dev, s.At(0))
		st, _ := tr.Device(dev, s.At(0))
		if odom.Pose != (geom.Pose2{}) || starts[st.Pose] {
			t.Errorf("%s: odometry starts at %+v, truth at %+v", dev, odom.Pose, st.Pose)
		}
		starts[st.Pose] = true
	}
}

// TestGroundRoversOverlapAndCloseLoops checks what collaborative mapping
// needs from the three ground robots.
func TestGroundRoversOverlapAndCloseLoops(t *testing.T) {
	const (
		near       = 1.5  // metres between two paths that count as overlapping
		minOverlap = 0.15 // of each robot's path
	)
	s := officeSite(t)
	tr := s.Truth()
	paths := map[spatial.DeviceID][]geom.Vec2{}
	rovers := []spatial.DeviceID{Rover1, Rover2, Rover3}
	for _, dev := range rovers {
		period := sec(s.byID[dev].route.period)
		a, _ := tr.Device(dev, s.At(7*time.Second))
		b, _ := tr.Device(dev, s.At(7*time.Second+period))
		if a.Pose.Pos().Dist(b.Pose.Pos()) > 1e-6 || math.Abs(geom.AngleDiff(a.Pose.Yaw, b.Pose.Yaw)) > 1e-6 || period > OfficeChangesFrom/2 {
			t.Errorf("%s does not close its loop at least twice before the change window (period %v)", dev, period)
		}
		for at := time.Duration(0); at < period; at += 500 * time.Millisecond {
			st, _ := tr.Device(dev, s.At(at))
			paths[dev] = append(paths[dev], st.Pose.Pos())
		}
	}
	for _, a := range rovers {
		for _, b := range rovers {
			if a == b {
				continue
			}
			shared := 0
			for _, p := range paths[a] {
				if slices.ContainsFunc(paths[b], func(q geom.Vec2) bool { return p.Dist(q) < near }) {
					shared++
				}
			}
			if frac := float64(shared) / float64(len(paths[a])); frac < minOverlap {
				t.Errorf("only %.2f of %s's path runs near %s's", frac, a, b)
			}
		}
	}
}
