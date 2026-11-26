package semantic

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Segmenter is the streaming form of the package. It is given what other
// features publish (each floor's occupancy map, camera calibrations, device
// poses) and the camera frames in time order, and answers with the current
// Scene of a floor. It remembers what each floor's maps showed clear, fuses
// the object detections, and keeps the ids of rooms and doors stable from one
// scene to the next. Not safe for concurrent use.
type Segmenter struct {
	cfg     Config
	floors  map[spatial.FloorID]*floorState
	cameras map[sensorKey]spatial.Calibration
	poses   map[spatial.DeviceID]timedPose
	objects *Objects
	stats   Stats
}

// Stats counts what happened to the camera frames a Segmenter was given.
type Stats struct {
	Frames  int // camera frames ingested
	Skipped int // frames without a valid calibration, a fresh device pose or a floor below the camera
	Placed  int // object detections placed on a floor
}

type sensorKey struct {
	device spatial.DeviceID
	sensor spatial.SensorID
}

type timedPose struct {
	pose spatial.DevicePose
	at   time.Time
}

type floorState struct {
	elevation float64
	current   *grid.Grid
	history   *History
	last      *Scene
	rooms     int // ids handed out so far
	doors     int
}

// NewSegmenter returns a Segmenter with no floors.
func NewSegmenter(cfg Config) (*Segmenter, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	objects, err := NewObjects(cfg)
	if err != nil {
		return nil, err
	}
	return &Segmenter{
		cfg: cfg, floors: map[spatial.FloorID]*floorState{}, cameras: map[sensorKey]spatial.Calibration{},
		poses: map[spatial.DeviceID]timedPose{}, objects: objects,
	}, nil
}

// SetMap gives the current occupancy map of a floor and the floor's World
// elevation. The map joins the floor's persistence memory at once; the
// Segmenter keeps the grid and reads it on the next Scene call, so the caller
// must not change it afterwards (pass a Clone of a map that is still being
// built).
func (s *Segmenter) SetMap(floor spatial.FloorID, elevation float64, g *grid.Grid) error {
	if floor == "" || g == nil || math.IsNaN(elevation) || math.IsInf(elevation, 0) {
		return fmt.Errorf("%w: floor, elevation or map", ErrInsufficient)
	}
	f := s.floors[floor]
	if f == nil || f.history.clear.CellSize() != g.CellSize() {
		h, err := NewHistory(g.CellSize(), s.cfg.WallThickness)
		if err != nil {
			return err
		}
		f = &floorState{history: h}
		s.floors[floor] = f
	}
	f.elevation, f.current = elevation, g
	return f.history.Observe(g)
}

// SetCalibration gives the calibration of a camera. A calibration that is not
// valid withdraws the camera: its frames are skipped until a valid one
// arrives. Calibrations of other sensors are ignored.
func (s *Segmenter) SetCalibration(c spatial.Calibration) {
	if c.Kind != spatial.KindCamera {
		return
	}
	k := sensorKey{c.Device, c.Sensor}
	if c.State != spatial.CalibrationValid || c.Intrinsics == nil {
		delete(s.cameras, k)
		return
	}
	s.cameras[k] = c
}

// SetDevicePose gives the site pose of a mobile device at a site instant. A
// mounted camera's frame is placed with the latest pose when that is at most
// Config.MaxPoseAge from the frame.
func (s *Segmenter) SetDevicePose(p spatial.DevicePose, at time.Time) {
	s.poses[p.Device] = timedPose{p, at}
}

// Ingest takes one measurement. Camera frames feed the object map; every
// other kind is ignored. Frames must arrive in time order.
func (s *Segmenter) Ingest(m spatial.Measurement) error {
	frame, ok := m.(spatial.CameraFrame)
	if !ok {
		return nil
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	s.stats.Frames++
	cal, ok := s.cameras[sensorKey{frame.Device, frame.Sensor}]
	if !ok {
		s.stats.Skipped++
		return nil
	}
	at := frame.Time.Add(-cal.TimeOffset)
	cameraToSite, floorY := cal.SensorToParent, math.Inf(-1)
	if cal.Parent == spatial.ParentSite {
		// A fixed camera looks at the highest floor below it.
		for _, f := range s.floors {
			if f.elevation <= cameraToSite.T.Y && f.elevation > floorY {
				floorY = f.elevation
			}
		}
	} else if p, ok := s.poses[cal.ParentDevice]; ok && at.Sub(p.at).Abs() <= s.cfg.MaxPoseAge {
		// A mounted camera looks at the floor its carrier stands on.
		floorY = p.pose.PositionM[1]
		cameraToSite = geom.Pose3FromPose2(p.pose.Pose2(), floorY).Compose(cal.SensorToParent)
	}
	if math.IsInf(floorY, -1) {
		s.stats.Skipped++
		return nil
	}
	placed, err := Place(s.cfg, frame, *cal.Intrinsics, cameraToSite, floorY)
	if err != nil {
		return err
	}
	for _, obs := range placed {
		obs.Time = at
		s.objects.Add(obs)
	}
	s.stats.Placed += len(placed)
	return nil
}

// Stats reports the frame counters.
func (s *Segmenter) Stats() Stats { return s.stats }

// Floors lists the floors that have a map, in ascending order.
func (s *Segmenter) Floors() []spatial.FloorID {
	out := make([]spatial.FloorID, 0, len(s.floors))
	for id := range s.floors {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Objects returns the confirmed objects on a floor that vote for room types.
func (s *Segmenter) Objects(floor spatial.FloorID) ([]spatial.Object, error) {
	f := s.floors[floor]
	if f == nil {
		return nil, ErrUnknownFloor
	}
	return s.objects.List(floor, f.elevation), nil
}

// Scene segments the current state of a floor. A room that covers most of a
// room of the previous scene, and a door within Config.SnapRadius of a
// previous door, keep their ids.
func (s *Segmenter) Scene(floor spatial.FloorID) (*Scene, error) {
	f := s.floors[floor]
	if f == nil {
		return nil, ErrUnknownFloor
	}
	scene, err := Segment(s.cfg, Input{
		Floor: floor, Elevation: f.elevation, Map: f.current, History: f.history, Objects: s.objects.List(floor, f.elevation),
	})
	if err != nil {
		return nil, err
	}
	f.keepIDs(s.cfg, scene)
	f.last = scene
	return scene, nil
}

// keepIDs renames the rooms and doors of a new scene after the previous one.
func (f *floorState) keepIDs(cfg Config, s *Scene) {
	rooms, doors := make([]string, len(s.Rooms)), make([]string, len(s.Doors))
	if prev := f.last; prev != nil {
		// Rooms: the previous room sharing more than half of both.
		overlap := map[[2]int32]int{}
		size, was := make([]int, len(s.Rooms)+1), make([]int, len(prev.Rooms)+1)
		for _, k := range prev.region {
			was[k]++
		}
		for i, k := range s.region {
			size[k]++
			if j := prev.layer.index(s.layer.gridCell(i)); k != 0 && j >= 0 && prev.region[j] != 0 {
				overlap[[2]int32{k, prev.region[j]}]++
			}
		}
		for pair, n := range overlap {
			// At most one previous room can hold more than half of a room.
			if 2*n > size[pair[0]] && 2*n > was[pair[1]] {
				rooms[pair[0]-1] = prev.Rooms[pair[1]-1].Region.ID
			}
		}
		// Doors: the nearest previous door, each used once.
		taken := make([]bool, len(prev.Doors))
		for i, d := range s.Doors {
			best, bestD := -1, cfg.SnapRadius
			for j, p := range prev.Doors {
				if dist := d.Point().Dist(p.Point()); !taken[j] && dist <= bestD {
					best, bestD = j, dist
				}
			}
			if best >= 0 {
				taken[best] = true
				doors[i] = prev.Doors[best].ID
			}
		}
	}
	for i := range rooms {
		if rooms[i] == "" {
			f.rooms++
			rooms[i] = fmt.Sprintf("%s-r%d", s.Floor, f.rooms)
		}
	}
	for i := range doors {
		if doors[i] == "" {
			f.doors++
			doors[i] = fmt.Sprintf("%s-d%d", s.Floor, f.doors)
		}
	}
	s.rename(rooms, doors)
}

// rename gives the rooms and doors new ids and rewrites every reference.
func (s *Scene) rename(rooms, doors []string) {
	old := make(map[string]string, len(rooms))
	for i := range s.Rooms {
		old[s.Rooms[i].Region.ID] = rooms[i]
		s.Rooms[i].Region.ID = rooms[i]
		s.Rooms[i].Region.Doors = nil
	}
	for i := range s.Doors {
		d := &s.Doors[i]
		d.ID = doors[i]
		for _, end := range [2]*spatial.PortalEnd{&d.A, &d.B} {
			if end.Region == "" {
				continue
			}
			end.Region = old[end.Region]
			for k := range s.Rooms {
				if rm := &s.Rooms[k].Region; rm.ID == end.Region && len(rm.Doors) < spatial.MaxRegionDoors {
					rm.Doors = append(rm.Doors, d.ID)
				}
			}
		}
	}
}
