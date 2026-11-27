package executor

import (
	"context"
	"errors"
	"math"
	"time"
)

// PlanarHTTPAdapter uses separate endpoints and vector commands. It cannot
// silently send a planar task to the scalar one-axis actuator.
type PlanarHTTPAdapter struct{ read, control *HTTPAdapter }

func NewPlanarHTTPAdapter(address, readToken, controlToken, robot string, floorY float64) (*PlanarHTTPAdapter, error) {
	if robot == "" || len(robot) > 128 || !finitePlanar(floorY) {
		return nil, errors.New("planar robot and floor required")
	}
	read, err := NewHTTPAdapter(address, readToken)
	if err != nil {
		return nil, err
	}
	control, err := NewHTTPAdapter(address, controlToken)
	if err != nil {
		return nil, err
	}
	decode := func(raw []byte) (State, error) {
		s, err := decodeStateFields(raw, true)
		if err != nil {
			return State{}, err
		}
		return s, validatePlanarState(s, robot, floorY)
	}
	read.decode, control.decode = decode, decode
	return &PlanarHTTPAdapter{read: read, control: control}, nil
}

func (a *PlanarHTTPAdapter) Session(ctx context.Context) (State, error) {
	return a.control.call(ctx, "POST", "/v1/inspection/session", map[string]any{})
}
func (a *PlanarHTTPAdapter) State(ctx context.Context) (State, error) {
	return a.read.call(ctx, "GET", "/v1/inspection/state", nil)
}
func (a *PlanarHTTPAdapter) Stop(ctx context.Context, session string, sequence uint64) error {
	_, err := a.control.call(ctx, "POST", "/v1/inspection/stop", map[string]any{"session": session, "sequence": sequence})
	return err
}
func (a *PlanarHTTPAdapter) Command(context.Context, string, uint64, float64, time.Duration) error {
	return errors.New("scalar command unavailable on planar adapter")
}
func (a *PlanarHTTPAdapter) CommandPlanar(ctx context.Context, session string, sequence uint64, v [3]float64, lease time.Duration) error {
	if session == "" || sequence == 0 || sequence > maxSequence || lease < time.Millisecond || lease > 500*time.Millisecond || v[1] != 0 || !finitePlanar(v[0]) || !finitePlanar(v[2]) || math.Hypot(v[0], v[2]) > .5 {
		return errors.New("invalid planar command")
	}
	_, err := a.control.call(ctx, "POST", "/v1/inspection/command", map[string]any{"session": session, "sequence": sequence, "velocity_mps": v, "lease_ms": lease.Milliseconds()})
	return err
}
