package geography

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PositionError is a conservative Euclidean distance bound in metres. Unknown
// never means zero. These bounds do not establish the accuracy of their inputs.
type PositionError struct {
	Known             bool     `json:"known"`
	MaxDistanceMetres *float64 `json:"max_distance_metres,omitempty"`
}

func (e PositionError) Validate() error {
	if !e.Known {
		if e.MaxDistanceMetres != nil {
			return errors.New("unknown position error cannot carry a bound")
		}
		return nil
	}
	if e.MaxDistanceMetres == nil || !frameNumberValid(*e.MaxDistanceMetres) || *e.MaxDistanceMetres < 0 {
		return errors.New("known position error requires a finite nonnegative bounded distance")
	}
	return nil
}

func knownPositionError(metres float64) PositionError {
	return PositionError{Known: true, MaxDistanceMetres: &metres}
}

func copyAngularError(in *world.AngularError) *world.AngularError {
	if in == nil {
		return nil
	}
	out := *in
	if in.MaxAngleRadians != nil {
		x := *in.MaxAngleRadians
		out.MaxAngleRadians = &x
	}
	return &out
}

// placementError uses the largest chord under the declared angular radius.
// The incoming error ball is unchanged by an arbitrary rigid rotation. The
// inverse lever is measured from the nominal translation, before rotation.
func placementError(input PositionError, lever [3]float64, translation float64, rotation *world.AngularError) (PositionError, error) {
	if err := input.Validate(); err != nil {
		return PositionError{}, err
	}
	if !frameNumberValid(translation) || translation < 0 {
		return PositionError{}, errors.New("translation uncertainty exceeds numeric bound")
	}
	radius := math.Hypot(math.Hypot(lever[0], lever[1]), lever[2])
	if math.IsNaN(radius) || math.IsInf(radius, 0) {
		return PositionError{}, errors.New("orientation lever exceeds numeric bound")
	}
	if rotation != nil {
		if err := rotation.Validate(); err != nil {
			return PositionError{}, err
		}
	}
	angleKnown := rotation != nil && rotation.Known
	angular := 0.0
	if angleKnown {
		angular = 2 * radius * math.Sin(*rotation.MaxAngleRadians/2)
	}
	contribution := translation + angular
	if !frameNumberValid(contribution) {
		return PositionError{}, errors.New("placement uncertainty exceeds numeric bound")
	}
	if !input.Known || (!angleKnown && radius != 0) {
		return PositionError{Known: false}, nil
	}
	out := knownPositionError(*input.MaxDistanceMetres + contribution)
	if err := out.Validate(); err != nil {
		return PositionError{}, err
	}
	return out, nil
}

// TransformWithError retains Transform's nominal result and all ancestry
// validity requirements, but composes full errors only along the actual local
// route. Shared ancestor placements cancel. A missing rotation bound affects
// this result only when its edge is traversed with a nonzero lever arm.
func (g *Graph) TransformWithError(position [3]float64, from, to world.FrameRef, at time.Time, inputError PositionError) (TransformResult, error) {
	if err := inputError.Validate(); err != nil {
		return TransformResult{}, err
	}
	r, err := g.Transform(position, from, to, at)
	if err != nil {
		return TransformResult{}, err
	}
	bound := inputError
	if inputError.MaxDistanceMetres != nil {
		bound = knownPositionError(*inputError.MaxDistanceMetres)
	}
	p := position
	for i := 0; i+1 < len(r.Path); i++ {
		current, next := r.Path[i], r.Path[i+1]
		e, forward := g.edges[current]
		forward = forward && e.frame.Parent == next
		if !forward {
			e = g.edges[next]
		}
		lever := p
		if !forward {
			for j := range lever {
				lever[j] -= e.frame.Translation[j]
			}
		}
		bound, err = placementError(bound, lever, e.frame.UncertaintyMetres, e.frame.RotationError)
		if err != nil {
			return TransformResult{}, err
		}
		p, err = frameApply(p, e, !forward)
		if err != nil {
			return TransformResult{}, err
		}
	}
	r.PositionError = &bound
	return r, nil
}
