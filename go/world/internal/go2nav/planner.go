// Package go2nav plans only through scan-observed planar space in an explicitly
// commissioned synthetic flat-floor area. It does not observe terrain or grant
// motion authority. Swept rectangle and heading-aware search algorithms are
// adapted from Wendy's Apache-2.0 sitenav; see docs/third-party-go2-navigation.md.
package go2nav

import (
	"container/heap"
	"errors"
	"math"
)

const CellSize = .10

// ROS LaserScan stores angular metadata as float32. At pi, the round-to-nearest
// error is below 1.2e-7 rad; a full turn assembled from a rounded increment has
// error below 3.8e-7 rad. This bound admits that representation without changing
// any measured angle or permitting a missing ray interval.
const angularQuantizationTolerance = 1e-6

var (
	ErrInvalid = errors.New("invalid planar navigation input")
	ErrUnknown = errors.New("planar clearance is unobserved")
	ErrBlocked = errors.New("planar swept footprint is blocked")
	ErrBounds  = errors.New("planar footprint or goal exceeds commissioned bounds")
	ErrNoRoute = errors.New("no bounded observed planar route")
)

type Point struct{ X, Y float64 }
type Pose struct{ X, Y, Yaw float64 }
type Bounds struct{ Min, Max Point }
type Scan struct {
	TiltedPose                                   *TiltedPose
	Sequence                                     uint64
	CapturedUnixNS                               int64
	WorldEpoch                                   uint64
	Frame                                        string
	Ranges                                       []float64
	AngleMin, AngleIncrement, RangeMin, RangeMax float64
	Robot, SensorMount                           Pose
	MountKnown                                   bool
	PositionUncertaintyM, YawUncertaintyRad      float64
	UncertaintyKnown                             bool
}

const (
	IsotropicV1     = "isotropic-v1"
	IsotropicGaitV1 = "isotropic-gait-v1"
	DirectionalV1   = "directional-v1"
)

type Profile struct {
	TiltedScan                                    *TiltedScanProfile
	MaxAbsRollRad, MaxAbsPitchRad                 float64
	MotionEnvelope                                string
	FlatFloor                                     bool
	HalfLengthM, HalfWidthM, ClearanceM           float64
	MaxPositionUncertaintyM, MaxYawUncertaintyRad float64
}
type MotionLimits struct {
	// Required only for isotropic-gait-v1; legacy models retain their .05 cap.
	MaxMeasuredLateralMPS                                                                float64
	MaxForwardMPS, MaxYawRateRPS, HorizonS, ExtraStopM, ExtraStopRad, WaypointToleranceM float64
}
type Command struct{ ForwardMPS, YawRateRPS float64 }
type cell struct{ X, Y int }
type occupancy uint8

const (
	unknown occupancy = iota
	free
	occupied
)

type Grid struct {
	motionEnvelope        string
	initialYawUncertainty float64
	area                  Bounds
	cells                 map[cell]occupancy
	halfLength, halfWidth float64
	sequence, epoch       uint64
	captured              int64
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func validPoint(p Point) bool {
	return finite(p.X) && finite(p.Y) && math.Abs(p.X) <= 100 && math.Abs(p.Y) <= 100
}
func validPose(p Pose) bool {
	return validPoint(Point{p.X, p.Y}) && finite(p.Yaw) && math.Abs(p.Yaw) <= math.Pi
}
func point(p Pose) Point          { return Point{p.X, p.Y} }
func normalize(a float64) float64 { return math.Atan2(math.Sin(a), math.Cos(a)) }
func at(p Point) cell             { return cell{int(math.Floor(p.X / CellSize)), int(math.Floor(p.Y / CellSize))} }
func center(c cell) Point {
	return Point{(float64(c.X) + .5) * CellSize, (float64(c.Y) + .5) * CellSize}
}
func distance(a, b Point) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }
func heading(a, b Point) float64  { return math.Atan2(b.Y-a.Y, b.X-a.X) }
func (b Bounds) contains(p Point) bool {
	return p.X >= b.Min.X && p.Y >= b.Min.Y && p.X <= b.Max.X && p.Y <= b.Max.Y
}
func (b Bounds) valid() bool {
	return validPoint(b.Min) && validPoint(b.Max) && b.Max.X > b.Min.X && b.Max.Y > b.Min.Y && b.Max.X-b.Min.X <= 8 && b.Max.Y-b.Min.Y <= 8
}

// Build copies no range slice into the resulting immutable grid. The caller
// verifies current capture age, clock agreement, odometry pairing and lifecycle
// identity. Positive source stamps alone do not establish freshness here.
func Build(s Scan, area Bounds, p Profile) (*Grid, error) {
	if (p.MotionEnvelope != IsotropicV1 && p.MotionEnvelope != DirectionalV1 && p.MotionEnvelope != IsotropicGaitV1) || !area.valid() || !p.FlatFloor || !s.MountKnown || !s.UncertaintyKnown || s.Sequence == 0 || s.WorldEpoch == 0 || s.CapturedUnixNS <= 0 || s.Frame != "odom" || !validPose(s.Robot) || !validPose(s.SensorMount) || !area.contains(point(s.Robot)) || math.Hypot(s.SensorMount.X, s.SensorMount.Y) > .6 {
		return nil, ErrInvalid
	}
	for _, v := range []float64{p.HalfLengthM, p.HalfWidthM, p.ClearanceM, p.MaxPositionUncertaintyM, p.MaxYawUncertaintyRad, s.PositionUncertaintyM, s.YawUncertaintyRad, s.AngleMin, s.AngleIncrement, s.RangeMin, s.RangeMax} {
		if !finite(v) {
			return nil, ErrInvalid
		}
	}
	if p.HalfLengthM < .425 || p.HalfLengthM > .6 || p.HalfWidthM < .30 || p.HalfWidthM > .4 || p.ClearanceM < .02 || p.ClearanceM > .3 || p.MaxPositionUncertaintyM <= 0 || p.MaxPositionUncertaintyM > .5 || p.MaxYawUncertaintyRad <= 0 || p.MaxYawUncertaintyRad > .5 || s.PositionUncertaintyM < 0 || s.PositionUncertaintyM > p.MaxPositionUncertaintyM || s.YawUncertaintyRad < 0 || s.YawUncertaintyRad > p.MaxYawUncertaintyRad || len(s.Ranges) < 180 || len(s.Ranges) > 8192 || s.AngleIncrement <= 0 || s.AngleIncrement > math.Pi/90+1e-9 || math.Abs(float64(len(s.Ranges))*s.AngleIncrement-2*math.Pi) > angularQuantizationTolerance || math.Abs(s.AngleMin) > math.Pi+angularQuantizationTolerance || s.RangeMin <= 0 || s.RangeMax <= s.RangeMin || s.RangeMax > 12 {
		return nil, ErrInvalid
	}
	for _, r := range s.Ranges {
		if math.IsInf(r, 1) {
			continue
		}
		if !finite(r) || r < s.RangeMin || r > s.RangeMax {
			return nil, ErrInvalid
		}
	}
	if p.TiltedScan != nil {
		return buildTilted(s, area, p)
	}
	if s.TiltedPose != nil {
		return nil, ErrInvalid
	}
	// Pose uncertainty expands the body. Angular uncertainty also displaces the
	// footprint corners and the translated sensor origin. These are explicit
	// supplied bounds, not a statistical confidence inferred from covariance.
	angular := (math.Hypot(p.HalfLengthM, p.HalfWidthM) + math.Hypot(s.SensorMount.X, s.SensorMount.Y)) * 2 * math.Sin(s.YawUncertaintyRad/2)
	padding := p.ClearanceM + s.PositionUncertaintyM + angular
	g := &Grid{motionEnvelope: p.MotionEnvelope, initialYawUncertainty: s.YawUncertaintyRad, area: area, cells: make(map[cell]occupancy), halfLength: p.HalfLengthM + padding, halfWidth: p.HalfWidthM + padding, sequence: s.Sequence, epoch: s.WorldEpoch, captured: s.CapturedUnixNS}
	sn, cs := math.Sincos(s.Robot.Yaw)
	origin := Point{s.Robot.X + cs*s.SensorMount.X - sn*s.SensorMount.Y, s.Robot.Y + sn*s.SensorMount.X + cs*s.SensorMount.Y}
	yaw := s.Robot.Yaw + s.SensorMount.Yaw
	lo, hi := at(area.Min), at(area.Max)
	if (hi.X-lo.X+1)*(hi.Y-lo.Y+1) > 81*81 {
		return nil, ErrInvalid
	}
	// A cell needs finite returns across its entire angular extent, including
	// both bracketing rays. No max-range point is invented for a missing return.
	// This is a finite-resolution planar observation, not proof that an object
	// narrower than the ray spacing cannot exist between beams.
	// The cell's support also covers the supplied uncertainty of the scan
	// origin/orientation. Body padding alone would not bound an angular error
	// at distant obstacle returns. The current body uncertainty is separately
	// included in the swept footprint above.
	radius := CellSize/math.Sqrt2 + s.PositionUncertaintyM
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			c := cell{x, y}
			q := center(c)
			if !area.contains(Point{q.X - CellSize/2, q.Y - CellSize/2}) || !area.contains(Point{q.X + CellSize/2, q.Y + CellSize/2}) {
				continue
			}
			d := distance(q, origin)
			required := d + radius + .02
			half := math.Pi
			if d > radius {
				half = math.Min(math.Pi, math.Asin(radius/d)+s.YawUncertaintyRad)
			}
			angle := normalize(math.Atan2(q.Y-origin.Y, q.X-origin.X) - yaw - s.AngleMin)
			if angle < 0 {
				angle += 2 * math.Pi
			}
			first, last := int(math.Floor((angle-half)/s.AngleIncrement)), int(math.Ceil((angle+half)/s.AngleIncrement))
			clear := true
			for i := first; i <= last; i++ {
				j := i % len(s.Ranges)
				if j < 0 {
					j += len(s.Ranges)
				}
				if !finite(s.Ranges[j]) || s.Ranges[j] <= required {
					clear = false
					break
				}
			}
			if clear {
				g.cells[c] = free
			}
		}
	}
	for i, r := range s.Ranges {
		if finite(r) {
			a := yaw + s.AngleMin + float64(i)*s.AngleIncrement
			q := Point{origin.X + r*math.Cos(a), origin.Y + r*math.Sin(a)}
			if area.contains(q) {
				g.cells[at(q)] = occupied
			}
		}
	}
	return g, nil
}

func (g *Grid) cellError(c cell) error {
	switch g.cells[c] {
	case free:
		return nil
	case occupied:
		return ErrBlocked
	default:
		return ErrUnknown
	}
}

// sweepError tests full cell rectangles against the exact convex translation of
// the padded oriented footprint, using separating axes. Cell-center tests alone
// miss corner collisions. Padding covers intermediate orientations in Turn.
func (g *Grid) sweepError(a, b Point, yaw, padding float64) error {
	if g == nil || !validPoint(a) || !validPoint(b) || !finite(yaw) || !finite(padding) || padding < 0 {
		return ErrInvalid
	}
	s, c := math.Sincos(yaw)
	l, w := g.halfLength+padding, g.halfWidth+padding
	rx, ry := math.Abs(c)*l+math.Abs(s)*w, math.Abs(s)*l+math.Abs(c)*w
	min := Point{math.Min(a.X, b.X) - rx, math.Min(a.Y, b.Y) - ry}
	max := Point{math.Max(a.X, b.X) + rx, math.Max(a.Y, b.Y) + ry}
	if !g.area.contains(min) || !g.area.contains(max) {
		return ErrBounds
	}
	axes := []Point{{1, 0}, {0, 1}, {c, s}, {-s, c}}
	if d := distance(a, b); d > 1e-9 {
		axes = append(axes, Point{-(b.Y - a.Y) / d, (b.X - a.X) / d})
	}
	type span struct {
		axis   Point
		lo, hi float64
	}
	spans := make([]span, 0, len(axes))
	for _, axis := range axes {
		r := l*math.Abs(axis.X*c+axis.Y*s) + w*math.Abs(-axis.X*s+axis.Y*c) + CellSize/2*(math.Abs(axis.X)+math.Abs(axis.Y))
		pa, pb := a.X*axis.X+a.Y*axis.Y, b.X*axis.X+b.Y*axis.Y
		spans = append(spans, span{axis, math.Min(pa, pb) - r, math.Max(pa, pb) + r})
	}
	lo, hi := at(min), at(max)
	var result error
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			q := center(cell{x, y})
			overlap := true
			for _, r := range spans {
				v := q.X*r.axis.X + q.Y*r.axis.Y
				if v <= r.lo+1e-9 || v >= r.hi-1e-9 {
					overlap = false
					break
				}
			}
			if overlap {
				err := g.cellError(cell{x, y})
				if errors.Is(err, ErrBlocked) {
					return err
				}
				if err != nil {
					result = err
				}
			}
		}
	}
	return result
}
func (g *Grid) ClearSweep(a, b Point, yaw float64) bool   { return g.sweepError(a, b, yaw, 0) == nil }
func (g *Grid) turnError(p Point, from, to float64) error { return g.turnErrorPadding(p, from, to, 0) }
func (g *Grid) turnErrorPadding(p Point, from, to, translationPadding float64) error {
	return g.rotateSweepError(p, p, from, to, translationPadding)
}
func (g *Grid) rotateSweepError(a, b Point, from, to, translationPadding float64) error {
	if g == nil || !finite(from) || !finite(to) || !finite(translationPadding) || translationPadding < 0 || math.Abs(to-from) > 2*math.Pi {
		return ErrInvalid
	}
	delta := to - from
	n := max(1, int(math.Ceil(math.Abs(delta)/(math.Pi/90))))
	// Disk expansion is enclosed by expanding both rectangle half-extents.
	// The expanded corner radius bounds displacement to the nearest sampled
	// angle, so the entire continuous rotation is covered between samples.
	padding := translationPadding + math.Hypot(g.halfLength+translationPadding, g.halfWidth+translationPadding)*math.Abs(delta)/float64(n)/2
	for i := 0; i <= n; i++ {
		if err := g.sweepError(a, b, from+delta*float64(i)/float64(n), padding); err != nil {
			return err
		}
	}
	return nil
}
func (g *Grid) ClearTurn(p Point, from, to float64) bool {
	if g == nil {
		return false
	}
	return g.turnError(p, from, to) == nil
}
func (g *Grid) transition(p Point, yaw float64, q Point) error {
	if distance(p, q) < 1e-9 {
		return g.sweepError(p, p, yaw, 0)
	}
	h := yaw + normalize(heading(p, q)-yaw)
	if err := g.turnError(p, yaw, h); err != nil {
		return err
	}
	return g.sweepError(p, q, h, 0)
}

type state struct {
	c cell
	h int
}
type entry struct {
	s              state
	priority, cost float64
}
type queue []entry

func (q queue) Len() int { return len(q) }
func (q queue) Less(i, j int) bool {
	if q[i].priority == q[j].priority {
		return q[i].cost < q[j].cost
	}
	return q[i].priority < q[j].priority
}
func (q queue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *queue) Push(v any)   { *q = append(*q, v.(entry)) }
func (q *queue) Pop() any     { v := (*q)[len(*q)-1]; *q = (*q)[:len(*q)-1]; return v }

// Plan includes the start and exact goal. Planning is bounded and must happen
// while stopped. Consumers must revalidate authority and fresh observations
// after it returns; an old route never authorizes a future command.
func (g *Grid) Plan(start Pose, goal Point) ([]Point, error) {
	if g == nil || !validPose(start) || !validPoint(goal) {
		return nil, ErrInvalid
	}
	if !g.area.contains(goal) || distance(point(start), goal) > 8 {
		return nil, ErrBounds
	}
	if err := g.sweepError(point(start), point(start), start.Yaw, 0); err != nil {
		return nil, err
	}
	if distance(point(start), goal) < 1e-9 {
		return []Point{point(start)}, nil
	}
	if err := g.cellError(at(goal)); err != nil {
		return nil, err
	}
	if g.transition(point(start), start.Yaw, goal) == nil {
		return []Point{point(start), goal}, nil
	}
	root := state{at(point(start)), 8}
	pos := func(s state) Point {
		return Point{start.X + float64(s.c.X-root.c.X)*CellSize, start.Y + float64(s.c.Y-root.c.Y)*CellSize}
	}
	angle := func(s state) float64 {
		if s.h == 8 {
			return start.Yaw
		}
		return float64(s.h) * math.Pi / 4
	}
	open := &queue{{root, distance(point(start), goal), 0}}
	heap.Init(open)
	costs := map[state]float64{root: 0}
	parents := map[state]state{}
	closed := map[state]bool{}
	dirs := []cell{{1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}, {0, -1}, {1, -1}}
	for open.Len() > 0 && len(closed) < 12000 && open.Len() < 50000 {
		e := heap.Pop(open).(entry)
		if closed[e.s] || e.cost != costs[e.s] {
			continue
		}
		closed[e.s] = true
		p := pos(e.s)
		if distance(p, goal) <= CellSize*1.5 && g.transition(p, angle(e.s), goal) == nil {
			path := []Point{goal}
			s := e.s
			for s != root && len(path) < 256 {
				path = append(path, pos(s))
				s = parents[s]
			}
			if s != root {
				return nil, ErrNoRoute
			}
			path = append(path, point(start))
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path, nil
		}
		for h, d := range dirs {
			next := state{cell{e.s.c.X + d.X, e.s.c.Y + d.Y}, h}
			q := pos(next)
			if closed[next] || g.transition(p, angle(e.s), q) != nil {
				continue
			}
			cost := e.cost + distance(p, q) + .02*math.Abs(normalize(float64(h)*math.Pi/4-angle(e.s)))
			if cost > 12 {
				continue
			}
			old, seen := costs[next]
			if !seen || cost < old {
				costs[next] = cost
				parents[next] = e.s
				heap.Push(open, entry{next, cost + distance(q, goal), cost})
			}
		}
	}
	return nil, ErrNoRoute
}

// BodyMotion is measured horizontal body velocity, including lateral drift.
type BodyMotion struct{ ForwardMPS, LateralMPS, YawRateRPS float64 }

// CommandObserved checks a conservative joint translation/rotation envelope.
// Total horizontal travel is assumed bounded by speed*HorizonS+ExtraStopM,
// and angular travel by abs(yaw rate)*HorizonS+ExtraStopRad. These are explicit
// commissioned simulation assumptions, not dynamics inferred from one sample.
// The translation disk permits arbitrary drift direction; the symmetric yaw
// interval permits reversal. Every intersected cell must be observed free.
func (g *Grid) CommandObserved(p Pose, target Point, motion BodyMotion, l MotionLimits) (Command, error) {
	if g != nil && g.motionEnvelope == IsotropicGaitV1 {
		return g.commandGait(p, target, motion, l)
	}
	if !finite(motion.LateralMPS) || math.Abs(motion.LateralMPS) > .05 {
		return Command{}, ErrInvalid
	}
	// Reuse the complete existing input/limit validation without using its
	// axis-only measured-motion checks as evidence of joint clearance.
	if _, err := g.commandInputs(p, target, motion.ForwardMPS, motion.YawRateRPS, l); err != nil {
		return Command{}, err
	}
	radius := math.Hypot(motion.ForwardMPS, motion.LateralMPS)*l.HorizonS + l.ExtraStopM
	angle := math.Abs(motion.YawRateRPS)*l.HorizonS + l.ExtraStopRad
	if err := g.continuationEnvelope(p, radius, 0, angle); err != nil {
		return Command{}, err
	}
	// Keep axis transitions stopped until measured drift settles. A zero
	// proposal is still subject to fresh authority/deadline checks by callers.
	if math.Abs(motion.LateralMPS) > .03 || (math.Abs(motion.ForwardMPS) > .03 && math.Abs(motion.YawRateRPS) > .03) {
		return Command{}, nil
	}
	cmd, err := g.Command(p, target, motion.ForwardMPS, motion.YawRateRPS, l)
	if err != nil || cmd == (Command{}) {
		return cmd, err
	}
	// A new axis command and residual motion may coexist. Sum their travel
	// budgets rather than treating two separate clearance checks as a union.
	// This deliberately overcounts same-direction motion; no acceleration or
	// braking model is inferred from the measured sample.
	radius = math.Hypot(motion.ForwardMPS, motion.LateralMPS)*l.HorizonS + l.ExtraStopM
	angle = (math.Abs(motion.YawRateRPS)+math.Abs(cmd.YawRateRPS))*l.HorizonS + l.ExtraStopRad
	if err = g.continuationEnvelope(p, radius, math.Abs(cmd.ForwardMPS)*l.HorizonS, angle); err != nil {
		return Command{}, err
	}
	return cmd, nil
}

// Command proposes a body-forward OR in-place yaw command after checking its
// stopping area. Horizon and extra stopping travel are commissioned simulation
// assumptions. Call for both measured and prior-commanded motion as needed;
// this helper never substitutes for an actuator watchdog or authority check.
func (g *Grid) Command(p Pose, target Point, currentForwardMPS, currentYawRateRPS float64, l MotionLimits) (Command, error) {
	if g != nil && g.motionEnvelope == IsotropicGaitV1 {
		return g.commandGait(p, target, BodyMotion{ForwardMPS: currentForwardMPS, YawRateRPS: currentYawRateRPS}, l)
	}
	zero := Command{}
	if _, err := g.commandInputs(p, target, currentForwardMPS, currentYawRateRPS, l); err != nil {
		return zero, err
	}
	if err := g.sweepError(point(p), point(p), p.Yaw, 0); err != nil {
		return zero, err
	}
	// Meaningful simultaneous drift needs a curved-motion model. Refuse it;
	// neither an axis-only sweep nor separate straight/turn checks prove it.
	if math.Abs(currentForwardMPS) > .03 && math.Abs(currentYawRateRPS) > .03 {
		return zero, ErrBlocked
	}
	if math.Abs(currentForwardMPS) > .001 {
		travel := currentForwardMPS*l.HorizonS + math.Copysign(l.ExtraStopM, currentForwardMPS)
		q := Point{p.X + math.Cos(p.Yaw)*travel, p.Y + math.Sin(p.Yaw)*travel}
		if err := g.sweepError(point(p), q, p.Yaw, 0); err != nil {
			return zero, err
		}
	}
	if math.Abs(currentYawRateRPS) > .001 {
		end := p.Yaw + currentYawRateRPS*l.HorizonS + math.Copysign(l.ExtraStopRad, currentYawRateRPS)
		if err := g.turnError(point(p), p.Yaw, end); err != nil {
			return zero, err
		}
	}
	if distance(point(p), target) <= l.WaypointToleranceM {
		return zero, nil
	}
	if !g.area.contains(target) {
		return zero, ErrBounds
	}
	errAngle := normalize(heading(point(p), target) - p.Yaw)
	if math.Abs(errAngle) > .08 {
		if math.Abs(currentForwardMPS) > .03 {
			return zero, nil
		}
		rate := math.Copysign(l.MaxYawRateRPS, errAngle)
		end := p.Yaw + rate*l.HorizonS + math.Copysign(l.ExtraStopRad, rate)
		if err := g.turnError(point(p), p.Yaw, end); err != nil {
			return zero, err
		}
		return Command{YawRateRPS: rate}, nil
	}
	if math.Abs(currentYawRateRPS) > .03 {
		return zero, nil
	}
	travel := l.MaxForwardMPS*l.HorizonS + l.ExtraStopM
	q := Point{p.X + math.Cos(p.Yaw)*travel, p.Y + math.Sin(p.Yaw)*travel}
	if err := g.sweepError(point(p), q, p.Yaw, 0); err != nil {
		return zero, err
	}
	return Command{ForwardMPS: l.MaxForwardMPS}, nil
}

func (g *Grid) commandInputs(p Pose, target Point, currentForwardMPS, currentYawRateRPS float64, l MotionLimits) (Command, error) {
	zero := Command{}
	for _, v := range []float64{currentForwardMPS, currentYawRateRPS, l.MaxForwardMPS, l.MaxYawRateRPS, l.HorizonS, l.ExtraStopM, l.ExtraStopRad, l.WaypointToleranceM} {
		if !finite(v) {
			return zero, ErrInvalid
		}
	}
	if g == nil || !validPose(p) || !validPoint(target) || l.MaxForwardMPS <= 0 || l.MaxForwardMPS > .35 || l.MaxYawRateRPS <= 0 || l.MaxYawRateRPS > .5 || l.HorizonS < 1 || l.HorizonS > 2 || l.ExtraStopM < .05 || l.ExtraStopM > .5 || l.ExtraStopRad < .05 || l.ExtraStopRad > .5 || l.WaypointToleranceM <= 0 || l.WaypointToleranceM > .3 || math.Abs(currentForwardMPS) > .5 || math.Abs(currentYawRateRPS) > .8 {
		return zero, ErrInvalid
	}
	return zero, nil
}

// CheckContinuation includes up to two retained prior axis targets as well as
// measured drift and the newly proposed axis target. Even a zero proposal must
// cover the prior target's residual travel. The maximum prior magnitudes bound
// either retained target; their motion is added to the measured and proposed
// budgets. This is a conditional simulation envelope, not a braking model.
func (g *Grid) CheckContinuation(p Pose, measured BodyMotion, prior []Command, proposed Command, l MotionLimits) error {
	if len(prior) > 2 || !g.validLateral(measured.LateralMPS, l) {
		return ErrInvalid
	}
	if _, err := g.commandInputs(p, point(p), measured.ForwardMPS, measured.YawRateRPS, l); err != nil {
		return err
	}
	valid := func(c Command) bool {
		return finite(c.ForwardMPS) && finite(c.YawRateRPS) && c.ForwardMPS >= 0 && c.ForwardMPS <= l.MaxForwardMPS && math.Abs(c.YawRateRPS) <= l.MaxYawRateRPS && (c.ForwardMPS == 0 || c.YawRateRPS == 0)
	}
	if !valid(proposed) {
		return ErrInvalid
	}
	pv, pw := 0.0, 0.0
	for _, c := range prior {
		if !valid(c) {
			return ErrInvalid
		}
		pv = math.Max(pv, c.ForwardMPS)
		pw = math.Max(pw, math.Abs(c.YawRateRPS))
	}
	radius := math.Hypot(measured.ForwardMPS, measured.LateralMPS)*l.HorizonS + l.ExtraStopM
	angle := (math.Abs(measured.YawRateRPS)+pw+math.Abs(proposed.YawRateRPS))*l.HorizonS + l.ExtraStopRad
	// Angular travel >=pi in either direction covers every orientation. Keeping
	// one full turn bounds work without dropping any reachable footprint.
	angle = math.Min(angle, math.Pi)
	return g.continuationEnvelope(p, radius, (pv+proposed.ForwardMPS)*l.HorizonS, angle)
}

// continuationEnvelope assumes arbitrary residual travel <=residual and
// nonnegative forward travel <=forward with heading within the angular budget.
// A command's sign does not prove this physical decomposition; directional-v1
// is an explicit commissioned simulation model. Isotropic-v1 assumes no
// direction for target-induced travel and preserves its original envelope.
func (g *Grid) continuationEnvelope(p Pose, residual, forward, angle float64) error {
	if g == nil || !validPose(p) || !finite(residual) || !finite(forward) || !finite(angle) || residual < 0 || forward < 0 || angle < 0 {
		return ErrInvalid
	}
	a, b := point(p), point(p)
	padding := residual + forward
	switch g.motionEnvelope {
	case IsotropicV1, IsotropicGaitV1:
	case DirectionalV1:
		// Body padding alone cannot bound the direction of future center travel.
		angle += g.initialYawUncertainty
		if angle < math.Pi/2 && forward > 0 {
			b = Point{a.X + forward*math.Cos(p.Yaw), a.Y + forward*math.Sin(p.Yaw)}
			padding = residual + forward*math.Sin(angle)
		}
	default:
		return ErrInvalid
	}
	angle = math.Min(angle, math.Pi)
	return g.rotateSweepError(a, b, p.Yaw-angle, p.Yaw+angle, padding)
}
