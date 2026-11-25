package tasking

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PatrolExclusions are operator restrictions, not sensor observations. Coordinates
// are integer millimetres in navigation XY, Z-up: World (X,Y,Z) maps to (X,-Z).
type PatrolExclusions struct {
	Frame      world.FrameRef    `json:"frame"`
	Revision   int               `json:"revision"`
	Rectangles []PatrolRectangle `json:"rectangles"`
	SHA256     string            `json:"sha256"`
}
type PatrolRectangle struct {
	ID    string `json:"id"`
	MinMM []int  `json:"min_mm"`
	MaxMM []int  `json:"max_mm"`
}

// Unknown coordinates must not become a usable zero-valued corner. Keep this
// decoder strict even when the enclosing caller also rejects unknown fields.
func (r *PatrolRectangle) UnmarshalJSON(raw []byte) error {
	var wire struct {
		ID  string          `json:"id"`
		Min json.RawMessage `json:"min_mm"`
		Max json.RawMessage `json:"max_mm"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("one restricted rectangle object required")
	}
	corner := func(raw json.RawMessage) ([]int, error) {
		var pair []*int
		if err := json.Unmarshal(raw, &pair); err != nil {
			return nil, err
		}
		if len(pair) != 2 || pair[0] == nil || pair[1] == nil {
			return nil, errors.New("restricted corner requires exactly two non-null integer millimetre coordinates")
		}
		return []int{*pair[0], *pair[1]}, nil
	}
	low, err := corner(wire.Min)
	if err != nil {
		return err
	}
	high, err := corner(wire.Max)
	if err != nil {
		return err
	}
	*r = PatrolRectangle{ID: wire.ID, MinMM: low, MaxMM: high}
	return nil
}

// The configured Go2 footprint is 0.90 x 0.58 m with 0.025 m padding.
// Its circumscribed radius plus a 0.20 m simulation tracking allowance bounds
// all headings. This is not a hardware clearance or stopping certification.
func PatrolExclusionRadius() float64 { return math.Hypot(.45+.025, .29+.025) + .20 }

func (p *PatrolExclusions) Digest() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "wendy.patrol.exclusions.v1\nnav-xy-z-up\n%s\n%s\n%d\n", p.Frame.ID, p.Frame.Revision, p.Revision)
	for _, r := range p.Rectangles {
		if len(r.MinMM) != 2 || len(r.MaxMM) != 2 {
			return ""
		}
		fmt.Fprintf(&b, "%s:%d,%d:%d,%d\n", r.ID, r.MinMM[0], r.MinMM[1], r.MaxMM[0], r.MaxMM[1])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
func (p *PatrolExclusions) Validate(frame world.FrameRef, area Bounds) error {
	if p == nil {
		return nil
	}
	if p.Frame != frame || !go2Token.MatchString(p.Frame.ID) || !go2Token.MatchString(p.Frame.Revision) || p.Revision != 1 || len(p.Rectangles) == 0 || len(p.Rectangles) > 16 {
		return errors.New("restricted areas require matching frame, revision 1 and 1..16 rectangles")
	}
	ids := map[string]bool{}
	for _, r := range p.Rectangles {
		if !go2Token.MatchString(r.ID) || ids[r.ID] || len(r.MinMM) != 2 || len(r.MaxMM) != 2 {
			return errors.New("restricted areas require unique IDs and both two-coordinate corners")
		}
		ids[r.ID] = true
		if r.MinMM[0] >= r.MaxMM[0] || r.MinMM[1] >= r.MaxMM[1] || float64(r.MinMM[0])/1000 < area.Min[0] || float64(r.MaxMM[0])/1000 > area.Max[0] || float64(r.MinMM[1])/1000 < -area.Max[2] || float64(r.MaxMM[1])/1000 > -area.Min[2] {
			return errors.New("restricted rectangle must be nonempty and inside mission area")
		}
	}
	if p.SHA256 != p.Digest() {
		return errors.New("restricted area digest mismatch")
	}
	return nil
}
func (p *PatrolExclusions) Clear(x, y float64) bool {
	if p == nil {
		return true
	}
	if !finite(x) || !finite(y) {
		return false
	}
	for _, r := range p.Rectangles {
		if len(r.MinMM) != 2 || len(r.MaxMM) != 2 {
			return false
		}
		dx := math.Max(math.Max(float64(r.MinMM[0])/1000-x, 0), x-float64(r.MaxMM[0])/1000)
		dy := math.Max(math.Max(float64(r.MinMM[1])/1000-y, 0), y-float64(r.MaxMM[1])/1000)
		if math.Hypot(dx, dy) <= PatrolExclusionRadius() {
			return false
		}
	}
	return true
}
func patrolPositionClear(s Spec, t Telemetry) bool {
	return s.Patrol == nil || s.Patrol.Exclusions.Clear(t.Position[0], -t.Position[2])
}
