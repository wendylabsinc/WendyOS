package sitenav

import (
	"context"
	"errors"
	"math"
)

type VisualHint struct {
	Position Point   `json:"position"`
	Yaw      float64 `json:"yaw"`
	At       float64 `json:"at"`
	Inliers  int     `json:"inliers"`
	Residual float64 `json:"residual"`
}

type RegistrationInput struct {
	Cloud      SurfaceCloud
	Pose       Pose
	ClearFloor []Point
	Visual     *VisualHint
	Reference  []Surface
}

type RegistrationFit struct {
	NeedsCorroboration bool          `json:"needsCorroboration"`
	Pose               PoseCandidate `json:"pose"`
	Architecture       int           `json:"architecture"`
	Furniture          int           `json:"furniture"`
	Reference          int           `json:"reference"`
	Directions         int           `json:"directions"`
	Cost               float64       `json:"cost"`
	Valid              bool          `json:"valid"`
	Plausible          bool          `json:"plausible"`
	Rejection          string        `json:"rejection,omitempty"`
	Classes            []int         `json:"classes"` // 0 rejected, 1 architecture, 2 furniture, 3 saved scan
}

type LayeredRoom struct {
	Base           RoomMap
	Walls, Objects []RoomMap
	Tops           []Triangle
}

func NewLayeredRoom(architecture, furniture []Triangle) (LayeredRoom, error) {
	base, err := RoomFromMesh(architecture)
	if err != nil {
		return LayeredRoom{}, err
	}
	model := LayeredRoom{Base: base}
	var floors []Triangle
	for _, t := range architecture {
		_, up := triangleArea(t)
		if up > .98 && math.Abs((t[0].Y+t[1].Y+t[2].Y)/3-base.FloorY) < .06 {
			floors = append(floors, t)
		}
	}
	for _, height := range surfaceHeights {
		walls, e := roomFromMeshAtHeight(architecture, height)
		if e != nil {
			walls = RoomMap{}
		}
		objects, e := roomFromMeshAtHeight(append(append([]Triangle(nil), floors...), furniture...), height)
		if e != nil {
			objects = RoomMap{}
		}
		model.Walls = append(model.Walls, walls)
		model.Objects = append(model.Objects, objects)
	}
	for _, t := range furniture {
		_, up := triangleArea(t)
		if up > .95 && (t[0].Y+t[1].Y+t[2].Y)/3 > base.FloorY+.3 {
			model.Tops = append(model.Tops, t)
		}
	}
	return model, nil
}

func heightBand(z float64) int {
	best := 0
	for i := range surfaceHeights {
		if math.Abs(surfaceHeights[i]-z) < math.Abs(surfaceHeights[best]-z) {
			best = i
		}
	}
	return best
}

func topDistance(p Vertex, triangles []Triangle) float64 {
	best := .0625
	q := Point{p.X, p.Y}
	for _, t := range triangles {
		h := (t[0].Y + t[1].Y + t[2].Y) / 3
		if math.Abs(p.Z-h) > .2 {
			continue
		}
		a, b, c := Point{t[0].X, -t[0].Z}, Point{t[1].X, -t[1].Z}, Point{t[2].X, -t[2].Z}
		cross := func(a, b Point) float64 { return (b.X-a.X)*(q.Y-a.Y) - (b.Y-a.Y)*(q.X-a.X) }
		u, v, w := cross(a, b), cross(b, c), cross(c, a)
		if (u >= 0 && v >= 0 && w >= 0) || (u <= 0 && v <= 0 && w <= 0) {
			best = math.Min(best, (p.Z-h)*(p.Z-h))
		}
	}
	return best
}

// RegisterSurfaces keeps height, semantic class and surface direction throughout
// search. Furniture is supporting evidence, never equivalent to architecture.
// Returned alternatives are hypotheses, not permission to align or move.
func RegisterSurfaces(ctx context.Context, model LayeredRoom, input RegistrationInput) ([]RegistrationFit, error) {
	if len(input.Cloud.Surfaces) < 24 {
		return nil, errors.New("Waiting for more observed surface structure.")
	}
	if len(input.Cloud.Surfaces) > 900 || len(input.Reference) > 3000 {
		return nil, errors.New("Localization map exceeds its bounded sample budget.")
	}
	if !input.Pose.Position.Valid() || !finite(input.Cloud.Floor) {
		return nil, errors.New("Invalid scan origin or floor.")
	}
	scan := input.Cloud.Surfaces
	for _, p := range scan {
		if !finite(p.Position.Z) || !finite(p.Direction.X) || !finite(p.Direction.Y) || !(Point{p.Position.X, p.Position.Y}).Valid() {
			return nil, errors.New("Invalid surface observation.")
		}
	}
	// A reference map is an optional geometric target. It contains past observed
	// surfaces in site coordinates, not navigable cells and not a prior pose vote.
	type key3 struct{ X, Y, Z int }
	reference := map[key3][]Surface{}
	for _, p := range input.Reference {
		if !(Point{p.Position.X, p.Position.Y}).Valid() || !finite(p.Position.Z) || math.Abs(p.Position.Z) > 1000 || !finite(p.Direction.X) || !finite(p.Direction.Y) || (!p.Horizontal && math.Abs(math.Hypot(p.Direction.X, p.Direction.Y)-1) > .05) {
			return nil, errors.New("Invalid saved surface map.")
		}
		k := key3{int(math.Floor(p.Position.X / .2)), int(math.Floor(p.Position.Y / .2)), int(math.Floor(p.Position.Z / .2))}
		reference[k] = append(reference[k], p)
	}
	referenceDistance := func(p Vertex, direction Point, horizontal bool) (float64, bool) {
		stable := false
		best := .0625
		k := key3{int(math.Floor(p.X / .2)), int(math.Floor(p.Y / .2)), int(math.Floor(p.Z / .2))}
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					for _, v := range reference[key3{k.X + dx, k.Y + dy, k.Z + dz}] {
						if v.Horizontal != horizontal || (!horizontal && math.Abs(v.Direction.X*direction.X+v.Direction.Y*direction.Y) < .966) {
							continue
						}
						dist := (v.Position.X-p.X)*(v.Position.X-p.X) + (v.Position.Y-p.Y)*(v.Position.Y-p.Y) + (v.Position.Z-p.Z)*(v.Position.Z-p.Z)
						if dist < best {
							best = dist
							stable = v.Stable
						}
					}
				}
			}
		}
		return best, stable
	}
	freeCells := map[Cell]bool{}
	for _, p := range input.ClearFloor {
		if p.Valid() {
			freeCells[cell(p)] = true
		}
	}
	var free []Point
	for k := range freeCells {
		interior := true
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				interior = interior && freeCells[Cell{k.X + dx, k.Y + dy}]
			}
		}
		if interior {
			free = append(free, center(k))
		}
	}
	// Sort all map-derived samples before capping so a packet replay is stable.
	sortPoints(free)
	if len(free) > 160 {
		all := free
		free = make([]Point, 160)
		for i := range free {
			free[i] = all[i*len(all)/len(free)]
		}
	}
	evaluate := func(c PoseCandidate, stride int, details bool) RegistrationFit {
		out := RegistrationFit{Pose: c}
		if !model.Base.onFloor(c.Position) {
			out.Pose.RMS = 1
			out.Pose.cost = 1
			out.Cost = 1
			return out
		}
		sn, cs := math.Sincos(c.Rotation)
		type group struct {
			points       []Point
			direction    Point
			offset       float64
			architecture bool
			columns      map[Cell]bool
		}
		groups := map[[3]int]*group{}
		cost, total, weight, inliers := 0., 0., 0., 0.
		stableReferences := 0
		if details {
			out.Classes = make([]int, len(scan))
		}
		for i := 0; i < len(scan); i += stride {
			sample := scan[i]
			x, y := sample.Position.X-input.Pose.Position.X, sample.Position.Y-input.Pose.Position.Y
			q := Vertex{c.Position.X + cs*x - sn*y, c.Position.Y + sn*x + cs*y, model.Base.FloorY + sample.Position.Z - input.Cloud.Floor}
			direction := Point{cs*sample.Direction.X - sn*sample.Direction.Y, sn*sample.Direction.X + cs*sample.Direction.Y}
			d, kind := .0625, 0
			residual := .36
			referenceStable := false
			var matched wallSurface
			sampleWeight := 1.
			if sample.Horizontal {
				sampleWeight = .2
				td := topDistance(q, model.Tops)
				residual = td
				if td < .04 {
					d = .025 + .4*td
					kind = 2
				}
			}
			if !sample.Horizontal {
				band := heightBand(sample.Position.Z - input.Cloud.Floor)
				for class, m := range []RoomMap{model.Walls[band], model.Objects[band]} {
					if m.width == 0 {
						continue
					}
					distance, id := m.nearestSurface(Point{q.X, q.Y})
					residual = math.Min(residual, distance)
					if id < 0 || distance > .04 {
						continue
					}
					wall := m.surfaces[id]
					if math.Abs(direction.X*wall.direction.X+direction.Y*wall.direction.Y) < .966 {
						continue
					}
					value := distance
					if class == 1 {
						value = .025 + .4*distance
					}
					if value < d {
						d = value
						kind = class + 1
						matched = wall
					}
				}
			}
			if len(reference) > 0 {
				rd, stable := referenceDistance(q, direction, sample.Horizontal)
				residual = math.Min(residual, rd)
				if rd < .04 && .012+.7*rd < d {
					d = .012 + .7*rd
					kind = 3
					referenceStable = stable
				}
			}
			cost += sampleWeight * d
			total += sampleWeight * residual
			weight += sampleWeight
			if kind != 0 {
				inliers += sampleWeight
				if kind == 1 {
					out.Architecture++
				}
				if kind == 2 {
					out.Furniture++
				}
				if kind == 3 {
					out.Reference++
					if referenceStable {
						stableReferences++
					}
				}
			}
			if details {
				out.Classes[i] = kind
			}
			if details && !sample.Horizontal && kind != 0 {
				// Saved surfaces use their measured direction and offset. Model walls
				// use their actual plane, keeping furniture and architecture distinct.
				if kind == 3 {
					matched = wallSurface{direction: direction, offset: -direction.Y*q.X + direction.X*q.Y}
				}
				angle := math.Atan2(matched.direction.Y, matched.direction.X)
				offset := matched.offset
				if angle < 0 {
					angle += math.Pi
					offset = -offset
				}
				groupKind := kind
				if kind == 3 && referenceStable {
					groupKind = 4
				}
				k := [3]int{groupKind, int(math.Round(angle / (math.Pi / 18))), int(math.Round(offset / .15))}
				g := groups[k]
				if g == nil {
					g = &group{direction: matched.direction, offset: matched.offset, architecture: kind == 1 || referenceStable, columns: map[Cell]bool{}}
					groups[k] = g
				}
				ck := cell(Point{q.X, q.Y})
				if !g.columns[ck] {
					g.points = append(g.points, Point{q.X, q.Y})
					g.columns[ck] = true
				}
			}
		}
		out.Pose.Coverage = inliers / weight
		out.Pose.RMS = math.Sqrt(total / weight)
		cost /= weight
		conflicts := 0
		for _, p := range free {
			x, y := p.X-input.Pose.Position.X, p.Y-input.Pose.Position.Y
			q := Point{c.Position.X + cs*x - sn*y, c.Position.Y + sn*x + cs*y}
			if model.Base.distanceSquared(q) < .075*.075 {
				conflicts++
			}
		}
		if len(free) >= 24 {
			out.Pose.floorConflict = float64(conflicts) / float64(len(free))
			cost += out.Pose.floorConflict * .25
		}
		visualAgrees := false
		if v := input.Visual; v != nil {
			distance := c.Position.Distance(v.Position)
			angle := math.Abs(math.Remainder(input.Pose.Yaw+c.Rotation-v.Yaw, 2*math.Pi))
			cost += .018 * math.Min(4, distance*distance/(.4*.4)+angle*angle/(.2*.2))
			visualAgrees = distance < .6 && angle < .3
		}
		out.Pose.cost = cost
		out.Cost = cost
		if details {
			var supported []*group
			architecture := false
			for _, g := range groups {
				direction, planar := planarWallDirection(g.points)
				if len(g.points) < 6 || !planar || math.Abs(direction.X*g.direction.X+direction.Y*g.direction.Y) < .966 {
					continue
				}
				supported = append(supported, g)
				architecture = architecture || g.architecture
			}
			if len(supported) > 0 {
				out.Directions = 1
			}
			for i, a := range supported {
				for _, b := range supported[i+1:] {
					if math.Abs(a.direction.X*b.direction.Y-a.direction.Y*b.direction.X) > .342 {
						out.Directions = 2
					}
				}
			}
			// At least one architectural surface anchors furniture. A registered
			// robot map may supply that anchor only with a broad measured match.
			anchored := architecture && (out.Architecture >= 12 || stableReferences >= 12)
			// A well-fitting parallel-wall explanation remains a competing pose,
			// even though it cannot establish position by itself. Discarding it
			// would let a worse corner match appear uniquely determined.
			out.Plausible = anchored && out.Directions >= 1 && out.Pose.Coverage >= .5 && cost < .045 && out.Pose.floorConflict <= .05
			out.Valid = out.Plausible && (out.Directions >= 2 || visualAgrees)
			// A door-sized plane can supply the only cross-wall direction and
			// make a shifted corner look convincing. Test loss of each supported
			// plane; fragile geometry needs independent camera or view evidence.
			robust := len(supported) >= 3
			for omitted := range supported {
				independentDirections := false
				for _, a := range supported {
					if sameSupportPlane(a.direction, a.offset, supported[omitted].direction, supported[omitted].offset) || !a.architecture {
						continue
					}
					for _, b := range supported {
						if sameSupportPlane(b.direction, b.offset, supported[omitted].direction, supported[omitted].offset) || !b.architecture {
							continue
						}
						if math.Abs(a.direction.X*b.direction.Y-a.direction.Y*b.direction.X) > .342 {
							independentDirections = true
						}
					}
				}
				robust = robust && independentDirections
			}
			out.NeedsCorroboration = !robust && !visualAgrees
			switch {
			case out.Directions == 0:
				out.Rejection = "Matched surface points do not span a reliable wall direction."
			case !anchored:
				out.Rejection = "No sufficiently broad architectural wall match anchors this position."
			case out.Directions < 2 && !visualAgrees:
				out.Rejection = "Only parallel surface directions match. A second direction or a verified camera match is needed."
			case out.Pose.floorConflict > .05:
				out.Rejection = "This alignment places observed clear floor across room walls."
			case out.Pose.Coverage < .5:
				out.Rejection = "Too few observed surfaces match this room position."
			case cost >= .045:
				out.Rejection = "The alignment error is too large for the observed surfaces and available camera evidence."
			}
		}
		return out
	}
	stride := max(1, (len(scan)+179)/180)
	raw, err := searchRoom(ctx, model.Base, func(c PoseCandidate) PoseCandidate { return evaluate(c, 1, false).Pose }, func(c PoseCandidate) PoseCandidate { return evaluate(c, stride, false).Pose }, func(c PoseCandidate) bool { return c.cost < .09 && c.Coverage >= .25 })
	if err != nil {
		return nil, err
	}
	var results []RegistrationFit
	for _, c := range raw {
		results = append(results, evaluate(c, 1, true))
	}
	// Furniture receives a weaker search score than architecture. That preference
	// cannot resolve two geometrically indistinguishable explanations: a desk
	// corner can also fit an unrelated room corner. Keep that ambiguity unless
	// an independent camera pose agrees with the architectural explanation.
	for i := range results {
		fit := &results[i]
		if !fit.Valid {
			continue
		}
		if v := input.Visual; v != nil && fit.Pose.Position.Distance(v.Position) < .6 && math.Abs(math.Remainder(input.Pose.Yaw+fit.Pose.Rotation-v.Yaw, 2*math.Pi)) < .3 {
			continue
		}
		for _, other := range results {
			if other.Furniture == 0 || other.Architecture >= 12 || other.Reference >= 12 || other.Pose.Coverage < math.Max(.8, fit.Pose.Coverage-.05) || other.Pose.RMS > fit.Pose.RMS+.03 {
				continue
			}
			if compatiblePose(fit.Pose, other.Pose) {
				continue
			}
			fit.Valid = false
			fit.Rejection = "The same observed shape also fits furniture elsewhere in the room. More distinct geometry or a verified camera match is needed."
			break
		}
	}
	return results, nil
}

// Height bands and saved references can label one physical plane more than once.
// Removing a plane must remove those aliases as well.
func sameSupportPlane(a Point, ao float64, b Point, bo float64) bool {
	dot := a.X*b.X + a.Y*b.Y
	if math.Abs(dot) < .966 {
		return false
	}
	if dot < 0 {
		bo = -bo
	}
	return math.Abs(ao-bo) < .25
}
