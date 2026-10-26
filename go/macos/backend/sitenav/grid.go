// Package sitenav plans bounded Go2 journeys through observed floor cells.
// Unknown space, holes and slopes are not traversable. Site alignment is
// session-local, manually placed or confirmed from scan-matching candidates.
package sitenav

import (
	"container/heap"
	"errors"
	"math"
	"sort"
	"time"
)

const CellSize = 0.10

// Clearance is outside the robot, not a radius measured from its center.
const Clearance = 0.10

// Conservative standing envelope around the odometry base origin. Rounded up
// from Unitree's 70 x 31 cm standing dimensions (https://www.unitree.com/go2/).
// Attachments extending outside this envelope require a larger footprint.
const HalfLength = 0.38
const HalfWidth = 0.18
const MaxDistance = 8.0

type Point struct{ X, Y float64 }
type Cell struct{ X, Y int }
type Grid struct{ Floor, Obstacles map[Cell]bool }

// ObservedClearFloor returns measured cells only. Call on the original scan,
// never the departure grid that may contain an operator-authorized allowance.
func (g Grid) ObservedClearFloor() []Point {
	var points []Point
	for c, observed := range g.Floor {
		if observed && !g.Obstacles[c] {
			points = append(points, center(c))
		}
	}
	return points
}

type Departure struct {
	Position Point
	Yaw      float64
}

// A supervised, fixed departure footprint, never a moving bubble of free space.
// Missing floor is allowed only in cells touched by the initial body envelope
// and its clearance buffer. Observed obstacles and coherent drops stay blocked.
func (g Grid) WithDeparture(d *Departure) Grid {
	if d == nil || !d.Position.Valid() || !finite(d.Yaw) {
		return g
	}
	floor := make(map[Cell]bool, len(g.Floor)+100)
	for c, v := range g.Floor {
		floor[c] = v
	}
	g.sweptCells(d.Position, d.Position, d.Yaw, 0, func(c Cell) bool { floor[c] = true; return true })
	return Grid{floor, g.Obstacles}
}

// WithUnobservedFloor is an explicit supervised traversal policy. It admits
// missing floor within the bounded planning area, but never removes occupied
// cells or measured drops. It must not be used as localization evidence.
func (g Grid) WithUnobservedFloor(origin Point) Grid {
	if !origin.Valid() {
		return g
	}
	floor := make(map[Cell]bool, len(g.Floor))
	for c, v := range g.Floor {
		floor[c] = v
	}
	radius := MaxDistance + 1 // footprint and stopping margin beyond the goal
	lo, hi := cell(Point{origin.X - radius, origin.Y - radius}), cell(Point{origin.X + radius, origin.Y + radius})
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			floor[Cell{x, y}] = true
		}
	}
	return Grid{floor, g.Obstacles}
}

func (g Grid) ClearanceError(p Point, subject string) error {
	return g.PoseClearanceError(p, 0, subject)
}

func (g Grid) PoseClearanceError(p Point, yaw float64, subject string) error {
	if !p.Valid() || !finite(yaw) {
		return errors.New("Invalid map pose.")
	}
	missing, blocked := false, false
	g.sweptCells(p, p, yaw, 0, func(c Cell) bool {
		blocked = blocked || g.Obstacles[c]
		missing = missing || !g.Floor[c]
		return true
	})
	if blocked {
		return errors.New("An obstacle or drop overlaps Go2's footprint or its 10 cm clearance at " + subject + ".")
	}
	if missing {
		return errors.New("LiDAR has not observed all the floor beneath Go2's footprint and its 10 cm clearance at " + subject + ".")
	}
	return nil
}

// sweptCells tests whole occupied cells against the exact convex sweep of an
// oriented rectangle. Separating axes include both rectangle and grid axes,
// plus the sweep edge. Checking only cell centers misses thin corner overlaps.
func (g Grid) sweptCells(a, b Point, yaw, padding float64, visit func(Cell) bool) bool {
	if !a.Valid() || !b.Valid() || !finite(yaw) {
		return false
	}
	s, c := math.Sincos(yaw)
	l, w := HalfLength+Clearance+padding, HalfWidth+Clearance+padding
	rx, ry := math.Abs(c)*l+math.Abs(s)*w, math.Abs(s)*l+math.Abs(c)*w
	lo := cell(Point{math.Min(a.X, b.X) - rx, math.Min(a.Y, b.Y) - ry})
	hi := cell(Point{math.Max(a.X, b.X) + rx, math.Max(a.Y, b.Y) + ry})
	axes := []Point{{1, 0}, {0, 1}, {c, s}, {-s, c}}
	if d := a.Distance(b); d > 1e-9 {
		axes = append(axes, Point{-(b.Y - a.Y) / d, (b.X - a.X) / d})
	}
	type projection struct {
		axis   Point
		lo, hi float64
	}
	spans := make([]projection, 0, len(axes))
	for _, axis := range axes {
		radius := l*math.Abs(axis.X*c+axis.Y*s) + w*math.Abs(-axis.X*s+axis.Y*c)
		radius += CellSize / 2 * (math.Abs(axis.X) + math.Abs(axis.Y))
		pa, pb := a.X*axis.X+a.Y*axis.Y, b.X*axis.X+b.Y*axis.Y
		spans = append(spans, projection{axis, math.Min(pa, pb) - radius, math.Max(pa, pb) + radius})
	}
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			q := Cell{x, y}
			p := center(q)
			overlap := true
			for _, span := range spans {
				v := p.X*span.axis.X + p.Y*span.axis.Y
				if v <= span.lo+1e-9 || v >= span.hi-1e-9 {
					overlap = false
					break
				}
			}
			if overlap && !visit(q) {
				return false
			}
		}
	}
	return true
}

func (g Grid) ClearPose(p Point, yaw float64) bool {
	return g.Sweep(p, p, yaw)
}

func (g Grid) Sweep(a, b Point, yaw float64) bool {
	return g.sweptCells(a, b, yaw, 0, func(c Cell) bool { return g.Floor[c] && !g.Obstacles[c] })
}

// Turn checks the shortest in-place turn used by the controller. Padding bounds
// every corner's travel between angular samples, so intermediate poses cannot
// slip between the checks. Translation and rotation are commanded separately.
func (g Grid) Turn(p Point, from, to float64) bool {
	return g.turnCells(p, from, to, func(c Cell) bool { return g.Floor[c] && !g.Obstacles[c] })
}

func (g Grid) turnCells(p Point, from, to float64, visit func(Cell) bool) bool {
	if !finite(from) || !finite(to) {
		return false
	}
	delta := math.Remainder(to-from, 2*math.Pi)
	n := max(1, int(math.Ceil(math.Abs(delta)/(math.Pi/90))))
	padding := math.Hypot(HalfLength+Clearance, HalfWidth+Clearance) * math.Abs(delta) / float64(n) / 2
	for i := 0; i <= n; i++ {
		yaw := from + delta*float64(i)/float64(n)
		if !g.sweptCells(p, p, yaw, padding, visit) {
			return false
		}
	}
	return true
}

func Heading(a, b Point) float64 { return math.Atan2(b.Y-a.Y, b.X-a.X) }

func (g Grid) Transition(a Point, yaw float64, b Point) bool {
	if a.Distance(b) < 1e-9 {
		return g.ClearPose(a, yaw)
	}
	heading := Heading(a, b)
	return g.Turn(a, yaw, heading) && g.Sweep(a, b, heading)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func (p Point) Valid() bool {
	return finite(p.X) && finite(p.Y) && math.Abs(p.X) < 10000 && math.Abs(p.Y) < 10000
}
func (p Point) Distance(q Point) float64 { return math.Hypot(p.X-q.X, p.Y-q.Y) }
func cell(p Point) Cell {
	return Cell{int(math.Floor(p.X / CellSize)), int(math.Floor(p.Y / CellSize))}
}
func center(c Cell) Point {
	return Point{(float64(c.X) + 0.5) * CellSize, (float64(c.Y) + 0.5) * CellSize}
}

func (g Grid) Clear(p Point) bool      { return g.ClearPose(p, 0) }
func (g Grid) Segment(a, b Point) bool { return g.Sweep(a, b, Heading(a, b)) }

type routeState struct {
	cell    Cell
	heading int
}
type entry struct {
	state routeState
	score float64
}
type frontier []entry

func (h frontier) Len() int           { return len(h) }
func (h frontier) Less(i, j int) bool { return h[i].score < h[j].score }
func (h frontier) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *frontier) Push(v any)        { *h = append(*h, v.(entry)) }
func (h *frontier) Pop() any          { v := (*h)[len(*h)-1]; *h = (*h)[:len(*h)-1]; return v }

func (g Grid) Plan(start, goal Point) ([]Point, error) { return g.PlanPose(start, 0, goal) }

// Heading belongs to the search state: reaching a cell lengthways does not
// imply that Go2 can turn sideways there. The root keeps the actual unsnapped
// pose, and the final edge reaches the exact requested destination.
func (g Grid) PlanPose(start Point, yaw float64, goal Point) ([]Point, error) {
	deadline := time.Now().Add(2 * time.Second)
	path, err := g.planPose(start, yaw, goal, deadline)
	if err == nil || !start.Valid() || !goal.Valid() || !finite(yaw) || start.Distance(goal) > MaxDistance ||
		!g.ClearPose(start, yaw) || !g.Floor[cell(goal)] || g.Obstacles[cell(goal)] {
		return path, err
	}
	// The eight map headings can all require an impossible initial turn even
	// when the robot can leave a narrow space along its actual heading. Try
	// bounded straight prefixes before joining the same heading-aware search.
	// Keep the original grid/allowance and a single deadline for every attempt.
	for step := 1; step <= 8 && time.Now().Before(deadline); step++ {
		distance := float64(step) * CellSize
		exit := Point{start.X + distance*math.Cos(yaw), start.Y + distance*math.Sin(yaw)}
		if !g.Sweep(start, exit, yaw) {
			break
		}
		tail, tailErr := g.planPose(exit, yaw, goal, deadline)
		if tailErr != nil {
			continue
		}
		length := distance
		for i := 1; i < len(tail); i++ {
			length += tail[i-1].Distance(tail[i])
		}
		if length <= 12 {
			return append([]Point{start}, tail...), nil
		}
	}
	return nil, err
}

func (g Grid) planPose(start Point, yaw float64, goal Point, deadline time.Time) ([]Point, error) {
	if !start.Valid() || !goal.Valid() || !finite(yaw) || start.Distance(goal) > MaxDistance {
		return nil, errors.New("Choose a destination within 8 metres of Go2.")
	}
	if err := g.PoseClearanceError(start, yaw, "Go2"); err != nil {
		return nil, err
	}
	if g.Transition(start, yaw, goal) {
		return []Point{start, goal}, nil
	}
	if !g.Floor[cell(goal)] || g.Obstacles[cell(goal)] {
		return nil, errors.New("The destination needs observed clear floor for Go2's footprint and its 10 cm clearance.")
	}
	root := routeState{cell(start), 8}
	point := func(s routeState) Point {
		if s == root {
			return start
		}
		return Point{start.X + float64(s.cell.X-root.cell.X)*CellSize, start.Y + float64(s.cell.Y-root.cell.Y)*CellSize}
	}
	angle := func(s routeState) float64 {
		if s == root {
			return yaw
		}
		return float64(s.heading) * math.Pi / 4
	}
	open := &frontier{{root, 0}}
	heap.Init(open)
	cost := map[routeState]float64{root: 0}
	parents := map[routeState]routeState{}
	closed := map[routeState]bool{}
	directions := []Cell{{1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}, {0, -1}, {1, -1}}
	// Every node shares the start's sub-cell offset. Cache the swept cells
	// for each arrival/departure heading pair; translating that exact mask is
	// equivalent to repeating all geometric checks at every search node.
	masks := map[[2]int][]Cell{}
	transition := func(at routeState, to int, d Cell) bool {
		key := [2]int{at.heading, to}
		mask, ok := masks[key]
		if !ok {
			cells := map[Cell]bool{}
			collect := func(c Cell) bool { cells[Cell{c.X - root.cell.X, c.Y - root.cell.Y}] = true; return true }
			targetYaw := float64(to) * math.Pi / 4
			g.turnCells(start, angle(at), targetYaw, collect)
			end := Point{start.X + float64(d.X)*CellSize, start.Y + float64(d.Y)*CellSize}
			g.sweptCells(start, end, targetYaw, 0, collect)
			for c := range cells {
				mask = append(mask, c)
			}
			sort.Slice(mask, func(i, j int) bool {
				if mask[i].Y != mask[j].Y {
					return mask[i].Y < mask[j].Y
				}
				return mask[i].X < mask[j].X
			})
			masks[key] = mask
		}
		for _, offset := range mask {
			c := Cell{at.cell.X + offset.X, at.cell.Y + offset.Y}
			if !g.Floor[c] || g.Obstacles[c] {
				return false
			}
		}
		return true
	}
	for open.Len() > 0 && len(closed) < 30000 {
		if len(closed)%128 == 0 && time.Now().After(deadline) {
			break
		}
		at := heap.Pop(open).(entry).state
		if closed[at] {
			continue
		}
		closed[at] = true
		p, heading := point(at), angle(at)
		if p.Distance(goal) <= CellSize*1.5 && g.Transition(p, heading, goal) {
			path := []Point{goal}
			for at != root {
				path = append(path, point(at))
				at = parents[at]
			}
			path = append(path, start)
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			// Only remove collinear points. Arbitrary shortcuts can introduce a turn
			// whose corner sweep was not checked during the search.
			out := []Point{start}
			for i := 1; i < len(path)-1; i++ {
				before, after := Heading(out[len(out)-1], path[i]), Heading(path[i], path[i+1])
				if path[i].Distance(path[i+1]) < 1e-9 || math.Abs(math.Remainder(after-before, 2*math.Pi)) < 1e-8 {
					continue
				}
				out = append(out, path[i])
			}
			out = append(out, goal)
			length := 0.0
			for i := 1; i < len(out); i++ {
				length += out[i-1].Distance(out[i])
			}
			if length > 12 {
				return nil, errors.New("This route is longer than 12 metres. Choose a closer destination.")
			}
			return out, nil
		}
		for h, d := range directions {
			next := routeState{Cell{at.cell.X + d.X, at.cell.Y + d.Y}, h}
			q := point(next)
			if closed[next] || start.Distance(q) > MaxDistance+1 {
				continue
			}
			nextCost := cost[at] + p.Distance(q)
			if old, ok := cost[next]; ok && old <= nextCost {
				continue
			}
			if !transition(at, h, d) {
				continue
			}
			cost[next] = nextCost
			parents[next] = at
			heap.Push(open, entry{next, nextCost + q.Distance(goal)})
		}
	}
	return nil, errors.New("No route with room for Go2's footprint, 10 cm clearance and turns through observed floor.")
}

// Alignment converts robot odometry XY into the site's right-handed Y-up
// coordinates, represented here as (site X, -site Z).
type Alignment struct {
	Origin, Site Point
	Rotation     float64
}

func (a Alignment) ToSite(p Point) Point {
	c, s := math.Cos(a.Rotation), math.Sin(a.Rotation)
	x, y := p.X-a.Origin.X, p.Y-a.Origin.Y
	return Point{a.Site.X + c*x - s*y, a.Site.Y + s*x + c*y}
}
func (a Alignment) ToRobot(p Point) Point {
	c, s := math.Cos(a.Rotation), math.Sin(a.Rotation)
	x, y := p.X-a.Site.X, p.Y-a.Site.Y
	return Point{a.Origin.X + c*x + s*y, a.Origin.Y - s*x + c*y}
}

// The physical Go2 ignores forward requests below roughly 0.5 m/s. Use its
// walking rate until stopping; a lower proportional command is not a slow gait.
const WalkingSpeed = .55

func Velocity(position Point, yaw float64, target Point) (forward, turn float64) {
	error := math.Remainder(math.Atan2(target.Y-position.Y, target.X-position.X)-yaw, 2*math.Pi)
	turn = math.Max(-0.6, math.Min(0.6, error*1.5))
	if math.Abs(error) < 0.04 && position.Distance(target) > 0.04 {
		forward = WalkingSpeed
		turn = 0
	}
	return
}
