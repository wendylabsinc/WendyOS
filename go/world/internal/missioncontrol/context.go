// Package missioncontrol defines the immutable, commissioned policy for native
// simulation missions. A context never grants operator authority by itself.
package missioncontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const InspectType = "wendy.nav.inspect.v1"

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Bounds struct {
	Min [3]float64 `json:"min"`
	Max [3]float64 `json:"max"`
}
type Context struct {
	Version         int                 `json:"version"`
	Tenant          string              `json:"tenant"`
	Site            string              `json:"site"`
	Audience        string              `json:"audience"`
	RunID           string              `json:"run_id"`
	PackageSHA256   string              `json:"package_sha256"`
	Assignee        string              `json:"assignee"`
	Frame           world.FrameRef      `json:"frame"`
	Type            string              `json:"type"`
	Class           mission.SafetyClass `json:"class"`
	Mode            mission.Mode        `json:"mode"`
	Area            Bounds              `json:"area"`
	RequiredSensors []string            `json:"required_sensors"`
	MinimumEnergy   float64             `json:"minimum_energy"`
	Fallback        mission.Action      `json:"fallback"`
	SpeedMPS        float64             `json:"speed_mps"`
	ToleranceM      float64             `json:"tolerance_m"`
	StopSpeedMPS    float64             `json:"stop_speed_mps"`
	MaxDurationMS   int64               `json:"max_duration_ms"`
	NotBeforeUnixMS int64               `json:"not_before_unix_ms"`
	NotAfterUnixMS  int64               `json:"not_after_unix_ms"`
}
type SignedContext struct {
	Context   Context `json:"context"`
	Signature []byte  `json:"signature"`
}
type ProposalRef struct {
	Audience   string `json:"audience"`
	ID         string `json:"id"`
	BodySHA256 string `json:"body_sha256"`
}

func objectFields(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("expected control object")
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid control field")
		}
		if _, exists := out[key]; exists {
			return nil, errors.New("duplicate control field")
		}
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid control object end")
	}
	return out, nil
}
func (s *SignedContext) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data)
	if err != nil {
		return err
	}
	if len(fields) != 2 || len(fields["context"]) == 0 || len(fields["signature"]) == 0 {
		return errors.New("invalid signed control context")
	}
	if err = json.Unmarshal(fields["context"], &s.Context); err != nil {
		return err
	}
	if err = json.Unmarshal(fields["signature"], &s.Signature); err != nil {
		return err
	}
	return nil
}

// Required numeric fields and fixed-size vectors must not normalize missing
// values, JSON null or extra coordinates into a different signed policy.
func (c *Context) UnmarshalJSON(data []byte) error {
	type wire Context
	var value wire
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	fields, err := objectFields(data)
	if err != nil {
		return errors.New("invalid control context object")
	}
	canonical, _ := json.Marshal(value)
	var required map[string]json.RawMessage
	_ = json.Unmarshal(canonical, &required)
	if len(fields) != len(required) {
		return errors.New("missing control context field")
	}
	for key := range required {
		if raw, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("missing or null control context field")
		}
	}
	*c = Context(value)
	return nil
}
func (b *Bounds) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data)
	if err != nil || len(fields) != 2 {
		return errors.New("invalid control bounds")
	}
	for key, target := range map[string]*[3]float64{"min": &b.Min, "max": &b.Max} {
		raw, ok := fields[key]
		if !ok {
			return errors.New("missing control bounds")
		}
		*target, err = DecodeTarget(append(append([]byte(`{"target":`), raw...), '}'))
		if err != nil {
			return err
		}
	}
	return nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func (c Context) Validate() error {
	for _, s := range []string{c.Tenant, c.Site, c.Audience, c.RunID, c.Assignee} {
		if s == "" || len(s) > 256 {
			return errors.New("invalid mission control scope")
		}
	}
	if c.Version != 1 || !hashPattern.MatchString(c.PackageSHA256) || c.Frame.Validate() != nil || c.Type != InspectType || c.Class != mission.Move || c.Mode != mission.Autonomous || c.Fallback != mission.Hold {
		return errors.New("unsupported mission control context")
	}
	for i := 0; i < 3; i++ {
		if !finite(c.Area.Min[i]) || !finite(c.Area.Max[i]) || c.Area.Min[i] >= c.Area.Max[i] {
			return errors.New("invalid mission control area")
		}
	}
	if len(c.RequiredSensors) == 0 || len(c.RequiredSensors) > 32 {
		return errors.New("invalid mission control sensors")
	}
	seen := map[string]bool{}
	for _, sensor := range c.RequiredSensors {
		if sensor == "" || len(sensor) > 256 || seen[sensor] {
			return errors.New("invalid mission control sensor")
		}
		seen[sensor] = true
	}
	if !finite(c.MinimumEnergy) || c.MinimumEnergy < 0 || c.MinimumEnergy > 1 || !finite(c.SpeedMPS) || c.SpeedMPS <= 0 || c.SpeedMPS > .5 || !finite(c.ToleranceM) || c.ToleranceM <= 0 || c.ToleranceM > 10 || !finite(c.StopSpeedMPS) || c.StopSpeedMPS < 0 || c.StopSpeedMPS > c.SpeedMPS {
		return errors.New("invalid mission control limits")
	}
	if c.NotBeforeUnixMS <= 0 || c.NotAfterUnixMS > 253402214400000 || c.NotAfterUnixMS <= c.NotBeforeUnixMS || c.NotAfterUnixMS-c.NotBeforeUnixMS > (365*24*time.Hour).Milliseconds() || c.MaxDurationMS <= 0 || c.MaxDurationMS > (24*time.Hour).Milliseconds() || c.MaxDurationMS > c.NotAfterUnixMS-c.NotBeforeUnixMS {
		return errors.New("invalid mission control lifetime")
	}
	return nil
}
func (p ProposalRef) Validate() error {
	if p.Audience == "" || len(p.Audience) > 256 || !idPattern.MatchString(p.ID) || !hashPattern.MatchString(p.BodySHA256) {
		return errors.New("invalid mission proposal reference")
	}
	return nil
}
func Digest(s SignedContext) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Equal(a, b SignedContext) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func Clone(s SignedContext) SignedContext {
	s.Signature = append([]byte(nil), s.Signature...)
	s.Context.RequiredSensors = append([]string(nil), s.Context.RequiredSensors...)
	return s
}

// DecodeTarget is deliberately narrower than arbitrary proposal parameters.
// JSON null and short arrays must not silently become zero-valued coordinates.
func DecodeTarget(data []byte) ([3]float64, error) {
	var out [3]float64
	d := json.NewDecoder(bytes.NewReader(data))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return out, errors.New("inspect parameters must be an object")
	}
	key, err := d.Token()
	if err != nil || key != "target" {
		return out, errors.New("inspect parameters must contain only target")
	}
	var values []json.RawMessage
	if d.Decode(&values) != nil || len(values) != 3 {
		return out, errors.New("inspect target must have three numbers")
	}
	closing, err := d.Token()
	if err != nil || closing != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return out, errors.New("inspect parameters must contain only target")
	}
	for i, v := range values {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &out[i]) != nil || !finite(out[i]) {
			return out, errors.New("invalid inspect coordinate")
		}
	}
	return out, nil
}
