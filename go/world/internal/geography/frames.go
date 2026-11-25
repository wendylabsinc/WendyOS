package geography

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	MaxFrameEdges = 4096
	MaxFrameDepth = 128
	// This is an arithmetic input bound, not a claim of survey accuracy.
	MaxFrameCoordinateMetres = 1e9
)

type frameEdge struct {
	frame    world.LocalFrame
	rotation [3][3]float64
}

// Graph is immutable after construction and may be used concurrently. Keys
// include the complete frame revision; a matching ID alone is insufficient.
type Graph struct {
	root  world.FrameRef
	edges map[world.FrameRef]frameEdge
}

type TransformResult struct {
	Position          [3]float64       `json:"position_m"`
	UncertaintyMetres float64          `json:"uncertainty_metres"`
	ValidFrom         time.Time        `json:"valid_from"`
	ValidUntil        time.Time        `json:"valid_until"`
	Path              []world.FrameRef `json:"path"`
	PositionError     *PositionError   `json:"position_error,omitempty"`
}

func frameRefValid(ref world.FrameRef) bool {
	return ref.Validate() == nil && len(ref.ID) <= 256 && len(ref.Revision) <= 256
}

func frameNumberValid(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && math.Abs(x) <= MaxFrameCoordinateMetres
}

func framePointValid(p [3]float64) bool {
	return frameNumberValid(p[0]) && frameNumberValid(p[1]) && frameNumberValid(p[2])
}

// NewGraph requires every named placement to reach the declared root. Duplicate
// child revisions are rejected even when their values match, avoiding ambiguous
// ownership of a placement. Rotations map child coordinates to parent coordinates.
func NewGraph(root world.FrameRef, frames []world.LocalFrame) (*Graph, error) {
	if !frameRefValid(root) || len(frames) > MaxFrameEdges {
		return nil, errors.New("invalid root or too many frame edges")
	}
	g := &Graph{root: root, edges: make(map[world.FrameRef]frameEdge, len(frames))}
	for _, f := range frames {
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("invalid placement %s/%s: %w", f.Ref.ID, f.Ref.Revision, err)
		}
		if !frameRefValid(f.Ref) || !frameRefValid(f.Parent) || !framePointValid(f.Translation) || !frameNumberValid(f.UncertaintyMetres) {
			return nil, errors.New("frame placement exceeds numeric or identifier bounds")
		}
		if f.Ref == root {
			return nil, errors.New("root cannot have a parent")
		}
		if _, exists := g.edges[f.Ref]; exists {
			return nil, errors.New("duplicate child frame revision")
		}
		q := f.RotationXYZW
		// Validate accepts floating-point unit norm tolerance. Normalize within
		// that tolerance so transpose remains the inverse of a rigid rotation.
		norm := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
		for i := range q {
			q[i] /= norm
		}
		x, y, z, w := q[0], q[1], q[2], q[3]
		m := [3][3]float64{
			{1 - 2*(y*y+z*z), 2 * (x*y - z*w), 2 * (x*z + y*w)},
			{2 * (x*y + z*w), 1 - 2*(x*x+z*z), 2 * (y*z - x*w)},
			{2 * (x*z - y*w), 2 * (y*z + x*w), 1 - 2*(x*x+y*y)},
		}
		f.RotationError = copyAngularError(f.RotationError)
		g.edges[f.Ref] = frameEdge{frame: f, rotation: m}
	}
	for ref := range g.edges {
		seen := make(map[world.FrameRef]bool)
		for ref != root {
			if seen[ref] {
				return nil, errors.New("frame graph contains a cycle")
			}
			if len(seen) == MaxFrameDepth {
				return nil, errors.New("frame graph exceeds maximum depth")
			}
			seen[ref] = true
			edge, ok := g.edges[ref]
			if !ok {
				return nil, errors.New("frame parent revision is missing")
			}
			ref = edge.frame.Parent
		}
	}
	return g, nil
}

func (g *Graph) chain(ref world.FrameRef) ([]world.FrameRef, error) {
	if !frameRefValid(ref) {
		return nil, errors.New("invalid frame reference")
	}
	path := make([]world.FrameRef, 0, MaxFrameDepth+1)
	for {
		path = append(path, ref)
		if ref == g.root {
			return path, nil
		}
		edge, ok := g.edges[ref]
		if !ok {
			return nil, errors.New("unknown frame revision")
		}
		ref = edge.frame.Parent
	}
}

func frameApply(p [3]float64, e frameEdge, inverse bool) ([3]float64, error) {
	var out [3]float64
	if inverse {
		for i := range p {
			p[i] -= e.frame.Translation[i]
		}
	}
	for i := range out {
		for j := range p {
			if inverse {
				out[i] += e.rotation[j][i] * p[j]
			} else {
				out[i] += e.rotation[i][j] * p[j]
			}
		}
		if !inverse {
			out[i] += e.frame.Translation[i]
		}
	}
	if !framePointValid(out) {
		return out, errors.New("transformed point exceeds numeric bounds")
	}
	return out, nil
}

// Transform resolves the shortest route between the exact frame revisions.
// Every edge in either frame's ancestry must be valid at at, even if an edge
// cancels from that route. Result bounds intersect those dependencies and sum
// each dependency's translational uncertainty once. Orientation uncertainty and
// input-point uncertainty are not added. Use TransformWithError for a full bound.
func (g *Graph) Transform(position [3]float64, from, to world.FrameRef, at time.Time) (TransformResult, error) {
	if g == nil || !frameRefValid(g.root) || at.IsZero() || !framePointValid(position) {
		return TransformResult{}, errors.New("graph, finite bounded point and explicit time required")
	}
	a, err := g.chain(from)
	if err != nil {
		return TransformResult{}, err
	}
	b, err := g.chain(to)
	if err != nil {
		return TransformResult{}, err
	}
	result := TransformResult{Position: position}
	seen := make(map[world.FrameRef]bool)
	for _, chain := range [][]world.FrameRef{a, b} {
		for _, ref := range chain {
			if ref == g.root || seen[ref] {
				continue
			}
			seen[ref] = true
			e := g.edges[ref].frame
			if at.Before(e.ValidFrom) || !at.Before(e.ValidUntil) {
				return TransformResult{}, fmt.Errorf("frame placement %s/%s is not valid at requested time", ref.ID, ref.Revision)
			}
			if result.ValidFrom.IsZero() || e.ValidFrom.After(result.ValidFrom) {
				result.ValidFrom = e.ValidFrom
			}
			if result.ValidUntil.IsZero() || e.ValidUntil.Before(result.ValidUntil) {
				result.ValidUntil = e.ValidUntil
			}
			result.UncertaintyMetres += e.UncertaintyMetres
			if !frameNumberValid(result.UncertaintyMetres) {
				return TransformResult{}, errors.New("frame uncertainty exceeds numeric bound")
			}
		}
	}
	// Remove the common suffix except its first element, the common ancestor.
	ai, bi := len(a)-1, len(b)-1
	for ai > 0 && bi > 0 && a[ai-1] == b[bi-1] {
		ai--
		bi--
	}
	result.Path = append(result.Path, a[:ai+1]...)
	for j := bi - 1; j >= 0; j-- {
		result.Path = append(result.Path, b[j])
	}
	for j := 0; j < ai; j++ {
		result.Position, err = frameApply(result.Position, g.edges[a[j]], false)
		if err != nil {
			return TransformResult{}, err
		}
	}
	for j := bi - 1; j >= 0; j-- {
		result.Position, err = frameApply(result.Position, g.edges[b[j]], true)
		if err != nil {
			return TransformResult{}, err
		}
	}
	return result, nil
}
