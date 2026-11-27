package collabdemo

import (
	"container/heap"
	"errors"
	"math"
)

// PlanRoute runs the office planner without assignment, authorization or a
// coordinator session. Coordinates are MuJoCo/grid XY; callers own execution.
func PlanRoute(grid Grid, start, goal Vec, reservations [][]Vec) ([]Vec, error) {
	if !validVec(grid.Origin) || !finite(grid.Resolution) || grid.Resolution <= 0 || grid.Resolution > 2 || grid.Width < 1 || grid.Height < 1 || grid.Width > 512 || grid.Height > 512 || grid.Width*grid.Height > 100000 || len(grid.Blocked) > grid.Width*grid.Height || len(reservations) > 16 {
		return nil, errors.New("invalid bounded route grid")
	}
	c := &Coordinator{scene: Scene{Grid: grid}, blocked: make(map[Cell]bool, len(grid.Blocked))}
	for _, cell := range grid.Blocked {
		if !grid.inside(cell) {
			return nil, errors.New("route obstacle outside grid")
		}
		c.blocked[cell] = true
	}
	for _, path := range reservations {
		if len(path) > 4096 {
			return nil, errors.New("route reservation exceeds bound")
		}
	}
	path := c.plan(start, goal, reservations)
	if len(path) == 0 {
		return nil, errors.New("no clear office route")
	}
	return path, nil
}

const Separation = 0.65
const MaxSpeed = 0.55

func distance(a, b Vec) float64 { return math.Hypot(a[0]-b[0], a[1]-b[1]) }
func finite(v float64) bool     { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func validVec(v Vec) bool       { return finite(v[0]) && finite(v[1]) }
func (g Grid) cell(v Vec) Cell {
	return Cell{int(math.Floor((v[0] - g.Origin[0]) / g.Resolution)), int(math.Floor((v[1] - g.Origin[1]) / g.Resolution))}
}
func (g Grid) center(c Cell) Vec {
	return Vec{g.Origin[0] + (float64(c[0])+.5)*g.Resolution, g.Origin[1] + (float64(c[1])+.5)*g.Resolution}
}
func (g Grid) inside(c Cell) bool { return c[0] >= 0 && c[1] >= 0 && c[0] < g.Width && c[1] < g.Height }
func segmentDistance(p, a, b Vec) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	den := dx*dx + dy*dy
	if den == 0 {
		return distance(p, a)
	}
	t := math.Max(0, math.Min(1, ((p[0]-a[0])*dx+(p[1]-a[1])*dy)/den))
	return distance(p, Vec{a[0] + t*dx, a[1] + t*dy})
}
func nearPath(p Vec, path []Vec, margin float64) bool {
	for i, v := range path {
		if distance(p, v) < margin {
			return true
		}
		if i > 0 && segmentDistance(p, path[i-1], v) < margin {
			return true
		}
	}
	return false
}

// segmentsDistance includes interior crossings, unlike endpoint-only checks.
func segmentsDistance(a, b, c, d Vec) float64 {
	rx, ry, sx, sy := b[0]-a[0], b[1]-a[1], d[0]-c[0], d[1]-c[1]
	denominator := rx*sy - ry*sx
	if denominator != 0 {
		qx, qy := c[0]-a[0], c[1]-a[1]
		t, u := (qx*sy-qy*sx)/denominator, (qx*ry-qy*rx)/denominator
		if t >= 0 && t <= 1 && u >= 0 && u <= 1 {
			return 0
		}
	}
	return math.Min(math.Min(segmentDistance(a, c, d), segmentDistance(b, c, d)),
		math.Min(segmentDistance(c, a, b), segmentDistance(d, a, b)))
}

func segmentNearPath(a, b Vec, path []Vec, margin float64) bool {
	for i, point := range path {
		if segmentDistance(point, a, b) < margin {
			return true
		}
		if i > 0 && segmentsDistance(a, b, path[i-1], point) < margin {
			return true
		}
	}
	return false
}

// clearSegment checks the supercover: every closed grid cell touched by the
// segment, including both sides of a boundary and all four cells at a corner.
// It does not sample at intervals or assume that clear endpoints imply a clear
// interior. Out-of-grid cells must be rejected by allowed. The small tolerance
// only includes extra near-boundary cells; it never skips an obstruction.
func (g Grid) clearSegment(a, b Vec, allowed func(Cell) bool) bool {
	if !validVec(a) || !validVec(b) {
		return false
	}
	start := Vec{(a[0] - g.Origin[0]) / g.Resolution, (a[1] - g.Origin[1]) / g.Resolution}
	end := Vec{(b[0] - g.Origin[0]) / g.Resolution, (b[1] - g.Origin[1]) / g.Resolution}
	if !validVec(start) || !validVec(end) || start[0] < 0 || start[1] < 0 || end[0] < 0 || end[1] < 0 ||
		start[0] >= float64(g.Width) || end[0] >= float64(g.Width) || start[1] >= float64(g.Height) || end[1] >= float64(g.Height) {
		return false
	}
	const tolerance = 1e-10
	boundary := func(v float64) bool { return math.Abs(v-math.Round(v)) <= tolerance }
	pointClear := func(p Vec) bool {
		xs, ys := []int{int(math.Floor(p[0]))}, []int{int(math.Floor(p[1]))}
		if boundary(p[0]) {
			xs = []int{int(math.Round(p[0])) - 1, int(math.Round(p[0]))}
		}
		if boundary(p[1]) {
			ys = []int{int(math.Round(p[1])) - 1, int(math.Round(p[1]))}
		}
		for _, x := range xs {
			for _, y := range ys {
				if !allowed(Cell{x, y}) {
					return false
				}
			}
		}
		return true
	}
	if !pointClear(start) || !pointClear(end) {
		return false
	}
	dx, dy := end[0]-start[0], end[1]-start[1]
	cell, goal := Cell{int(math.Floor(start[0])), int(math.Floor(start[1]))}, Cell{int(math.Floor(end[0])), int(math.Floor(end[1]))}
	// A line lying on a grid boundary touches the cells on both sides.
	verticalBoundary, horizontalBoundary := dx == 0 && boundary(start[0]), dy == 0 && boundary(start[1])
	visit := func(c Cell) bool {
		if !allowed(c) {
			return false
		}
		if verticalBoundary && !allowed(Cell{int(math.Round(start[0])) - 1, c[1]}) {
			return false
		}
		if horizontalBoundary && !allowed(Cell{c[0], int(math.Round(start[1])) - 1}) {
			return false
		}
		return true
	}
	stepX, stepY := 1, 1
	maxX, maxY, deltaX, deltaY := math.Inf(1), math.Inf(1), math.Inf(1), math.Inf(1)
	if dx > 0 {
		maxX, deltaX = (float64(cell[0]+1)-start[0])/dx, 1/dx
	} else if dx < 0 {
		stepX, maxX, deltaX = -1, (start[0]-float64(cell[0]))/-dx, -1/dx
	}
	if dy > 0 {
		maxY, deltaY = (float64(cell[1]+1)-start[1])/dy, 1/dy
	} else if dy < 0 {
		stepY, maxY, deltaY = -1, (start[1]-float64(cell[1]))/-dy, -1/dy
	}
	for count := 0; count <= g.Width+g.Height+2; count++ {
		if !visit(cell) {
			return false
		}
		// An endpoint on a grid corner can belong to a different floor cell
		// from the last open segment when an axis travels backwards. Its
		// incident cells were all checked by pointClear above.
		if cell == goal || math.Min(maxX, maxY) >= 1 {
			return true
		}
		if math.Abs(maxX-maxY) <= tolerance {
			if !visit(Cell{cell[0] + stepX, cell[1]}) || !visit(Cell{cell[0], cell[1] + stepY}) {
				return false
			}
			cell[0], cell[1] = cell[0]+stepX, cell[1]+stepY
			maxX, maxY = maxX+deltaX, maxY+deltaY
		} else if maxX < maxY {
			cell[0], maxX = cell[0]+stepX, maxX+deltaX
		} else {
			cell[1], maxY = cell[1]+stepY, maxY+deltaY
		}
	}
	return false
}

// pullPath extends each segment while it stays visible, then keeps the last
// clear waypoint. At most twice as many visibility checks as input points are
// needed. It preserves the original endpoints and never invents a waypoint.
func pullPath(path []Vec, clear func(Vec, Vec) bool) []Vec {
	if len(path) < 2 {
		return nil
	}
	result := []Vec{path[0]}
	anchor, last := 0, 0
	for next := 1; next < len(path); next++ {
		if clear(path[anchor], path[next]) {
			last = next
			continue
		}
		if last == anchor {
			return nil
		}
		result = append(result, path[last])
		anchor = last
		if !clear(path[anchor], path[next]) {
			return nil
		}
		last = next
	}
	if result[len(result)-1] != path[len(path)-1] {
		result = append(result, path[len(path)-1])
	}
	return result
}

type candidate struct {
	cell           Cell
	cost, priority float64
	order          int
}
type queue []candidate

func (q queue) Len() int { return len(q) }
func (q queue) Less(i, j int) bool {
	if q[i].priority == q[j].priority {
		return q[i].order < q[j].order
	}
	return q[i].priority < q[j].priority
}
func (q queue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *queue) Push(x any)   { *q = append(*q, x.(candidate)) }
func (q *queue) Pop() any     { old := *q; x := old[len(old)-1]; *q = old[:len(old)-1]; return x }

// plan uses eight-neighbor A* followed by conservative line-of-sight pulling.
// Static blocked cells already include the physical robot margin. Other routes
// retain their whole-path reservation and half-cell-diagonal allowance.
func (c *Coordinator) plan(start, goal Vec, reservations [][]Vec) []Vec {
	g := c.scene.Grid
	if !validVec(start) || !validVec(goal) {
		return nil
	}
	for _, path := range reservations {
		for _, p := range path {
			if !validVec(p) {
				return nil
			}
		}
	}
	a, b := g.cell(start), g.cell(goal)
	if !g.inside(a) || !g.inside(b) || c.blocked[a] || c.blocked[b] {
		return nil
	}
	margin := Separation + g.Resolution*math.Sqrt2/2
	known := map[Cell]bool{}
	allowed := func(cell Cell) bool {
		if value, found := known[cell]; found {
			return value
		}
		if !g.inside(cell) || c.blocked[cell] {
			known[cell] = false
			return false
		}
		for _, p := range reservations {
			if nearPath(g.center(cell), p, margin) {
				known[cell] = false
				return false
			}
		}
		known[cell] = true
		return true
	}
	if !allowed(a) || !allowed(b) {
		return nil
	}
	for _, p := range reservations {
		if nearPath(start, p, Separation) || nearPath(goal, p, Separation) {
			return nil
		}
	}
	clear := func(a, b Vec) bool {
		if !g.clearSegment(a, b, allowed) {
			return false
		}
		for _, path := range reservations {
			if segmentNearPath(a, b, path, Separation) {
				return false
			}
		}
		return true
	}
	if !clear(start, g.center(a)) || !clear(g.center(b), goal) {
		return nil
	}
	front := &queue{{cell: a, priority: distance(g.center(a), g.center(b))}}
	heap.Init(front)
	cost := map[Cell]float64{a: 0}
	parent := map[Cell]Cell{}
	order := 0
	for front.Len() > 0 {
		n := heap.Pop(front).(candidate)
		if best := cost[n.cell]; n.cost > best {
			continue
		}
		if n.cell == b {
			cells := []Cell{b}
			for cells[len(cells)-1] != a {
				cells = append(cells, parent[cells[len(cells)-1]])
			}
			path := []Vec{start}
			for i := len(cells) - 1; i >= 0; i-- {
				path = append(path, g.center(cells[i]))
			}
			path = append(path, goal)
			return pullPath(path, clear)
		}
		for _, d := range []Cell{{1, 0}, {0, 1}, {-1, 0}, {0, -1}, {1, 1}, {-1, 1}, {-1, -1}, {1, -1}} {
			next := Cell{n.cell[0] + d[0], n.cell[1] + d[1]}
			if !allowed(next) || !clear(g.center(n.cell), g.center(next)) {
				continue
			}
			nc := n.cost + g.Resolution*math.Hypot(float64(d[0]), float64(d[1]))
			old, found := cost[next]
			if found && old <= nc {
				continue
			}
			cost[next] = nc
			parent[next] = n.cell
			order++
			heap.Push(front, candidate{cell: next, cost: nc, priority: nc + distance(g.center(next), g.center(b)), order: order})
		}
	}
	return nil
}
