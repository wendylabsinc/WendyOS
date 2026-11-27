// Package posegraph is a planar (SE2) pose graph with robust nonlinear
// least-squares optimisation.
//
// Nodes are poses (geom.Pose2: World X, Z and Yaw from +X toward +Z) grouped
// into trajectories, one per robot or session. Edges are measurements with an
// information matrix (inverse covariance): the relative pose of two nodes, an
// absolute pose or position of a node, the distance between two nodes, or a
// node's observation of a point landmark. Optimize moves the node estimates
// in place to the minimum of Σ ρ(rᵀ·Ω·r) by Levenberg-Marquardt on a sparse
// block Cholesky factor, and reports the χ² of every edge so that callers can
// reject false loop closures.
//
// Estimates of one trajectory start in that trajectory's own frame. Align
// moves a trajectory into another's frame from the edges between them, after
// which a joint Optimize refines both; FrameTransform reads the estimated
// transform between two trajectory frames.
//
// The state of a node is (x, z, yaw) in the graph frame, updated additively,
// so covariances are over (x, z, yaw) as everywhere else in geom. A Graph is
// not safe for concurrent use. Results depend only on the order of the calls
// that built the graph.
package posegraph

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

var (
	// ErrUnknownID reports a node, landmark, edge or trajectory that is not in the graph.
	ErrUnknownID = errors.New("posegraph: unknown id")
	// ErrInvalid reports an edge that cannot be a measurement: an unknown
	// kind, both ends on one node, a negative range, or an information matrix
	// that is not symmetric positive semi-definite and non-zero.
	ErrInvalid = errors.New("posegraph: invalid edge")
	// ErrAligned reports an Align of two trajectories that already share a frame.
	ErrAligned = errors.New("posegraph: trajectories already share a frame")
	// ErrDisconnected reports a frame query between trajectories that no
	// Align or Optimize has brought into a common frame.
	ErrDisconnected = errors.New("posegraph: trajectories do not share a frame")
)

type (
	// NodeID, LandmarkID and EdgeID are dense indices in insertion order.
	NodeID     int32
	LandmarkID int32
	EdgeID     int32
	// Trajectory names one robot's or session's chain of nodes and the frame
	// its poses are first expressed in.
	Trajectory string
)

// Kernel selects the robust loss ρ applied to the χ² of edges marked Robust.
type Kernel uint8

const (
	// KernelNone is the plain quadratic loss.
	KernelNone Kernel = iota
	// KernelHuber is quadratic up to Config.HuberDelta σ and linear beyond: it
	// bounds the pull of an outlier but never releases it.
	KernelHuber
	// KernelDCS uses the dynamic covariance scaling weights of Agarwal et al.
	// (2013): an edge keeps full weight up to χ² = Φ and is scaled by
	// (2Φ/(Φ+χ²))² beyond, so a false loop closure is switched off. The loss
	// is bounded by 3Φ, which makes the problem non-convex: it needs an
	// initial estimate in which the true closures are closer than the false ones.
	KernelDCS
)

// Config tunes Optimize. Zero fields take the documented default.
type Config struct {
	// MaxIterations bounds the Levenberg-Marquardt steps of one Optimize
	// call, rejected steps included. Default 100: a robust kernel converges
	// linearly while edges near its threshold settle.
	MaxIterations int
	// InitialLambda is the starting Marquardt damping relative to diag(H). Default 1e-4.
	InitialLambda float64
	// StepTol ends the iteration when no state component moves by more than
	// this many metres or radians. Default 1e-6.
	StepTol float64
	// CostTol ends the iteration when an accepted step lowers the robust cost
	// by at most this fraction. Default 1e-5.
	CostTol float64
	// Kernel is the robust loss of edges marked Robust. Default KernelNone.
	Kernel Kernel
	// HuberDelta is the Mahalanobis distance, in σ, at which the Huber loss
	// turns linear. Default 2.
	HuberDelta float64
	// DCSPhi is Φ: the χ² up to which a DCS edge keeps full weight. Default 5,
	// which leaves most inliers of a 3-dof edge unscaled (P(χ²₃ ≤ 5) = 0.83).
	DCSPhi float64
}

func (c Config) withDefaults() Config {
	if c.MaxIterations <= 0 {
		c.MaxIterations = 100
	}
	if c.InitialLambda <= 0 {
		c.InitialLambda = 1e-4
	}
	if c.StepTol <= 0 {
		c.StepTol = 1e-6
	}
	if c.CostTol <= 0 {
		c.CostTol = 1e-5
	}
	if c.HuberDelta <= 0 {
		c.HuberDelta = 2
	}
	if c.DCSPhi <= 0 {
		c.DCSPhi = 5
	}
	return c
}

// kernel returns ρ(χ²) and the IRLS weight ρ'(χ²).
func (c *Config) kernel(robust bool, chi2 float64) (rho, weight float64) {
	if robust {
		switch c.Kernel {
		case KernelHuber:
			if s := math.Sqrt(chi2); s > c.HuberDelta {
				return c.HuberDelta * (2*s - c.HuberDelta), c.HuberDelta / s
			}
		case KernelDCS:
			if phi := c.DCSPhi; chi2 > phi {
				s := 2 * phi / (phi + chi2)
				return 3*phi - 2*phi*s, s * s
			}
		}
	}
	return chi2, 1
}

// EdgeKind is the measurement model of an edge.
type EdgeKind uint8

const (
	// EdgeRelative measures the pose of node To in the frame of node From.
	EdgeRelative EdgeKind = iota + 1
	// EdgePosePrior measures the pose of node From in the graph frame.
	EdgePosePrior
	// EdgePositionPrior measures the position of node From in the graph
	// frame, as a GNSS or UWB fix does. It does not observe yaw.
	EdgePositionPrior
	// EdgeRange measures the distance between nodes From and To.
	EdgeRange
	// EdgeLandmark measures the position of a landmark in the frame of node From.
	EdgeLandmark
)

// Dim is the number of residual components of the kind, 0 for an unknown kind.
func (k EdgeKind) Dim() int {
	switch k {
	case EdgeRelative, EdgePosePrior:
		return 3
	case EdgePositionPrior, EdgeLandmark:
		return 2
	case EdgeRange:
		return 1
	}
	return 0
}

// Edge is one measurement. Build it with Relative, PosePrior, PositionPrior,
// Range or Observation; only the fields its kind names are read.
type Edge struct {
	Kind     EdgeKind
	From     NodeID
	To       NodeID     // EdgeRelative, EdgeRange
	Landmark LandmarkID // EdgeLandmark
	// Pose is To in From's frame (EdgeRelative) or the node's pose in the
	// graph frame (EdgePosePrior).
	Pose geom.Pose2
	// Point is the node's position in the graph frame (EdgePositionPrior) or
	// the landmark in From's frame, +X forward and +Z right (EdgeLandmark).
	Point geom.Vec2
	// Distance is the measured range in metres (EdgeRange).
	Distance float64
	// Info is the information matrix of the residual in its leading
	// Kind.Dim() rows and columns. The residual order is (x, z, yaw) in the
	// frame the measurement is expressed in, (x, z), or (range).
	Info geom.Mat3
	// Robust subjects the edge to Config.Kernel. Set it on loop closures,
	// inter-robot matches and absolute fixes, which can be wrong; leave it
	// off odometry, which holds a trajectory together.
	Robust bool
}

// Relative constrains to's pose in from's frame to z. info is over (x, z,
// yaw) of z: geom.Mat3.Inverse of a scan matcher's covariance (also of a
// degenerate one, whose unobserved direction then carries next to no
// information) or of spatial.OdometryNoise.Covariance for an odometry step.
func Relative(from, to NodeID, z geom.Pose2, info geom.Mat3) Edge {
	return Edge{Kind: EdgeRelative, From: from, To: to, Pose: z, Info: info}
}

// PosePrior constrains a node's pose in the graph frame.
func PosePrior(node NodeID, pose geom.Pose2, info geom.Mat3) Edge {
	return Edge{Kind: EdgePosePrior, From: node, Pose: pose, Info: info}
}

// PositionPrior constrains a node's position in the graph frame. info is
// row-major [xx, xz, zx, zz] in m⁻².
func PositionPrior(node NodeID, position geom.Vec2, info geom.Cov2) Edge {
	return Edge{Kind: EdgePositionPrior, From: node, Point: position, Info: info2(info)}
}

// Range constrains the distance between two nodes to distance ± sigma metres.
// A non-positive sigma makes the edge invalid.
func Range(a, b NodeID, distance, sigma float64) Edge {
	e := Edge{Kind: EdgeRange, From: a, To: b, Distance: distance}
	if sigma > 0 {
		e.Info[0] = 1 / (sigma * sigma)
	}
	return e
}

// Observation constrains a landmark's position in a node's frame. info is
// row-major [xx, xz, zx, zz] in m⁻².
func Observation(node NodeID, landmark LandmarkID, point geom.Vec2, info geom.Cov2) Edge {
	return Edge{Kind: EdgeLandmark, From: node, Landmark: landmark, Point: point, Info: info2(info)}
}

func info2(c geom.Cov2) geom.Mat3 { return geom.Mat3{c[0], c[1], 0, c[2], c[3]} }

type trajectory struct {
	name  Trajectory
	nodes []NodeID
	// frame labels the set of trajectories whose estimates share a frame.
	frame int32
}

// Graph holds the nodes, landmarks and edges and their current estimates.
type Graph struct {
	cfg    Config
	est    []geom.Pose2 // node estimates in the graph frame
	local  []geom.Pose2 // node poses in their trajectory's own frame, as added
	fixed  []bool
	trajOf []int32
	trajs  []trajectory
	byName map[Trajectory]int32 // lookups only, never iterated
	lms    []geom.Vec2
	edges  []Edge
	gone   []bool // removed edges keep their id
	// sys is the undamped factorisation at the current estimate. Every change
	// to the graph clears it.
	sys *system
}

// New returns an empty graph.
func New(cfg Config) *Graph {
	return &Graph{cfg: cfg.withDefaults(), byName: map[Trajectory]int32{}}
}

func finitePose(p geom.Pose2) bool { return finite(p.X, p.Z, p.Yaw) }

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// AddNode appends a node to a trajectory, creating the trajectory on first
// use. local is the pose in the trajectory's own frame (its odometry or local
// map frame). The estimate starts at the previous node's estimate composed
// with the local motion since it, so a node added after an optimisation or
// alignment starts from the corrected trajectory; the first node of a
// trajectory starts at local.
func (g *Graph) AddNode(t Trajectory, local geom.Pose2) (NodeID, error) {
	if !finitePose(local) {
		return 0, fmt.Errorf("posegraph: node pose: %w", geom.ErrNonFinite)
	}
	ti, ok := g.byName[t]
	if !ok {
		ti = int32(len(g.trajs))
		g.byName[t] = ti
		g.trajs = append(g.trajs, trajectory{name: t, frame: ti})
	}
	local = local.Normalized()
	est := local
	if nodes := g.trajs[ti].nodes; len(nodes) > 0 {
		last := nodes[len(nodes)-1]
		est = g.est[last].Compose(g.local[last].Between(local))
	}
	id := NodeID(len(g.est))
	g.est = append(g.est, est)
	g.local = append(g.local, local)
	g.fixed = append(g.fixed, false)
	g.trajOf = append(g.trajOf, ti)
	g.trajs[ti].nodes = append(g.trajs[ti].nodes, id)
	g.sys = nil
	return id, nil
}

// AddLandmark appends a point landmark with an initial position in the graph frame.
func (g *Graph) AddLandmark(position geom.Vec2) (LandmarkID, error) {
	if !finite(position.X, position.Z) {
		return 0, fmt.Errorf("posegraph: landmark: %w", geom.ErrNonFinite)
	}
	g.lms = append(g.lms, position)
	g.sys = nil
	return LandmarkID(len(g.lms) - 1), nil
}

func (g *Graph) hasNode(id NodeID) bool { return id >= 0 && int(id) < len(g.est) }

// AddEdge validates and appends a measurement. The information matrix is
// stored symmetrised.
func (g *Graph) AddEdge(e Edge) (EdgeID, error) {
	dim := e.Kind.Dim()
	if dim == 0 {
		return 0, fmt.Errorf("%w: kind %d", ErrInvalid, e.Kind)
	}
	if !g.hasNode(e.From) {
		return 0, fmt.Errorf("%w: node %d", ErrUnknownID, e.From)
	}
	switch e.Kind {
	case EdgeRelative, EdgeRange:
		if !g.hasNode(e.To) {
			return 0, fmt.Errorf("%w: node %d", ErrUnknownID, e.To)
		}
		if e.To == e.From {
			return 0, fmt.Errorf("%w: both ends on node %d", ErrInvalid, e.From)
		}
	case EdgeLandmark:
		if e.Landmark < 0 || int(e.Landmark) >= len(g.lms) {
			return 0, fmt.Errorf("%w: landmark %d", ErrUnknownID, e.Landmark)
		}
	}
	if !finitePose(e.Pose) || !finite(e.Point.X, e.Point.Z, e.Distance) {
		return 0, fmt.Errorf("posegraph: edge measurement: %w", geom.ErrNonFinite)
	}
	if e.Kind == EdgeRange && e.Distance < 0 {
		return 0, fmt.Errorf("%w: negative range", ErrInvalid)
	}
	info, err := checkInfo(e.Info, dim)
	if err != nil {
		return 0, err
	}
	e.Info = info
	g.edges = append(g.edges, e)
	g.gone = append(g.gone, false)
	g.sys = nil
	return EdgeID(len(g.edges) - 1), nil
}

// checkInfo returns the symmetrised leading d x d block of m, zero elsewhere,
// or an error unless that block is finite, symmetric to 1e-9 of its largest
// entry, positive semi-definite and non-zero.
func checkInfo(m geom.Mat3, d int) (geom.Mat3, error) {
	var out geom.Mat3
	scale := 0.0
	for i := 0; i < d; i++ {
		for j := 0; j < d; j++ {
			if !finite(m[i*3+j]) {
				return out, fmt.Errorf("posegraph: information matrix: %w", geom.ErrNonFinite)
			}
			out[i*3+j] = (m[i*3+j] + m[j*3+i]) / 2
			scale = math.Max(scale, math.Abs(m[i*3+j]))
		}
	}
	const tol = 1e-9
	bad := scale == 0
	for i := 0; i < 3; i++ {
		j := (i + 1) % 3
		bad = bad || math.Abs(m[i*3+j]-m[j*3+i]) > tol*scale && i < d && j < d
		// Sylvester for semi-definiteness: every principal minor is non-negative.
		bad = bad || out[i*4] < 0 || out[i*4]*out[j*4]-out[i*3+j]*out[i*3+j] < -tol*scale*scale
	}
	if bad || out.Det() < -tol*scale*scale*scale {
		return out, fmt.Errorf("%w: information matrix is not symmetric positive semi-definite", ErrInvalid)
	}
	return out, nil
}

// RemoveEdge drops an edge from the problem, for example a loop closure whose
// χ² marks it as false. Ids of other edges do not change.
func (g *Graph) RemoveEdge(id EdgeID) error {
	if id < 0 || int(id) >= len(g.edges) || g.gone[id] {
		return fmt.Errorf("%w: edge %d", ErrUnknownID, id)
	}
	g.gone[id] = true
	g.sys = nil
	return nil
}

// Edge returns a stored edge. ok is false for an unknown or removed id.
func (g *Graph) Edge(id EdgeID) (e Edge, ok bool) {
	if id < 0 || int(id) >= len(g.edges) || g.gone[id] {
		return Edge{}, false
	}
	return g.edges[id], true
}

// SetFixed holds a node at its current estimate (or releases it). A fixed
// node defines the frame of everything connected to it.
func (g *Graph) SetFixed(id NodeID, fixed bool) error {
	if !g.hasNode(id) {
		return fmt.Errorf("%w: node %d", ErrUnknownID, id)
	}
	g.fixed[id] = fixed
	g.sys = nil
	return nil
}

// SetPose overwrites a node's estimate in the graph frame.
func (g *Graph) SetPose(id NodeID, pose geom.Pose2) error {
	if !g.hasNode(id) {
		return fmt.Errorf("%w: node %d", ErrUnknownID, id)
	}
	if !finitePose(pose) {
		return fmt.Errorf("posegraph: node pose: %w", geom.ErrNonFinite)
	}
	g.est[id] = pose.Normalized()
	g.sys = nil
	return nil
}

// Pose returns a node's current estimate in the graph frame.
func (g *Graph) Pose(id NodeID) (geom.Pose2, error) {
	if !g.hasNode(id) {
		return geom.Pose2{}, fmt.Errorf("%w: node %d", ErrUnknownID, id)
	}
	return g.est[id], nil
}

// Local returns the pose a node was added with, in its trajectory's own frame.
func (g *Graph) Local(id NodeID) (geom.Pose2, error) {
	if !g.hasNode(id) {
		return geom.Pose2{}, fmt.Errorf("%w: node %d", ErrUnknownID, id)
	}
	return g.local[id], nil
}

// Landmark returns a landmark's current estimate in the graph frame.
func (g *Graph) Landmark(id LandmarkID) (geom.Vec2, error) {
	if id < 0 || int(id) >= len(g.lms) {
		return geom.Vec2{}, fmt.Errorf("%w: landmark %d", ErrUnknownID, id)
	}
	return g.lms[id], nil
}

// Nodes returns the nodes of a trajectory in insertion order, nil if unknown.
func (g *Graph) Nodes(t Trajectory) []NodeID {
	ti, ok := g.byName[t]
	if !ok {
		return nil
	}
	return slices.Clone(g.trajs[ti].nodes)
}

// NumNodes is the number of nodes; ids run from 0 to NumNodes()-1.
func (g *Graph) NumNodes() int { return len(g.est) }

// NumEdges is the number of edges ever added, removed ones included; ids run
// from 0 to NumEdges()-1.
func (g *Graph) NumEdges() int { return len(g.edges) }

// relabel merges frame label from into label to.
func (g *Graph) relabel(from, to int32) {
	for i := range g.trajs {
		if g.trajs[i].frame == from {
			g.trajs[i].frame = to
		}
	}
}
