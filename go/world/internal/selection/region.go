package selection

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Region is a read interest in one exact right-handed Y-up frame. It is not
// storage membership, navigation clearance or a coordinate migration.
type Region struct {
	Frame world.FrameRef `json:"frame"`
	MinXZ [2]float64     `json:"min_xz"`
	MaxXZ [2]float64     `json:"max_xz"`
}

func (r *Region) UnmarshalJSON(raw []byte) error {
	var v struct {
		Frame *world.FrameRef   `json:"frame"`
		Min   []json.RawMessage `json:"min_xz"`
		Max   []json.RawMessage `json:"max_xz"`
	}
	if authority.DecodeStrict(raw, &v) != nil || v.Frame == nil || len(v.Min) != 2 || len(v.Max) != 2 {
		return errors.New("region needs explicit frame and two complete corners")
	}
	var out Region
	out.Frame = *v.Frame
	for i := 0; i < 2; i++ {
		if bytes.Equal(v.Min[i], []byte("null")) || bytes.Equal(v.Max[i], []byte("null")) || json.Unmarshal(v.Min[i], &out.MinXZ[i]) != nil || json.Unmarshal(v.Max[i], &out.MaxXZ[i]) != nil {
			return errors.New("invalid region corner")
		}
	}
	if err := out.Validate(); err != nil {
		return err
	}
	*r = out
	return nil
}
func (r Region) Validate() error {
	if !bounded(r.Frame.ID) || !bounded(r.Frame.Revision) {
		return errors.New("region requires exact bounded frame revision")
	}
	for i := 0; i < 2; i++ {
		if !coordinate(r.MinXZ[i]) || !coordinate(r.MaxXZ[i]) || r.MinXZ[i] >= r.MaxXZ[i] {
			return errors.New("region requires finite ordered corners within one million metres")
		}
	}
	return nil
}
func coordinate(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= 1e6 }
func SpatialType(t string) bool {
	return t == "wendy.sensor.v1" || t == "wendy.hazard.v1" || t == annotations.Type || t == annotations.TypeV2
}

// Spatial is unsigned query metadata, independently derived from the original
// payload. ExpiresAt is the node's original retention/authority cap, not renewal.
type Spatial struct {
	ContentSHA256 string    `json:"content_sha256"`
	Key           string    `json:"key"`
	Class         string    `json:"class"`
	Reason        string    `json:"reason"`
	PositionM     []float64 `json:"position_m,omitempty"`
	UncertaintyM  *float64  `json:"uncertainty_m,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (r Region) Classify(c world.Component) Spatial {
	d := Spatial{ContentSHA256: Digest(c), Key: Key(c), Class: "unlocated", Reason: "missing or invalid declared spatial bounds"}
	if c.Frame != r.Frame || !SpatialType(c.Key.Type) {
		d.Class = "outside"
		d.Reason = "incompatible frame or unsupported type"
		return d
	}
	var p []float64
	radius := 0.0
	if c.Key.Type == annotations.Type || c.Key.Type == annotations.TypeV2 {
		n, err := annotations.Validate(c)
		if err != nil {
			return d
		}
		p = []float64{float64(n.PositionMM[0]) / 1000, float64(n.PositionMM[1]) / 1000, float64(n.PositionMM[2]) / 1000}
		d.Reason = "authored point; not measured accuracy"
	} else {
		var obj map[string]json.RawMessage
		if authority.DecodeStrictLimit(c.Payload, &obj, 64<<10) != nil {
			return d
		}
		var positions []json.RawMessage
		if json.Unmarshal(obj["position_m"], &positions) != nil || len(positions) != 3 || len(obj["uncertainty_m"]) == 0 || bytes.Equal(obj["uncertainty_m"], []byte("null")) || json.Unmarshal(obj["uncertainty_m"], &radius) != nil || !coordinate(radius) || radius < 0 {
			return d
		}
		for _, raw := range positions {
			var v float64
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &v) != nil || !coordinate(v) {
				return d
			}
			p = append(p, v)
		}
		if c.Key.Type == "wendy.sensor.v1" {
			if _, err := sensing.FromComponent(c); err != nil {
				return d
			}
		}
		d.Reason = "declared uncertainty intersects area; not independently measured"
	}
	d.PositionM = p
	d.UncertaintyM = &radius
	d.Class = "located"
	// A rectangular expansion conservatively contains the projected uncertainty
	// circle. Corner false positives are intentional; no possible intersection is dropped.
	if p[0]+radius < r.MinXZ[0] || p[0]-radius > r.MaxXZ[0] || p[2]+radius < r.MinXZ[1] || p[2]-radius > r.MaxXZ[1] {
		d.Class = "outside"
		d.Reason = "declared bounds outside area"
	}
	return d
}

// An explicitly null region is an invalid spatial request, not an all-world read.
func (s *Selection) UnmarshalJSON(raw []byte) error {
	type alias Selection
	var fields map[string]json.RawMessage
	if authority.DecodeStrictLimit(raw, &fields, MaxRequestBytes) != nil {
		return errors.New("invalid selection")
	}
	if v, ok := fields["region"]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return errors.New("null region is not an unfiltered selection")
	}
	var out alias
	if authority.DecodeStrictLimit(raw, &out, MaxRequestBytes) != nil {
		return errors.New("invalid selection fields")
	}
	*s = Selection(out)
	return nil
}
