package semantic

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Feature and ModelVersion identify the records this package derives: their
// provenance carries ModelID "wendy.spatial.semantic".
const (
	Feature      = "semantic"
	ModelVersion = "1"
	// LayerName is the Layer of the cell class tiles.
	LayerName = "semantic"
)

// Input is what one floor's segmentation is computed from.
type Input struct {
	Floor     spatial.FloorID
	Elevation float64
	// Map is the floor's occupancy map.
	Map *grid.Grid
	// History is the floor's persistence memory, or nil: without it a
	// closed door is part of its wall.
	History *History
	// Objects are the class-labelled objects placed in the site frame (see
	// Objects). Those within Config.FloorTolerance of Elevation vote for
	// the type of the room they stand in.
	Objects []spatial.Object
}

// Room is one region of a floor with the evidence behind it.
type Room struct {
	// Region is the published payload. Label is empty and LabelConfidence
	// zero when the votes do not decide the room type.
	Region spatial.Region
	// Votes is the vote weight per room type.
	Votes map[string]float64
	// Enclosure is the share of the room's outline that is structure or
	// another room rather than unobserved space: 1 for a room seen
	// completely, lower while part of it is still a frontier. Regions below
	// Config.MinEnclosure are not reported at all.
	Enclosure float64
	// InscribedM is the radius of the widest circle that fits in the room.
	InscribedM float64
}

// Scene is the semantic segmentation of one floor: a class per map cell,
// the rooms, and the doors between them. Rooms are the nodes of the floor's
// topological graph and Doors its edges.
type Scene struct {
	Floor     spatial.FloorID
	Elevation float64
	Rooms     []Room
	// Doors are the passages between rooms and the closed doors found by
	// persistence. Both ends of a door share its position; an end's Region
	// is empty where the door leads out of the mapped rooms.
	Doors []spatial.Portal
	// Unexplained is the residual of the cell classification: the share of
	// occupied cells that lie on no straight run and beside no wall, and
	// are furniture only by default.
	Unexplained float64

	layer  *layer
	region []int32 // per cell: 0, or 1 + index into Rooms
}

// Segment computes the semantic scene of one floor: cell classes from the
// geometry of the map and its persistence, rooms by a distance-transform
// watershed, doors where rooms touch, and room types from object votes.
// It returns ErrInsufficient when the map holds no room.
func Segment(cfg Config, in Input) (*Scene, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if in.Map == nil || in.Floor == "" {
		return nil, fmt.Errorf("%w: no map or floor", ErrInsufficient)
	}
	if in.History != nil && in.History.clear.CellSize() != in.Map.CellSize() {
		return nil, fmt.Errorf("%w: history cell size", grid.ErrIncompatible)
	}
	l, ok := classify(cfg, in.Map, in.History)
	if !ok {
		return nil, fmt.Errorf("%w: empty map", ErrInsufficient)
	}
	b := segmentRooms(cfg, l)
	if len(b.inscribed) == 0 {
		return nil, fmt.Errorf("%w: no free space of %.1f m2", ErrInsufficient, cfg.MinRoomArea)
	}
	s := &Scene{Floor: in.Floor, Elevation: in.Elevation, Unexplained: l.unexplained, layer: l, region: b.region}
	s.Rooms = make([]Room, len(b.inscribed))
	for i, k := range b.region {
		if k == 0 {
			continue
		}
		rm := &s.Rooms[k-1]
		if rm.Region.CellCounts == nil {
			rm.Region.CellCounts = map[string]int{}
		}
		rm.Region.CellCounts[l.class[i].String()]++
	}
	for k := range s.Rooms {
		rm := &s.Rooms[k]
		cells := 0
		for _, n := range rm.Region.CellCounts {
			cells += n
		}
		poly := l.outline(b.region, int32(k+1), cfg.PolygonTolerance)
		rm.Region = spatial.Region{
			ID: fmt.Sprintf("%s-r%d", in.Floor, k+1), Floor: in.Floor, Polygon: poly,
			AreaM2: float64(cells) * l.cell * l.cell, CellCounts: rm.Region.CellCounts,
			Location: spatial.Enclose(in.Elevation, poly...),
		}
		rm.InscribedM, rm.Enclosure = b.inscribed[k], b.enclosure[k]
		rm.Votes = map[string]float64{}
		if d := 2 * rm.InscribedM; cfg.CorridorVotes > 0 && rm.Region.AreaM2 >= cfg.CorridorElongation*d*d {
			rm.Votes[cfg.CorridorType] += cfg.CorridorVotes
		}
	}
	s.vote(cfg, in.Objects)
	s.doors(cfg, b)
	for k := range s.Rooms {
		if err := s.Rooms[k].Region.Validate(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// vote adds the objects' room-type votes and labels each room.
func (s *Scene) vote(cfg Config, objects []spatial.Object) {
	l := s.layer
	snap := l.cells(cfg.SnapRadius)
	for _, o := range objects {
		row := cfg.RoomTypes[o.Class]
		if len(row) == 0 || math.Abs(o.PositionM[1]-s.Elevation) > cfg.FloorTolerance {
			continue
		}
		c := grid.Cell{IX: int32(math.Floor(o.PositionM[0] / l.cell)), IZ: int32(math.Floor(o.PositionM[2] / l.cell))}
		k := s.nearestRoom(c, snap)
		if k == 0 {
			continue
		}
		for kind, w := range row {
			s.Rooms[k-1].Votes[kind] += w
		}
	}
	for k := range s.Rooms {
		rm := &s.Rooms[k]
		kinds := make([]string, 0, len(rm.Votes))
		total := 0.0
		for kind, w := range rm.Votes {
			kinds = append(kinds, kind)
			total += w
		}
		sort.Strings(kinds)
		best := ""
		for _, kind := range kinds {
			if best == "" || rm.Votes[kind] > rm.Votes[best] {
				best = kind
			}
		}
		// Too few votes, or no clear majority: unknown rather than guessed.
		if best == "" || total < cfg.MinVotes || rm.Votes[best] <= cfg.MinShare*total {
			continue
		}
		rm.Region.Label = best
		// One more vote against, so that a single object is never certain.
		rm.Region.LabelConfidence = rm.Votes[best] / (total + 1)
	}
}

// nearestRoom is the room of the cell, or of the nearest room cell within
// reach cells of it; 0 when there is none.
func (s *Scene) nearestRoom(c grid.Cell, reach int) int32 {
	l := s.layer
	best, bestD := int32(0), math.MaxInt
	for dz := -reach; dz <= reach; dz++ {
		for dx := -reach; dx <= reach; dx++ {
			i := l.index(grid.Cell{IX: c.IX + int32(dx), IZ: c.IZ + int32(dz)})
			if d := dx*dx + dz*dz; i >= 0 && s.region[i] != 0 && d < bestD && d <= reach*reach {
				best, bestD = s.region[i], d
			}
		}
	}
	return best
}

// doors builds the portals: one per passage between two rooms, and one per
// closed door leaf found by persistence.
func (s *Scene) doors(cfg Config, b basins) {
	l := s.layer
	type found struct {
		a, b   int32 // 1-based rooms; b may be 0
		centre geom.Vec2
		width  float64
		closed bool
		conf   float64
	}
	var all []found
	for _, b := range l.boundaries(cfg, b) {
		// A passage is the surer the smaller it is next to the rooms it joins.
		side := math.Sqrt(math.Min(s.Rooms[b.a-1].Region.AreaM2, s.Rooms[b.b-1].Region.AreaM2))
		all = append(all, found{a: b.a, b: b.b, centre: b.centre, width: b.width, conf: min(max(1-b.width/side, 0), 1)})
	}
	reach := l.cells(cfg.TipClearance)
	for _, leaf := range l.components(func(i int) bool { return l.class[i] == ClassDoor }) {
		var sum geom.Vec2
		beside := map[int32]int{}
		for _, i := range leaf {
			sum = sum.Add(l.centre(int(i)))
			ix, iz := int(i)%l.w, int(i)/l.w
			for _, d := range [4][2]int{{reach, 0}, {-reach, 0}, {0, reach}, {0, -reach}} {
				if l.inside(ix+d[0], iz+d[1]) {
					if k := s.region[(iz+d[1])*l.w+ix+d[0]]; k != 0 {
						beside[k]++
					}
				}
			}
		}
		// The two rooms seen most often beside the leaf; fewer where it
		// leads out of the mapped rooms.
		_, _, width := l.span(leaf)
		f := found{centre: sum.Scale(1 / float64(len(leaf))), width: width, closed: true, conf: 1}
		for k, n := range beside {
			switch {
			case f.a == 0 || n > beside[f.a] || (n == beside[f.a] && k < f.a):
				f.a, f.b = k, f.a
			case f.b == 0 || n > beside[f.b] || (n == beside[f.b] && k < f.b):
				f.b = k
			}
		}
		all = append(all, f)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].centre.Z != all[j].centre.Z {
			return all[i].centre.Z < all[j].centre.Z
		}
		return all[i].centre.X < all[j].centre.X
	})
	s.Doors = nil
	for n, f := range all {
		p := spatial.Portal{
			ID: fmt.Sprintf("%s-d%d", s.Floor, n+1), Kind: spatial.PortalDoor, State: spatial.PortalOpen,
			WidthM: f.width, Confidence: f.conf, Location: spatial.At(f.centre.Lift(s.Elevation), f.width/2),
		}
		if f.closed {
			p.State = spatial.PortalClosed
		} else if f.width > cfg.DoorMaxWidth {
			p.Kind = spatial.PortalOpening
		}
		p.A = spatial.PortalEnd{Floor: s.Floor, PositionM: p.PositionM}
		p.B = p.A
		if f.a != 0 {
			p.A.Region = s.Rooms[f.a-1].Region.ID
			s.Rooms[f.a-1].Region.Doors = append(s.Rooms[f.a-1].Region.Doors, p.ID)
		}
		if f.b != 0 {
			p.B.Region = s.Rooms[f.b-1].Region.ID
			s.Rooms[f.b-1].Region.Doors = append(s.Rooms[f.b-1].Region.Doors, p.ID)
		}
		s.Doors = append(s.Doors, p)
	}
	// A Region lists a bounded number of doors; the portals keep them all.
	for k := range s.Rooms {
		if d := s.Rooms[k].Region.Doors; len(d) > spatial.MaxRegionDoors {
			s.Rooms[k].Region.Doors = d[:spatial.MaxRegionDoors]
		}
	}
}

// ClassAt is the class of the map cell containing a site point.
func (s *Scene) ClassAt(p geom.Vec2) Class {
	if i := s.layer.index(s.cellAt(p)); i >= 0 {
		return s.layer.class[i]
	}
	return ClassUnknown
}

// RoomAt is the room whose cells contain a site point.
func (s *Scene) RoomAt(p geom.Vec2) (Room, bool) {
	if i := s.layer.index(s.cellAt(p)); i >= 0 && s.region[i] != 0 {
		return s.Rooms[s.region[i]-1], true
	}
	return Room{}, false
}

func (s *Scene) cellAt(p geom.Vec2) grid.Cell {
	return grid.Cell{IX: int32(math.Floor(p.X / s.layer.cell)), IZ: int32(math.Floor(p.Z / s.layer.cell))}
}

// Neighbours lists the rooms joined to a room by a door that is not closed,
// in ascending id order: the edges of the topological graph at that node.
func (s *Scene) Neighbours(roomID string) []string {
	var out []string
	for _, d := range s.Doors {
		if d.State == spatial.PortalClosed || d.A.Region == "" || d.B.Region == "" {
			continue
		}
		if other := d.B.Region; d.A.Region == roomID {
			out = append(out, other)
		} else if d.B.Region == roomID {
			out = append(out, d.A.Region)
		}
	}
	sort.Strings(out)
	return compact(out)
}

func compact(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Regions returns the region payloads, one per room.
func (s *Scene) Regions() []spatial.Region {
	out := make([]spatial.Region, len(s.Rooms))
	for i, rm := range s.Rooms {
		out[i] = rm.Region
	}
	return out
}

// LayerTiles returns the cell class layer as sealed tiles of side cells, in
// (IZ, IX) order, on the tiling of the occupancy map tiles. Tiles without an
// observed cell are left out.
func (s *Scene) LayerTiles(side int) ([]spatial.LayerTile, error) {
	l := s.layer
	if side < 1 || side > spatial.MaxTileSide {
		return nil, fmt.Errorf("%w: tile side %d", ErrConfig, side)
	}
	tiles := map[[2]int32]*spatial.LayerTile{}
	var keys [][2]int32
	for i, c := range l.class {
		if c == ClassUnknown {
			continue
		}
		cell := l.gridCell(i)
		tx, tz := grid.TileOf(cell, side)
		t := tiles[[2]int32{tz, tx}]
		if t == nil {
			t = &spatial.LayerTile{
				Floor: s.Floor, Layer: LayerName, IX: tx, IZ: tz, Side: side, CellSizeM: l.cell,
				OriginXZ: spatial.TileOrigin(tx, tz, side, l.cell), Cells: make([]byte, side*side), ClassNames: append([]string(nil), ClassNames...),
			}
			t.Seal(s.Elevation)
			tiles[[2]int32{tz, tx}] = t
			keys = append(keys, [2]int32{tz, tx})
		}
		t.Cells[int(cell.IZ-tz*int32(side))*side+int(cell.IX-tx*int32(side))] = byte(c)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]spatial.LayerTile, len(keys))
	for i, k := range keys {
		out[i] = *tiles[k]
		if err := out[i].Validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Payloads returns everything the scene publishes: its regions, its doors as
// portals, and the cell class layer in tiles of side cells.
func (s *Scene) Payloads(side int) ([]spatial.Payload, error) {
	tiles, err := s.LayerTiles(side)
	if err != nil {
		return nil, err
	}
	out := make([]spatial.Payload, 0, len(s.Rooms)+len(s.Doors)+len(tiles))
	for _, rm := range s.Rooms {
		out = append(out, rm.Region)
	}
	for _, d := range s.Doors {
		out = append(out, d)
	}
	for _, t := range tiles {
		out = append(out, t)
	}
	return out, nil
}

// Components wraps payloads as World components derived by this feature:
// Inferred provenance with ModelID "wendy.spatial.semantic" and the given
// input ancestry (the scans behind the map and the camera frames behind the
// objects), reduced to spatial.MaxInputs. Every component takes the same
// version, frame, timing and TTL.
func Components(scope spatial.Scope, version world.Version, frame world.FrameRef, timing world.Timing, ttl time.Duration, appVersion string, inputs []world.Input, payloads ...spatial.Payload) ([]world.Component, error) {
	prov, err := spatial.InferredProvenance(Feature, ModelVersion, appVersion, spatial.ReduceInputs(inputs))
	if err != nil {
		return nil, err
	}
	out := make([]world.Component, len(payloads))
	for i, p := range payloads {
		if out[i], err = spatial.ToComponent(scope, version, frame, timing, ttl, prov, p); err != nil {
			return nil, err
		}
	}
	return out, nil
}
