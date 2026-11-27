package spatialsim

import (
	"errors"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Truth is the ground truth of a Site. It exists to SCORE estimates. A test
// that feeds anything read from Truth into the estimator it is testing
// proves nothing; see the anti-goals of the spatial design.
type Truth struct{ s *Site }

// Room is one labelled region of a floor. Portal is non-empty for the
// footprint of a staircase or lift shaft, which is free space but not a room
// in the semantic sense, and names the vertical portal it belongs to.
type Room struct {
	ID      string
	Label   string
	Floor   spatial.FloorID
	Polygon []geom.Vec2
	Portal  string
}

// Door is a gap in a wall. A PortalDoor has a leaf that blocks lidar and
// sight while closed; a PortalOpening never closes.
type Door struct {
	ID            string
	Floor         spatial.FloorID
	Kind          spatial.PortalKind
	Segment       geom.Segment
	Rooms         [2]string       // room ids on either side; "" where there is none
	InitiallyOpen bool            // state at the epoch
	Toggles       []time.Duration // offsets from the epoch at which the state flips
}

// OpenAt is the door state at an offset from the epoch.
func (d Door) OpenAt(sinceEpoch time.Duration) bool {
	open := d.InitiallyOpen
	for _, t := range d.Toggles {
		if sinceEpoch >= t {
			open = !open
		}
	}
	return open
}

// DeviceState is where a device is. For a mobile device Pose is its base and
// Elevation the surface it stands on. For a fixed device Pose is its
// position with the yaw it faces (a camera's viewing direction) and
// Elevation its own World Y.
type DeviceState struct {
	Pose      geom.Pose2
	Elevation float64
	Floor     spatial.FloorID
	Velocity  geom.Vec2 // m/s in the site frame
	YawRate   float64   // rad/s
}

// Object is a piece of furniture present at an instant.
type Object struct {
	ID, Class string
	Floor     spatial.FloorID
	Shape     Shape
}

// ActorState is a moving person or cart at an instant.
type ActorState struct {
	ID, Class string
	Floor     spatial.FloorID
	Shape     Shape
	Velocity  geom.Vec2
}

// Landmark is a wall tag: Pose maps the tag frame (see Site.Fiducials) to
// the site, so Pose.T is the tag centre.
type Landmark struct {
	ID    string
	Floor spatial.FloorID
	Pose  geom.Pose3
}

// Beacon is a fixed UWB anchor. Its id is the Peer of the ranges to it.
type Beacon struct {
	ID       spatial.DeviceID
	Floor    spatial.FloorID
	Position geom.Vec3
}

// Interval is a half-open span [From, Until) of offsets from the epoch.
type Interval struct{ From, Until time.Duration }

// SoundSource emits while one of its intervals is active.
type SoundSource struct {
	ID       string
	Floor    spatial.FloorID
	Position geom.Vec3
	Active   []Interval
}

func (src SoundSource) ActiveAt(sinceEpoch time.Duration) bool {
	for _, iv := range src.Active {
		if sinceEpoch >= iv.From && sinceEpoch < iv.Until {
			return true
		}
	}
	return false
}

// GasSource adds PeakPPM·exp(−r²/2·SpreadM²) to the background on its level.
type GasSource struct {
	ID       string
	Floor    spatial.FloorID
	Position geom.Vec3
	PeakPPM  float64
	SpreadM  float64
}

// Ambiguity names rooms a lidar cannot tell apart from the inside. With two
// rooms, a pose in the first and the same pose translated by Shift in the
// second see the same geometry. With Continuous set there is one room and
// Shift is the unit direction along which any translation is unobservable
// while the room's ends are out of lidar range.
type Ambiguity struct {
	Floor      spatial.FloorID
	Rooms      []string
	Shift      geom.Vec2
	Continuous bool
}

// DisturbanceKind classifies a scheduled device disturbance.
type DisturbanceKind string

const (
	DisturbCameraBump DisturbanceKind = "camera_bump" // a fixed camera's pose changes
	DisturbMountShift DisturbanceKind = "mount_shift" // a robot's lidar mount changes
	DisturbLateStart  DisturbanceKind = "late_start"  // a device first appears, announcing only manifests
)

// Disturbance is one scheduled device event.
type Disturbance struct {
	Kind   DisturbanceKind
	Device spatial.DeviceID
	Sensor spatial.SensorID // empty for a late start
	At     time.Time
}

// NamedPoint is a fixed site point with a name, for scoring anchors.
type NamedPoint struct {
	Name     string
	Floor    spatial.FloorID
	Position geom.Vec3
}

// Cell is the state of one occupancy raster cell.
type Cell uint8

const (
	CellUnknown  Cell = iota // outside every room: no sensor can observe it
	CellFree                 // inside a room and clear
	CellOccupied             // touched by a wall, a closed door or furniture
)

// CellClass is what a raster cell is made of. Its String is the class name a
// semantic cell layer uses.
type CellClass uint8

const (
	ClassNone      CellClass = iota // CellUnknown
	ClassFree                       // CellFree
	ClassWall                       // a wall
	ClassDoor                       // the leaf of a closed door
	ClassFurniture                  // a piece of furniture, where no wall or door is
)

func (c CellClass) String() string {
	return [...]string{"", "free", "wall", "door", "furniture"}[c]
}

// Raster is the true static occupancy of one floor at one instant. Moving
// actors and devices are not in it. Cell (ix, iz) covers the square whose
// minimum corner is Origin + (ix, iz)·CellSize and is stored at iz·Width+ix.
// Classes is parallel to Cells and says what each cell is made of.
type Raster struct {
	Floor         spatial.FloorID
	Origin        geom.Vec2
	CellSize      float64
	Width, Height int
	Cells         []Cell
	Classes       []CellClass
}

// ClassAt is the class of the cell containing p; ClassNone outside the raster.
func (r Raster) ClassAt(p geom.Vec2) CellClass {
	ix, iz := int(math.Floor((p.X-r.Origin.X)/r.CellSize)), int(math.Floor((p.Z-r.Origin.Z)/r.CellSize))
	if ix < 0 || iz < 0 || ix >= r.Width || iz >= r.Height {
		return ClassNone
	}
	return r.Classes[iz*r.Width+ix]
}

// At is the cell containing p; CellUnknown outside the raster.
func (r Raster) At(p geom.Vec2) Cell {
	ix, iz := int(math.Floor((p.X-r.Origin.X)/r.CellSize)), int(math.Floor((p.Z-r.Origin.Z)/r.CellSize))
	if ix < 0 || iz < 0 || ix >= r.Width || iz >= r.Height {
		return CellUnknown
	}
	return r.Cells[iz*r.Width+ix]
}

// Centre is the site position of the middle of cell (ix, iz).
func (r Raster) Centre(ix, iz int) geom.Vec2 {
	return geom.Vec2{X: r.Origin.X + (float64(ix)+0.5)*r.CellSize, Z: r.Origin.Z + (float64(iz)+0.5)*r.CellSize}
}

// Device is where a device is at the instant; false before it starts.
func (tr Truth) Device(dev spatial.DeviceID, at time.Time) (DeviceState, bool) {
	s := tr.s
	d, t := s.byID[dev], at.Sub(epoch)
	if d == nil || t < d.start {
		return DeviceState{}, false
	}
	if d.route != nil {
		p, e, v, w := d.route.at((t - d.start).Seconds())
		return DeviceState{Pose: p, Elevation: e, Floor: s.levelAt(e).floorAt(p.Pos()).id, Velocity: v, YawRate: w}, true
	}
	st := DeviceState{Pose: d.pose, Elevation: d.elevation}
	if d.has(spatial.KindCamera) {
		cam := d.cameraAt(t)
		view := cam.Rotate(geom.Vec3{Z: 1})
		st = DeviceState{Pose: geom.Pose2{X: cam.T.X, Z: cam.T.Z, Yaw: math.Atan2(view.Z, view.X)}, Elevation: cam.T.Y}
	}
	st.Floor = d.level.floorAt(st.Pose.Pos()).id
	return st, true
}

// LidarMount is the lidar frame in the device base frame at the instant.
func (tr Truth) LidarMount(dev spatial.DeviceID, at time.Time) (geom.Pose2, bool) {
	d := tr.s.byID[dev]
	if d == nil || !d.has(spatial.KindLidar2D) {
		return geom.Pose2{}, false
	}
	return d.lidarAt(at.Sub(epoch)), true
}

// CameraMount is the optical frame in its parent at the instant: the device
// base frame (x forward, y up, z right) for a mobile camera, the site frame
// for a fixed one.
func (tr Truth) CameraMount(dev spatial.DeviceID, at time.Time) (geom.Pose3, spatial.ParentKind, bool) {
	d := tr.s.byID[dev]
	if d == nil || !d.has(spatial.KindCamera) {
		return geom.Pose3{}, "", false
	}
	parent := spatial.ParentSite
	if d.route != nil {
		parent = spatial.ParentDeviceBase
	}
	return d.cameraAt(at.Sub(epoch)), parent, true
}

// Camera is the true intrinsics and optical-frame-to-site pose at the
// instant; false before the device starts.
func (tr Truth) Camera(dev spatial.DeviceID, at time.Time) (geom.Intrinsics, geom.Pose3, bool) {
	d, t := tr.s.byID[dev], at.Sub(epoch)
	if d == nil || !d.has(spatial.KindCamera) || t < d.start {
		return geom.Intrinsics{}, geom.Pose3{}, false
	}
	pose, _ := tr.s.cameraPose(d, t)
	return d.intrinsics, pose, true
}

// ClockOffset is a sensor's clock minus site time: the amount added to the
// true sampling instant to get the timestamp its measurements carry.
func (tr Truth) ClockOffset(dev spatial.DeviceID, sensor spatial.SensorID) (time.Duration, bool) {
	if d := tr.s.byID[dev]; d != nil {
		for _, sn := range d.sensors {
			if sn.id == sensor {
				return sn.clock, true
			}
		}
	}
	return 0, false
}

// Actors is every scripted person and cart at the instant. Devices are not
// actors; see Movers.
func (tr Truth) Actors(at time.Time) []ActorState {
	var out []ActorState
	for _, a := range tr.s.actors {
		sh, v := a.at(at.Sub(epoch))
		out = append(out, ActorState{ID: a.id, Class: a.class, Floor: a.level.floorAt(sh.Centre).id, Shape: sh, Velocity: v})
	}
	return out
}

// Movers is everything that moves at the instant: the actors, then every
// mobile device that has started, with the id and body class a camera
// reports for it ("robot", or "person" for whoever carries a handheld).
// This is the set a tracker is scored against.
func (tr Truth) Movers(at time.Time) []ActorState {
	s, t := tr.s, at.Sub(epoch)
	out := tr.Actors(at)
	for _, d := range s.devices {
		if d.route == nil || t < d.start {
			continue
		}
		p, e, v, _ := d.route.at((t - d.start).Seconds())
		out = append(out, ActorState{
			ID: string(d.id), Class: d.class, Floor: s.levelAt(e).floorAt(p.Pos()).id, Velocity: v,
			Shape: Shape{Centre: p.Pos(), Yaw: p.Yaw, Radius: d.radius, Height: d.height},
		})
	}
	return out
}

// Rooms is every room and portal footprint, floor by floor.
func (tr Truth) Rooms() []Room {
	var out []Room
	for _, f := range tr.s.floors {
		out = append(out, f.rooms...)
	}
	return out
}

// RoomAt is the room of a floor that contains p.
func (tr Truth) RoomAt(floor spatial.FloorID, p geom.Vec2) (Room, bool) {
	if f := tr.s.floor(floor); f != nil {
		for _, r := range f.rooms {
			if geom.PointInPolygon(p, r.Polygon) {
				return r, true
			}
		}
	}
	return Room{}, false
}

// Walls is every wall segment on the level of a floor, door gaps removed.
// Floors that share a level share walls.
func (tr Truth) Walls(floor spatial.FloorID) []geom.Segment {
	if f := tr.s.floor(floor); f != nil {
		return slices.Clone(f.level.walls)
	}
	return nil
}

// Doors is every door and opening.
func (tr Truth) Doors() []Door {
	var out []Door
	for _, lv := range tr.s.levels {
		for _, d := range lv.doors {
			out = append(out, *d)
		}
	}
	return out
}

// DoorOpen is the state of a door at the instant; ok is false for an unknown id.
func (tr Truth) DoorOpen(id string, at time.Time) (open, ok bool) {
	for _, lv := range tr.s.levels {
		for _, d := range lv.doors {
			if d.ID == id {
				return d.OpenAt(at.Sub(epoch)), true
			}
		}
	}
	return false, false
}

func (s *Site) doorPortal(lv *level, d *Door, t time.Duration) spatial.Portal {
	mid := d.Segment.Midpoint().Lift(lv.elevation).Array()
	end := func(room string) spatial.PortalEnd {
		e := spatial.PortalEnd{Floor: d.Floor, Region: room, PositionM: mid}
		for _, f := range lv.floors {
			for _, r := range f.rooms {
				if r.ID == room {
					e.Floor = f.id
				}
			}
		}
		return e
	}
	state := spatial.PortalOpen
	if !d.OpenAt(t) {
		state = spatial.PortalClosed
	}
	return spatial.Portal{
		ID: d.ID, Kind: d.Kind, State: state, A: end(d.Rooms[0]), B: end(d.Rooms[1]), WidthM: d.Segment.Length(), Confidence: 1,
		Location: spatial.Enclose(lv.elevation, d.Segment.A, d.Segment.B),
	}
}

// Portals is every door and opening with its state at the instant, then the
// stairs and lifts, by id.
func (tr Truth) Portals(at time.Time) []spatial.Portal {
	var out []spatial.Portal
	for _, lv := range tr.s.levels {
		for _, d := range lv.doors {
			out = append(out, tr.s.doorPortal(lv, d, at.Sub(epoch)))
		}
	}
	out = append(out, tr.s.vertical...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Floors describes every floor with the portals that touch it.
func (tr Truth) Floors(at time.Time) []spatial.FloorInfo {
	portals := tr.Portals(at)
	var out []spatial.FloorInfo
	for _, f := range tr.s.floors {
		info := spatial.FloorInfo{Floor: f.id, Name: f.name, ElevationM: f.level.elevation, HeightM: 3, Outdoor: f.outdoor}
		for _, p := range portals {
			if p.A.Floor == f.id || p.B.Floor == f.id {
				info.Portals = append(info.Portals, p)
			}
		}
		lo, hi := f.bounds()
		loc := spatial.Enclose(f.level.elevation, lo, hi)
		info.Location = &loc
		out = append(out, info)
	}
	return out
}

func (f *floor) bounds() (lo, hi geom.Vec2) {
	lo, hi = geom.Vec2{X: math.Inf(1), Z: math.Inf(1)}, geom.Vec2{X: math.Inf(-1), Z: math.Inf(-1)}
	for _, r := range f.rooms {
		for _, p := range r.Polygon {
			lo = geom.Vec2{X: math.Min(lo.X, p.X), Z: math.Min(lo.Z, p.Z)}
			hi = geom.Vec2{X: math.Max(hi.X, p.X), Z: math.Max(hi.Z, p.Z)}
		}
	}
	return lo, hi
}

// Objects is the furniture present at the instant.
func (tr Truth) Objects(at time.Time) []Object {
	var out []Object
	for _, lv := range tr.s.levels {
		for _, f := range lv.furniture {
			if sh, ok := f.at(at.Sub(epoch)); ok {
				out = append(out, Object{ID: f.id, Class: f.class, Floor: f.floor.id, Shape: sh})
			}
		}
	}
	return out
}

// Landmarks is every wall tag with its true pose.
func (tr Truth) Landmarks() []Landmark {
	var out []Landmark
	for _, lv := range tr.s.levels {
		out = append(out, lv.tags...)
	}
	return out
}

func (tr Truth) Beacons() []Beacon           { return slices.Clone(tr.s.beacons) }
func (tr Truth) SoundSources() []SoundSource { return slices.Clone(tr.s.sounds) }
func (tr Truth) GasSources() []GasSource     { return slices.Clone(tr.s.gas) }

// GasConcentration is the true field on a floor's level at p, in ppm.
func (tr Truth) GasConcentration(floor spatial.FloorID, p geom.Vec2) float64 {
	f := tr.s.floor(floor)
	if f == nil {
		return GasBackgroundPPM
	}
	return tr.s.concentration(f.level, p)
}

// UwbMount is the UWB antenna of a device in its base frame (x forward, y up,
// z right). It equals what Survey declares.
func (tr Truth) UwbMount(dev spatial.DeviceID) (geom.Vec3, bool) {
	d := tr.s.byID[dev]
	if d == nil || !d.has(spatial.KindUwb) {
		return geom.Vec3{}, false
	}
	return d.uwbMount, true
}

// RangeLineOfSight reports whether the UWB path from a device's antenna to a
// peer (a beacon or another device) is clear of walls and closed doors at
// the instant. ok is false when either end is unknown, not started, or on
// another level.
func (tr Truth) RangeLineOfSight(dev, peer spatial.DeviceID, at time.Time) (clear, ok bool) {
	s := tr.s
	t := at.Sub(epoch)
	end := func(id spatial.DeviceID) (geom.Vec2, *level) {
		if d := s.byID[id]; d != nil && t >= d.start {
			p, lv := s.tagAt(d, t)
			return p.XZ(), lv
		}
		for _, b := range s.beacons {
			if b.ID == id {
				return b.Position.XZ(), s.floor(b.Floor).level
			}
		}
		return geom.Vec2{}, nil
	}
	a, la := end(dev)
	b, lb := end(peer)
	if la == nil || la != lb {
		return false, false
	}
	return !s.barriers(la, t).walled(a, b), true
}

// TargetPose is the target-to-site pose of the calibration target while it
// is being shown; false between shows.
func (tr Truth) TargetPose(at time.Time) (geom.Pose3, bool) {
	t := at.Sub(epoch)
	for _, sh := range tr.s.shows {
		if t >= sh.Start && t < sh.Start+sh.Duration {
			pose, _ := tr.s.boardPose(sh, t)
			return pose, true
		}
	}
	return geom.Pose3{}, false
}

// Changes is every change of the static environment, in time order.
// FirstObserved and LastObserved are both the instant it happened.
func (tr Truth) Changes() []spatial.ChangeEvent { return slices.Clone(tr.s.changes) }

// Disturbances is every scheduled device event, in time order.
func (tr Truth) Disturbances() []Disturbance { return slices.Clone(tr.s.disturbances) }

// GeoReference ties the site frame to the Earth: the geographic anchor of
// the site origin and the heading of the site frame.
func (tr Truth) GeoReference() spatial.GeoReference { return tr.s.geo }

// Ambiguity is the deliberately ambiguous region, when the scenario has one.
func (tr Truth) Ambiguity() (Ambiguity, bool) {
	if tr.s.ambiguity == nil {
		return Ambiguity{}, false
	}
	return *tr.s.ambiguity, true
}

// Points is a set of named fixed points: the centroid of every room, named
// by the room id, and the midpoint of every door, named by the door id.
func (tr Truth) Points() []NamedPoint {
	var out []NamedPoint
	for _, f := range tr.s.floors {
		for _, r := range f.rooms {
			if c, err := geom.PolygonCentroid(r.Polygon); err == nil {
				out = append(out, NamedPoint{Name: r.ID, Floor: f.id, Position: c.Lift(f.level.elevation)})
			}
		}
	}
	for _, lv := range tr.s.levels {
		for _, d := range lv.doors {
			out = append(out, NamedPoint{Name: d.ID, Floor: d.Floor, Position: d.Segment.Midpoint().Lift(lv.elevation)})
		}
	}
	return out
}

// Occupancy rasterises the static truth of a floor at the instant: walls,
// closed doors and present furniture are occupied, the rest of every room is
// free, everything else unknown. Raster.Classes tells the three kinds of
// occupied cell apart. The raster covers the floor's bounding box plus one
// cell of margin.
func (tr Truth) Occupancy(floor spatial.FloorID, at time.Time, cellSize float64) (Raster, error) {
	f := tr.s.floor(floor)
	if f == nil || !(cellSize > 0) {
		return Raster{}, errors.New("spatialsim: unknown floor or non-positive cell size")
	}
	t := at.Sub(epoch)
	lo, hi := f.bounds()
	r := Raster{
		Floor: floor, Origin: lo.Sub(geom.Vec2{X: cellSize, Z: cellSize}), CellSize: cellSize,
		Width: int(math.Ceil((hi.X-lo.X)/cellSize)) + 2, Height: int(math.Ceil((hi.Z-lo.Z)/cellSize)) + 2,
	}
	if r.Width*r.Height > 1<<24 {
		return Raster{}, errors.New("spatialsim: occupancy raster too large")
	}
	r.Cells = make([]Cell, r.Width*r.Height)
	r.Classes = make([]CellClass, r.Width*r.Height)
	index := func(p geom.Vec2) (int, int) {
		return int(math.Floor((p.X - r.Origin.X) / cellSize)), int(math.Floor((p.Z - r.Origin.Z) / cellSize))
	}
	set := func(ix, iz int, class CellClass) {
		r.Classes[iz*r.Width+ix] = class
		r.Cells[iz*r.Width+ix] = CellOccupied
		if class == ClassFree {
			r.Cells[iz*r.Width+ix] = CellFree
		}
	}
	// fill sets the cells whose centre satisfies inside, within a bounding box.
	fill := func(lo, hi geom.Vec2, class CellClass, inside func(geom.Vec2) bool) {
		x0, z0 := index(lo)
		x1, z1 := index(hi)
		for iz := max(z0, 0); iz <= min(z1, r.Height-1); iz++ {
			for ix := max(x0, 0); ix <= min(x1, r.Width-1); ix++ {
				if inside(r.Centre(ix, iz)) {
					set(ix, iz, class)
				}
			}
		}
	}
	// trace marks every cell a segment passes through.
	// ponytail: quarter-cell sampling, not an exact grid walk; fine for scoring rasters.
	trace := func(sg geom.Segment, class CellClass) {
		n := int(sg.Length()/(cellSize/4)) + 1
		for i := 0; i <= n; i++ {
			if ix, iz := index(sg.A.Lerp(sg.B, float64(i)/float64(n))); ix >= 0 && iz >= 0 && ix < r.Width && iz < r.Height {
				set(ix, iz, class)
			}
		}
	}
	for _, room := range f.rooms {
		rlo, rhi := room.Polygon[0], room.Polygon[0]
		for _, p := range room.Polygon {
			rlo = geom.Vec2{X: math.Min(rlo.X, p.X), Z: math.Min(rlo.Z, p.Z)}
			rhi = geom.Vec2{X: math.Max(rhi.X, p.X), Z: math.Max(rhi.Z, p.Z)}
		}
		fill(rlo, rhi, ClassFree, func(p geom.Vec2) bool { return geom.PointInPolygon(p, room.Polygon) })
	}
	// Furniture first, so that a wall or door it stands against keeps its class.
	for _, fu := range f.level.furniture {
		sh, ok := fu.at(t)
		if !ok {
			continue
		}
		b := geom.Vec2{X: sh.Bound(), Z: sh.Bound()}
		fill(sh.Centre.Sub(b), sh.Centre.Add(b), ClassFurniture, sh.Contains)
		if c := sh.Corners(); sh.Radius == 0 {
			for k := 0; k < 4; k++ {
				trace(geom.Segment{A: c[k], B: c[(k+1)%4]}, ClassFurniture)
			}
		}
	}
	for _, w := range tr.s.barriers(f.level, t).doors {
		trace(w, ClassDoor)
	}
	for _, w := range f.level.walls {
		trace(w, ClassWall)
	}
	return r, nil
}

// Entities is the true state of the site at the instant in the common
// entity form: devices that have started, actors (as tracks), furniture (as
// objects), wall tags (as landmarks), rooms (as regions) and portals.
func (tr Truth) Entities(at time.Time) []spatial.Entity {
	s := tr.s
	var out []spatial.Entity
	for _, d := range s.devices {
		st, ok := tr.Device(d.id, at)
		if !ok {
			continue
		}
		class := d.class
		if class == "" {
			class = string(d.kinds[0])
		}
		out = append(out, spatial.Entity{
			Kind: spatial.EntityDevice, ID: string(d.id), Floor: st.Floor, Position: st.Pose.Pos().Lift(st.Elevation),
			HasYaw: true, Yaw: st.Pose.Yaw, Extent: spatial.Extent{RadiusM: d.radius}, Class: class, Confidence: 1,
		})
	}
	elevation := func(floor spatial.FloorID) float64 { return s.floor(floor).level.elevation }
	extent := func(sh Shape) spatial.Extent {
		if sh.Radius > 0 {
			return spatial.Extent{RadiusM: sh.Radius}
		}
		c := sh.Corners()
		return spatial.Extent{Polygon: c[:]}
	}
	for _, a := range tr.Actors(at) {
		out = append(out, spatial.Entity{
			Kind: spatial.EntityTrack, ID: a.ID, Floor: a.Floor, Position: a.Shape.Centre.Lift(elevation(a.Floor)),
			HasYaw: true, Yaw: a.Shape.Yaw, Extent: extent(a.Shape), Class: a.Class, Confidence: 1,
		})
	}
	for _, o := range tr.Objects(at) {
		out = append(out, spatial.Entity{
			Kind: spatial.EntityObject, ID: o.ID, Floor: o.Floor, Position: o.Shape.Centre.Lift(elevation(o.Floor)),
			HasYaw: true, Yaw: o.Shape.Yaw, Extent: extent(o.Shape), Class: o.Class, Confidence: 1,
		})
	}
	for _, l := range tr.Landmarks() {
		out = append(out, spatial.Entity{Kind: spatial.EntityLandmark, ID: l.ID, Floor: l.Floor, Position: l.Pose.T, Confidence: 1})
	}
	for _, r := range tr.Rooms() {
		loc := spatial.Enclose(elevation(r.Floor), r.Polygon...)
		out = append(out, spatial.Entity{
			Kind: spatial.EntityRegion, ID: r.ID, Floor: r.Floor, Position: loc.Point(), Extent: spatial.Extent{Polygon: r.Polygon},
			Class: "region", Label: r.Label, Confidence: 1, UncertaintyM: loc.UncertaintyM,
		})
	}
	for _, p := range tr.Portals(at) {
		out = append(out, p.Entities()...)
	}
	for i := range out {
		out[i].ObservedAt = at
	}
	spatial.SortEntities(out)
	return out
}
