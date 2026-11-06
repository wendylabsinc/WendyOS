package sitenav

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

type Vertex struct{ X, Y, Z float64 }
type Triangle [3]Vertex
type wallSurface struct {
	direction Point
	offset    float64
}
type RoomMap struct {
	FloorY        float64
	walls         []Point
	floor         [][3]Point
	low, high     Point
	fieldLow      Point
	distance      []float64
	surfaceAt     []int
	surfaces      []wallSurface
	width, height int
}

var ErrAmbiguousRoom = errors.New("The scan fits more than one room position or heading. More distinct wall surfaces are needed to locate Go2.")

type PoseCandidate struct {
	Position                Point
	Rotation, RMS, Coverage float64
	cost                    float64
	floorConflict           float64
}
type wallSample struct {
	point, direction Point
	oriented         bool
}

// A count alone cannot distinguish a useful corner from many returns along
// one wall. Require independent columns and two-dimensional spatial support.
// This only admits a position suggestion; confirmation and navigation checks
// remain separate.
func localizationStructure(points []Point) error {
	columns := map[Cell]Point{}
	for _, p := range points {
		if !p.Valid() {
			return errors.New("Invalid localization point.")
		}
		columns[cell(p)] = p
	}
	if len(columns) < 24 {
		return fmt.Errorf("LiDAR sees %d wall landmarks; at least 24 are needed to suggest a position.", len(columns))
	}
	var mean Point
	for _, p := range columns {
		mean.X += p.X
		mean.Y += p.Y
	}
	n := float64(len(columns))
	mean.X /= n
	mean.Y /= n
	xx, xy, yy := 0.0, 0.0, 0.0
	for _, p := range columns {
		x, y := p.X-mean.X, p.Y-mean.Y
		xx += x * x
		xy += x * y
		yy += y * y
	}
	xx /= n
	xy /= n
	yy /= n
	difference := math.Hypot(xx-yy, 2*xy)
	major, minor := (xx+yy+difference)/2, (xx+yy-difference)/2
	if major < .25 || minor < .04 {
		return errors.New("LiDAR wall landmarks are too concentrated or lie along one wall. More room structure is needed to locate Go2.")
	}
	return nil
}

// A single-level section through vertical geometry, 70 cm above the floor.
// This proposes a visual alignment only; it is never navigation free space.
func RoomFromMesh(triangles []Triangle) (RoomMap, error) {
	return roomFromMeshAtHeight(triangles, .7)
}

func roomFromMeshAtHeight(triangles []Triangle, height float64) (RoomMap, error) {
	var room RoomMap
	var wallSurfaceIDs []int
	if len(triangles) < 4 || len(triangles) > 100000 {
		return room, errors.New("The room geometry is too small or too complex for scan matching.")
	}
	minimum := math.Inf(1)
	for _, t := range triangles {
		for _, p := range t {
			if !finite(p.X) || !finite(p.Y) || !finite(p.Z) || math.Max(math.Abs(p.X), math.Max(math.Abs(p.Y), math.Abs(p.Z))) > 1000 {
				return room, errors.New("Invalid room coordinates.")
			}
			minimum = math.Min(minimum, p.Y)
		}
	}
	// Choose a substantial horizontal surface near the bottom of the model.
	areas, levels := map[int]float64{}, map[int]float64{}
	for _, t := range triangles {
		area, up := triangleArea(t)
		y := (t[0].Y + t[1].Y + t[2].Y) / 3
		if up > .98 && y < minimum+.25 {
			k := int(math.Round(y / .05))
			areas[k] += area
			levels[k] += area * y
		}
	}
	best, bestArea := 0, 0.0
	for k, a := range areas {
		if a > bestArea+1e-6 || (math.Abs(a-bestArea) <= 1e-6 && k > best) {
			best, bestArea = k, a
		}
	}
	if bestArea < 1 {
		return room, errors.New("Scan matching needs a visible, level room floor of at least one square metre.")
	}
	room.FloorY = levels[best] / bestArea
	room.low = Point{math.Inf(1), math.Inf(1)}
	room.high = Point{math.Inf(-1), math.Inf(-1)}
	section := room.FloorY + height
	for _, t := range triangles {
		_, up := triangleArea(t)
		y := (t[0].Y + t[1].Y + t[2].Y) / 3
		if up > .98 && math.Abs(y-room.FloorY) < .06 {
			if len(room.floor) >= 5000 {
				return room, errors.New("The floor mesh is too detailed for local scan matching.")
			}
			f := [3]Point{}
			for i, v := range t {
				p := Point{v.X, -v.Z}
				f[i] = p
				room.low.X = math.Min(room.low.X, p.X)
				room.low.Y = math.Min(room.low.Y, p.Y)
				room.high.X = math.Max(room.high.X, p.X)
				room.high.Y = math.Max(room.high.Y, p.Y)
			}
			room.floor = append(room.floor, f)
		}
		if up > .2 {
			continue
		}
		var cuts []Point
		for i, a := range t {
			b := t[(i+1)%3]
			if (a.Y < section) == (b.Y < section) {
				continue
			}
			u := (section - a.Y) / (b.Y - a.Y)
			cuts = append(cuts, Point{a.X + u*(b.X-a.X), -(a.Z + u*(b.Z-a.Z))})
		}
		if len(cuts) == 2 {
			length := cuts[0].Distance(cuts[1])
			if length < .01 {
				continue
			}
			direction := Point{(cuts[1].X - cuts[0].X) / length, (cuts[1].Y - cuts[0].Y) / length}
			if direction.X < 0 || (direction.X == 0 && direction.Y < 0) {
				direction.X, direction.Y = -direction.X, -direction.Y
			}
			offset := cuts[0].X*(-direction.Y) + cuts[0].Y*direction.X
			id := -1
			for i, surface := range room.surfaces {
				if surface.direction.X*direction.X+surface.direction.Y*direction.Y > .995 && math.Abs(surface.offset-offset) < .12 {
					id = i
					break
				}
			}
			if id < 0 {
				id = len(room.surfaces)
				room.surfaces = append(room.surfaces, wallSurface{direction, offset})
			}
			n := int(math.Ceil(cuts[0].Distance(cuts[1]) / .05))
			if n > 1000 {
				return room, errors.New("Room walls are too large for local scan matching.")
			}
			if len(room.walls)+n+1 > 20000 {
				return room, errors.New("The wall mesh is too detailed for local scan matching.")
			}
			for i := 0; i <= n; i++ {
				u := float64(i) / float64(max(n, 1))
				room.walls = append(room.walls, Point{cuts[0].X + u*(cuts[1].X-cuts[0].X), cuts[0].Y + u*(cuts[1].Y-cuts[0].Y)})
				wallSurfaceIDs = append(wallSurfaceIDs, id)
			}
		}
	}
	if len(room.walls) < 40 || room.high.X-room.low.X > 30 || room.high.Y-room.low.Y > 30 || room.high.X-room.low.X < 1 || room.high.Y-room.low.Y < 1 {
		return room, errors.New("Scan matching needs room walls and a floor no larger than 30 metres across.")
	}
	room.fieldLow = Point{room.low.X - .6, room.low.Y - .6}
	room.width = int(math.Ceil((room.high.X-room.low.X+1.2)/.05)) + 1
	room.height = int(math.Ceil((room.high.Y-room.low.Y+1.2)/.05)) + 1
	room.distance = make([]float64, room.width*room.height)
	room.surfaceAt = make([]int, len(room.distance))
	for i := range room.distance {
		room.distance[i] = .36
		room.surfaceAt[i] = -1
	}
	for at, p := range room.walls {
		cx, cy := int(math.Round((p.X-room.fieldLow.X)/.05)), int(math.Round((p.Y-room.fieldLow.Y)/.05))
		for dy := -12; dy <= 12; dy++ {
			for dx := -12; dx <= 12; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= room.width || y >= room.height {
					continue
				}
				px, py := room.fieldLow.X+float64(x)*.05, room.fieldLow.Y+float64(y)*.05
				d := (p.X-px)*(p.X-px) + (p.Y-py)*(p.Y-py)
				i := y*room.width + x
				if d < room.distance[i] {
					room.distance[i] = d
					room.surfaceAt[i] = wallSurfaceIDs[at]
				}
			}
		}
	}
	return room, nil
}

func triangleArea(t Triangle) (area, up float64) {
	a, b := Vertex{t[1].X - t[0].X, t[1].Y - t[0].Y, t[1].Z - t[0].Z}, Vertex{t[2].X - t[0].X, t[2].Y - t[0].Y, t[2].Z - t[0].Z}
	x, y, z := a.Y*b.Z-a.Z*b.Y, a.Z*b.X-a.X*b.Z, a.X*b.Y-a.Y*b.X
	n := math.Sqrt(x*x + y*y + z*z)
	if n == 0 {
		return 0, 0
	}
	return n / 2, math.Abs(y) / n
}
func (m RoomMap) distanceSquared(p Point) float64 {
	distance, _ := m.nearestSurface(p)
	return distance
}

func (m RoomMap) nearestSurface(p Point) (float64, int) {
	x, y := int(math.Round((p.X-m.fieldLow.X)/.05)), int(math.Round((p.Y-m.fieldLow.Y)/.05))
	if x < 0 || y < 0 || x >= m.width || y >= m.height {
		return .36, -1
	}
	i := y*m.width + x
	return m.distance[i], m.surfaceAt[i]
}
func (m RoomMap) onFloor(p Point) bool {
	if m.distanceSquared(p) < .35*.35 {
		return false
	}
	return m.containsFloor(p)
}

func (m RoomMap) containsFloor(p Point) bool {
	cross := func(a, b Point) float64 { return (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X) }
	for _, t := range m.floor {
		a, b, c := cross(t[0], t[1]), cross(t[1], t[2]), cross(t[2], t[0])
		if (a >= 0 && b >= 0 && c >= 0) || (a <= 0 && b <= 0 && c <= 0) {
			return true
		}
	}
	return false
}

// MatchRoom searches distinct position/yaw hypotheses but proposes only a
// separated best fit supported by independent wall directions. Confirmation
// remains required. Coverage measures correspondence, not pose probability.
func MatchRoom(ctx context.Context, room RoomMap, points []Point, origin Point) ([]PoseCandidate, error) {
	return MatchRoomWithFloor(ctx, room, points, origin, nil)
}

// MatchRoomWithFloor uses measured, obstacle-free floor as contradictory evidence:
// a proposed wall must not pass through it. Missing floor is never invented, and
// points beyond the modeled floor are not treated as contradictions because an
// open doorway can reveal a neighboring room. This does not authorize navigation.
func MatchRoomWithFloor(ctx context.Context, room RoomMap, points []Point, origin Point, clearFloor []Point) ([]PoseCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := localizationStructure(points); err != nil {
		return nil, err
	}
	points = append([]Point(nil), points...)
	sort.Slice(points, func(i, j int) bool {
		if points[i].X == points[j].X {
			return points[i].Y < points[j].Y
		}
		return points[i].X < points[j].X
	})
	var unique []Point
	seen := map[Cell]bool{}
	for _, p := range points {
		if p.Distance(origin) < 8 && !seen[cell(p)] {
			unique = append(unique, Point{p.X - origin.X, p.Y - origin.Y})
			seen[cell(p)] = true
		}
	}
	if err := localizationStructure(unique); err != nil {
		return nil, err
	}
	neighborhoods := map[Cell][]Point{}
	for _, p := range unique {
		k := cell(p)
		neighborhoods[k] = append(neighborhoods[k], p)
	}
	var scan []wallSample
	count := min(600, len(unique))
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := unique[i*len(unique)/count]
		k := cell(p)
		var neighbors []Point
		for dy := -5; dy <= 5; dy++ {
			for dx := -5; dx <= 5; dx++ {
				for _, other := range neighborhoods[Cell{k.X + dx, k.Y + dy}] {
					if p.Distance(other) <= .45 {
						neighbors = append(neighbors, other)
					}
				}
			}
		}
		direction, oriented := planarWallDirection(neighbors)
		scan = append(scan, wallSample{p, direction, oriented})
	}
	coarse := scan
	if len(scan) > 180 {
		coarse = nil
		for i := 0; i < 180; i++ {
			coarse = append(coarse, scan[i*len(scan)/180])
		}
	}
	floorCells := map[Cell]bool{}
	for _, p := range clearFloor {
		if !p.Valid() {
			return nil, errors.New("Invalid observed floor point.")
		}
		if p.Distance(origin) < 8 {
			floorCells[cell(p)] = true
		}
	}
	// Use only the interior of measured clear patches. A wall beside a floor
	// cell is expected; a wall through a 30 cm-wide clear patch is contradictory.
	var free []Point
	for k := range floorCells {
		interior := true
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				interior = interior && floorCells[Cell{k.X + dx, k.Y + dy}]
			}
		}
		if interior {
			p := center(k)
			free = append(free, Point{p.X - origin.X, p.Y - origin.Y})
		}
	}
	sort.Slice(free, func(i, j int) bool {
		if free[i].X == free[j].X {
			return free[i].Y < free[j].Y
		}
		return free[i].X < free[j].X
	})
	if len(free) > 240 {
		all := free
		free = make([]Point, 240)
		for i := range free {
			free[i] = all[i*len(all)/len(free)]
		}
	}
	correspondence := func(p wallSample, q Point, s, cos float64) (float64, int, bool) {
		distance, id := room.nearestSurface(q)
		if id < 0 {
			return distance, id, false
		}
		if p.oriented {
			direction := Point{cos*p.direction.X - s*p.direction.Y, s*p.direction.X + cos*p.direction.Y}
			wall := room.surfaces[id].direction
			if math.Abs(direction.X*wall.X+direction.Y*wall.Y) < math.Cos(math.Pi/12) {
				return distance, id, false
			}
		}
		return distance, id, true
	}
	fit := func(c PoseCandidate, sample []wallSample) PoseCandidate {
		if !room.onFloor(c.Position) {
			c.RMS = 1
			c.cost = 1
			return c
		}
		s, cos := math.Sincos(c.Rotation)
		total, robust, inliers := 0.0, 0.0, 0
		for _, p := range sample {
			q := Point{c.Position.X + cos*p.point.X - s*p.point.Y, c.Position.Y + s*p.point.X + cos*p.point.Y}
			d, _, consistent := correspondence(p, q, s, cos)
			total += d
			// Unmodeled chairs and other clutter are outliers. Once a return
			// is beyond wall tolerance it must not pull the entire scan toward
			// a wall. Keep the untrimmed RMS for honest fit reporting.
			if consistent {
				robust += math.Min(d, .25*.25)
			} else {
				robust += .25 * .25
			}
			if consistent && d <= .2*.2 {
				inliers++
			}
		}
		c.RMS = math.Sqrt(total / float64(len(sample)))
		c.cost = robust / float64(len(sample))
		c.Coverage = float64(inliers) / float64(len(sample))
		if len(free) >= 24 {
			conflicts := 0
			for _, p := range free {
				q := Point{c.Position.X + cos*p.X - s*p.Y, c.Position.Y + s*p.X + cos*p.Y}
				if room.distanceSquared(q) < .075*.075 {
					conflicts++
				}
			}
			c.floorConflict = float64(conflicts) / float64(len(free))
			c.cost += c.floorConflict * .25
		}
		return c
	}
	independentWalls := func(c PoseCandidate) bool {
		type support struct {
			count     int
			low, high float64
			points    []Point
		}
		groups := make([]support, len(room.surfaces))
		s, cos := math.Sincos(c.Rotation)
		for _, p := range scan {
			q := Point{c.Position.X + cos*p.point.X - s*p.point.Y, c.Position.Y + s*p.point.X + cos*p.point.Y}
			d, id, consistent := correspondence(p, q, s, cos)
			if !consistent || d > .2*.2 {
				continue
			}
			g := &groups[id]
			wall := room.surfaces[id].direction
			along := q.X*wall.X + q.Y*wall.Y
			if g.count == 0 {
				g.low, g.high = along, along
			}
			g.count++
			g.points = append(g.points, q)
			g.low, g.high = math.Min(g.low, along), math.Max(g.high, along)
		}
		// Model wall IDs alone do not prove independent observed directions.
		// Test the entire corresponding group, including sparse points whose
		// individual neighborhoods were too small to estimate a direction.
		for i := range groups {
			g := &groups[i]
			direction, planar := planarWallDirection(g.points)
			wall := room.surfaces[i].direction
			if !planar || math.Abs(direction.X*wall.X+direction.Y*wall.Y) < math.Cos(math.Pi/12) {
				g.count = 0
			}
		}
		for i, a := range groups {
			if a.count < 6 || a.high-a.low < .6 {
				continue
			}
			for j := i + 1; j < len(groups); j++ {
				b := groups[j]
				if b.count < 6 || b.high-b.low < .6 {
					continue
				}
				u, v := room.surfaces[i].direction, room.surfaces[j].direction
				if math.Abs(u.X*v.Y-u.Y*v.X) >= math.Sin(math.Pi/9) {
					return true
				}
			}
		}
		return false
	}
	candidates, err := searchRoom(ctx, room, func(c PoseCandidate) PoseCandidate { return fit(c, scan) }, func(c PoseCandidate) PoseCandidate { return fit(c, coarse) }, func(c PoseCandidate) bool {
		return c.cost < .04 && c.Coverage >= .55 && c.floorConflict <= .05 && independentWalls(c)
	})
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, errors.New("No room position fits the observed wall directions and clear floor well enough to locate Go2.")
	}
	best := candidates[0]
	for _, alternative := range candidates[1:] {
		// Five centimetres of residual error is within the voxel/map sampling
		// scale. Do not turn near-equal, distinct hypotheses into a guessed yaw.
		if alternative.cost-best.cost <= math.Max(.05*.05, best.cost*.15) {
			return nil, ErrAmbiguousRoom
		}
	}
	return candidates[:1], nil
}

func searchRoom(ctx context.Context, room RoomMap, fit, coarseFit func(PoseCandidate) PoseCandidate, valid func(PoseCandidate) bool) ([]PoseCandidate, error) {
	separate := func(a, b PoseCandidate) bool {
		return a.Position.Distance(b.Position) > .6 || math.Abs(math.Remainder(a.Rotation-b.Rotation, 2*math.Pi)) > .35
	}
	insert := func(list []PoseCandidate, c PoseCandidate, limit int) []PoseCandidate {
		for i, old := range list {
			if !separate(old, c) {
				if c.cost < old.cost {
					list[i] = c
				}
				sort.Slice(list, func(i, j int) bool { return list[i].cost < list[j].cost })
				return list
			}
		}
		list = append(list, c)
		sort.Slice(list, func(i, j int) bool { return list[i].cost < list[j].cost })
		if len(list) > limit {
			list = list[:limit]
		}
		return list
	}
	var seeds []PoseCandidate
	for yaw := -math.Pi; yaw < math.Pi; yaw += math.Pi / 12 {
		var headingSeeds []PoseCandidate
		for y := room.low.Y; y <= room.high.Y; y += .3 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := room.low.X; x <= room.high.X; x += .3 {
				c := coarseFit(PoseCandidate{Position: Point{x, y}, Rotation: yaw})
				if c.RMS < .5 {
					headingSeeds = insert(headingSeeds, c, 4)
				}
			}
		}
		// Preserve competing headings through refinement. A dense region must
		// not consume the entire seed budget before alternatives are tested.
		seeds = append(seeds, headingSeeds...)
	}
	var candidates []PoseCandidate
	for _, seed := range seeds {
		c := fit(seed)
		for _, step := range []struct{ position, angle float64 }{{.15, math.Pi / 24}, {.05, math.Pi / 90}, {.02, math.Pi / 180}} {
			for iteration := 0; iteration < 12; iteration++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				best := c
				for dx := -1; dx <= 1; dx++ {
					for dy := -1; dy <= 1; dy++ {
						for da := -1; da <= 1; da++ {
							n := fit(PoseCandidate{Position: Point{c.Position.X + float64(dx)*step.position, c.Position.Y + float64(dy)*step.position}, Rotation: c.Rotation + float64(da)*step.angle})
							if n.cost < best.cost {
								best = n
							}
						}
					}
				}
				if best.cost >= c.cost {
					break
				}
				c = best
			}
		}
		if valid(c) {
			c.Rotation = math.Remainder(c.Rotation, 2*math.Pi)
			candidates = insert(candidates, c, 24)
		}
	}

	return candidates, nil
}
