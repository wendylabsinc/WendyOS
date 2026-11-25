// Package patroltour defines the immutable finite inspection plan and retained
// original-capture receipts. It provides no authority or motion transport.
package patroltour

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/strictjson"
	"math"
	"regexp"
)

const Method = "inspect-selected-rooms-once-and-return-v1"

// ObservationHeight is the commissioned nominal Y-up height for this planar
// navigation adapter. It cannot authorize a different vertical observation.
const ObservationHeight = 0.3

const NavigationMode = "fixed-position-yaw-v1"

type Stop struct {
	RoomID   string    `json:"room_id"`
	Position []float64 `json:"position"`
	Yaw      float64   `json:"yaw"`
}

func (s *Stop) UnmarshalJSON(raw []byte) error {
	var v struct {
		RoomID   *string    `json:"room_id"`
		Position []*float64 `json:"position"`
		Yaw      *float64   `json:"yaw"`
	}
	if err := strictjson.DecodeLimit(raw, &v, 4096); err != nil {
		return err
	}
	if v.RoomID == nil || v.Yaw == nil || len(v.Position) != 3 {
		return errors.New("tour stop requires room, three explicit coordinates and yaw")
	}
	p := make([]float64, 3)
	for i, n := range v.Position {
		if n == nil {
			return errors.New("tour coordinates cannot be null")
		}
		p[i] = *n
	}
	*s = Stop{*v.RoomID, p, *v.Yaw}
	return nil
}

type Policy struct {
	Method string `json:"method"`
	Stops  []Stop `json:"stops"`
}

func (p *Policy) UnmarshalJSON(raw []byte) error {
	type plain Policy
	var v plain
	if err := strictjson.DecodeLimit(raw, &v, 16384); err != nil {
		return err
	}
	*p = Policy(v)
	return nil
}

var token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (p Policy) Validate() error {
	if p.Method != Method || len(p.Stops) < 1 || len(p.Stops) > 8 {
		return errors.New("tour requires one to eight fixed inspection stops")
	}
	seen := map[string]bool{}
	for _, s := range p.Stops {
		if !token.MatchString(s.RoomID) || seen[s.RoomID] || len(s.Position) != 3 || s.Position[1] != ObservationHeight || math.IsNaN(s.Yaw) || math.IsInf(s.Yaw, 0) || math.Abs(s.Yaw) > math.Pi {
			return errors.New("invalid or repeated tour stop")
		}
		seen[s.RoomID] = true
		for _, v := range s.Position {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("non-finite tour destination")
			}
		}
	}
	return nil
}

type Visit struct {
	RoomID         string   `json:"room_id"`
	SimulationNS   int64    `json:"sample_simulation_ns"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Evidence       []string `json:"evidence"`
}
type Evidence struct {
	Visits []Visit `json:"visits"`
}

func (p Policy) Complete(e *Evidence, beforeNS int64) bool {
	if p.Validate() != nil || e == nil || len(e.Visits) != len(p.Stops) {
		return false
	}
	var last int64
	seen := map[string]bool{}
	for i, v := range e.Visits {
		if v.RoomID != p.Stops[i].RoomID || v.SimulationNS <= last || v.SimulationNS > beforeNS || !digest.MatchString(v.ManifestSHA256) || seen[v.ManifestSHA256] || len(v.Evidence) < 4 || len(v.Evidence) > 8 {
			return false
		}
		refs := map[string]bool{}
		for _, d := range v.Evidence {
			if !digest.MatchString(d) || refs[d] {
				return false
			}
			refs[d] = true
		}
		if !refs[v.ManifestSHA256] {
			return false
		}
		seen[v.ManifestSHA256] = true
		last = v.SimulationNS
	}
	return true
}
