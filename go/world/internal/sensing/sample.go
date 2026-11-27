// Package sensing validates the synthetic multimodal adapter contract. It does
// not grant publishing authority; the node authenticates the outer component.
package sensing

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Schema = "wendy.sensor.sample.v1"
const ComponentType = "wendy.sensor.v1"
const MaxSampleBytes = 65536
const MaxEvidenceBytes = 8 << 20
const MaxLidarPoints = 1024

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type EvidenceRef struct {
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}
type Camera struct {
	Width                 int       `json:"width"`
	Height                int       `json:"height"`
	FX                    float64   `json:"fx"`
	FY                    float64   `json:"fy"`
	CX                    float64   `json:"cx"`
	CY                    float64   `json:"cy"`
	CameraToSite          []float64 `json:"camera_to_site"`
	CalibrationValidForNS int64     `json:"calibration_valid_for_ns"`
}
type Gas struct {
	ValuePPM       float64 `json:"value_ppm"`
	UncertaintyPPM float64 `json:"uncertainty_ppm"`
	Species        string  `json:"species"`
}
type Audio struct {
	SampleRateHz int   `json:"sample_rate_hz"`
	Channels     int   `json:"channels"`
	DurationNS   int64 `json:"duration_ns"`
}

// Lidar points are already in the sample's site frame, using World Y-up axes.
// An empty, non-nil cloud is a scan with no returns, not missing metadata.
type Lidar struct {
	PointsM   [][3]float64 `json:"points_m"`
	MaxRangeM float64      `json:"max_range_m"`
}

// LidarSensorID identifies optional LiDAR sources in the live scene contract.
func LidarSensorID(id string) bool {
	return len(id) > len("lidar-") && len(id) <= 80 && id[:len("lidar-")] == "lidar-" && identifier.MatchString(id)
}

// SensorLink preserves the source wire timestamp and bytes. The current bridge
// maps an explicitly configured Unix-microsecond simulation source, never receipt time.
type SensorLink struct {
	Protocol      string `json:"protocol"`
	ChannelID     uint32 `json:"channel_id"`
	FrameSequence uint32 `json:"frame_sequence"`
	TimestampUS   uint64 `json:"timestamp_us"`
	Flags         uint32 `json:"flags"`
	FrameSHA256   string `json:"frame_sha256"`
	SourceClock   string `json:"source_clock"`
	AnchorUnixUS  int64  `json:"anchor_unix_us"`
	ClockErrorUS  uint32 `json:"clock_error_us"`
}

type Sample struct {
	SchemaVersion       string         `json:"schema_version"`
	ID                  string         `json:"id"`
	SiteID              string         `json:"site_id"`
	Frame               world.FrameRef `json:"frame"`
	SensorID            string         `json:"sensor_id"`
	Kind                string         `json:"kind"`
	Sequence            uint64         `json:"sequence"`
	SampleNS            int64          `json:"sample_ns"`
	RunID               string         `json:"run_id"`
	ScenarioVersion     string         `json:"scenario_version"`
	Seed                uint64         `json:"seed"`
	PackageSHA256       string         `json:"package_sha256"`
	SensorModel         string         `json:"sensor_model"`
	PositionM           []float64      `json:"position_m"`
	UncertaintyM        float64        `json:"uncertainty_m"`
	CalibrationRevision string         `json:"calibration_revision"`
	Camera              *Camera        `json:"camera,omitempty"`
	Gas                 *Gas           `json:"gas,omitempty"`
	Audio               *Audio         `json:"audio,omitempty"`
	Lidar               *Lidar         `json:"lidar,omitempty"`
	Evidence            *EvidenceRef   `json:"evidence,omitempty"`
	SensorLink          *SensorLink    `json:"sensorlink,omitempty"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func DigestValid(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func (e EvidenceRef) Validate() error {
	if !DigestValid(e.SHA256) || e.Size < 1 || e.Size > MaxEvidenceBytes || (e.ContentType != "image/png" && e.ContentType != "image/jpeg" && e.ContentType != "audio/wav") {
		return errors.New("invalid evidence reference")
	}
	return nil
}
func (s Sample) Validate() error {
	if s.SchemaVersion != Schema || !identifier.MatchString(s.RunID) || !identifier.MatchString(s.SensorID) || s.Sequence == 0 || s.ID != fmt.Sprintf("%s/%s/%d", s.RunID, s.SensorID, s.Sequence) || s.SiteID == "" || s.ScenarioVersion == "" || len(s.ScenarioVersion) > 256 || s.SensorModel == "" || len(s.SensorModel) > 256 || s.CalibrationRevision == "" || len(s.CalibrationRevision) > 256 || !DigestValid(s.PackageSHA256) || s.SampleNS < 0 || s.SampleNS > int64(24*time.Hour) || s.Seed >= 1<<32 {
		return errors.New("invalid sample identity, timing or lineage")
	}
	if err := s.Frame.Validate(); err != nil {
		return err
	}
	if len(s.PositionM) != 3 || !finite(s.UncertaintyM) || s.UncertaintyM < 0 {
		return errors.New("invalid sample position or uncertainty")
	}
	for _, v := range s.PositionM {
		if !finite(v) {
			return errors.New("nonfinite position")
		}
	}
	switch s.Kind {
	case "camera":
		if s.Camera == nil || s.Gas != nil || s.Audio != nil || s.Lidar != nil || s.Evidence == nil || (s.Evidence.ContentType != "image/png" && s.Evidence.ContentType != "image/jpeg") {
			return errors.New("camera requires only camera metadata and PNG/JPEG evidence")
		}
		c := s.Camera
		if c.Width < 1 || c.Width > 1024 || c.Height < 1 || c.Height > 1024 || !finite(c.FX) || !finite(c.FY) || !finite(c.CX) || !finite(c.CY) || c.FX <= 0 || c.FY <= 0 || c.CX < 0 || c.CX > float64(c.Width) || c.CY < 0 || c.CY > float64(c.Height) || c.CalibrationValidForNS <= 0 || c.CalibrationValidForNS > int64(24*time.Hour) || len(c.CameraToSite) != 16 {
			return errors.New("invalid camera calibration")
		}
		m := c.CameraToSite
		for _, v := range m {
			if !finite(v) {
				return errors.New("nonfinite camera transform")
			}
		}
		for i := 0; i < 3; i++ {
			if math.Abs(m[i*4+3]-s.PositionM[i]) > 1e-6 {
				return errors.New("camera pose disagrees with sensor position")
			}
		}
		for i, want := range []float64{0, 0, 0, 1} {
			if math.Abs(m[12+i]-want) > 1e-9 {
				return errors.New("invalid homogeneous transform")
			}
		}
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				dot := 0.0
				for k := 0; k < 3; k++ {
					dot += m[i*4+k] * m[j*4+k]
				}
				want := 0.0
				if i == j {
					want = 1
				}
				if math.Abs(dot-want) > 1e-6 {
					return errors.New("camera rotation must be orthonormal")
				}
			}
		}
		det := m[0]*(m[5]*m[10]-m[6]*m[9]) - m[1]*(m[4]*m[10]-m[6]*m[8]) + m[2]*(m[4]*m[9]-m[5]*m[8])
		if math.Abs(det-1) > 1e-6 {
			return errors.New("camera transform must preserve handedness")
		}
	case "gas":
		if s.Gas == nil || s.Camera != nil || s.Audio != nil || s.Lidar != nil || s.Evidence != nil {
			return errors.New("gas requires scalar metadata only")
		}
		g := s.Gas
		if !finite(g.ValuePPM) || !finite(g.UncertaintyPPM) || g.ValuePPM < 0 || g.UncertaintyPPM < 0 || g.Species == "" || len(g.Species) > 128 {
			return errors.New("invalid gas measurement")
		}
	case "audio":
		if s.Audio == nil || s.Camera != nil || s.Gas != nil || s.Lidar != nil || s.Evidence == nil || s.Evidence.ContentType != "audio/wav" {
			return errors.New("audio requires only audio metadata and WAV evidence")
		}
		a := s.Audio
		if a.SampleRateHz < 2000 || a.SampleRateHz > 48000 || a.Channels != 1 || a.DurationNS <= 0 || a.DurationNS > int64(10*time.Second) || s.SampleNS < a.DurationNS {
			return errors.New("invalid audio metadata or capture interval")
		}
	case "lidar":
		if s.Lidar == nil || s.Camera != nil || s.Gas != nil || s.Audio != nil || s.Evidence != nil || s.SensorLink != nil {
			return errors.New("lidar requires inline point metadata only")
		}
		cloud := s.Lidar
		if cloud.PointsM == nil || len(cloud.PointsM) > MaxLidarPoints || !finite(cloud.MaxRangeM) || cloud.MaxRangeM <= 0 {
			return errors.New("invalid lidar cloud or range")
		}
		for _, point := range cloud.PointsM {
			for _, value := range point {
				if !finite(value) {
					return errors.New("nonfinite lidar point")
				}
			}
		}
		if raw, err := json.Marshal(s); err != nil || len(raw) > MaxSampleBytes {
			return errors.New("sample too large")
		}
	default:
		return errors.New("unsupported sensor kind")
	}
	if s.SensorLink != nil {
		link := s.SensorLink
		if s.Kind != "camera" || s.Evidence == nil || s.Evidence.ContentType != "image/jpeg" || link.Protocol != "wendylite.v1.sensorlink" || link.ChannelID == 0 || link.TimestampUS == 0 || link.TimestampUS > uint64(math.MaxInt64/1000) || link.AnchorUnixUS <= 0 || link.AnchorUnixUS > math.MaxInt64/1000 || link.SourceClock != "unix-us" || link.ClockErrorUS == 0 || link.ClockErrorUS > 100000 || link.Flags & ^uint32(1) != 0 || !DigestValid(link.FrameSHA256) || link.FrameSHA256 != s.Evidence.SHA256 {
			return errors.New("invalid SensorLink camera lineage or clock mapping")
		}
		delta := int64(link.TimestampUS) - link.AnchorUnixUS
		if delta < 0 || delta > int64(24*time.Hour/time.Microsecond) || delta*1000 != s.SampleNS {
			return errors.New("SensorLink capture time disagrees with simulation timeline")
		}
	}
	if s.Evidence != nil {
		return s.Evidence.Validate()
	}
	return nil
}

// Decode rejects duplicate and unknown fields, missing numeric metadata and
// shortened vectors, which encoding/json would otherwise silently zero-fill.
func Decode(raw []byte) (Sample, error) {
	var s Sample
	if len(raw) > MaxSampleBytes {
		return s, errors.New("sample too large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if e := unique(d, 0); e != nil {
		return s, e
	}
	if _, e := d.Token(); e != io.EOF {
		return s, errors.New("trailing JSON")
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return s, e
	}
	required := []string{"schema_version", "id", "site_id", "frame", "sensor_id", "kind", "sequence", "sample_ns", "run_id", "scenario_version", "seed", "package_sha256", "sensor_model", "position_m", "uncertainty_m", "calibration_revision"}
	if e := present(fields, required...); e != nil {
		return s, e
	}
	for name := range fields {
		if name != "lidar" && strings.EqualFold(name, "lidar") {
			return s, errors.New("lidar field must use its exact name")
		}
	}
	for name, keys := range map[string][]string{"camera": {"width", "height", "fx", "fy", "cx", "cy", "camera_to_site", "calibration_valid_for_ns"}, "gas": {"value_ppm", "uncertainty_ppm", "species"}, "audio": {"sample_rate_hz", "channels", "duration_ns"}, "lidar": {"points_m", "max_range_m"}, "evidence": {"sha256", "content_type", "size"}, "frame": {"id", "revision"}, "sensorlink": {"protocol", "channel_id", "frame_sequence", "timestamp_us", "flags", "frame_sha256", "source_clock", "anchor_unix_us", "clock_error_us"}} {
		if b, ok := fields[name]; ok {
			var nested map[string]json.RawMessage
			if e := json.Unmarshal(b, &nested); e != nil {
				return s, e
			}
			if e := present(nested, keys...); e != nil {
				return s, fmt.Errorf("%s: %w", name, e)
			}
			if name == "lidar" {
				if len(nested) != len(keys) {
					return s, errors.New("unknown lidar metadata field")
				}
				var points []json.RawMessage
				if e := json.Unmarshal(nested["points_m"], &points); e != nil || len(points) > MaxLidarPoints {
					return s, errors.New("invalid bounded lidar cloud")
				}
				for _, point := range points {
					var coordinates []json.RawMessage
					if e := json.Unmarshal(point, &coordinates); e != nil || len(coordinates) != 3 {
						return s, errors.New("lidar points require exactly three coordinates")
					}
				}
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&s); e != nil {
		return s, e
	}
	return s, s.Validate()
}
func present(m map[string]json.RawMessage, names ...string) error {
	for _, n := range names {
		b, ok := m[n]
		if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return fmt.Errorf("required field %s missing or null", n)
		}
	}
	return nil
}
func unique(d *json.Decoder, depth int) error {
	if depth > 20 {
		return errors.New("JSON nesting too deep")
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	if t == nil {
		return errors.New("null is not a sensor value")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return errors.New("duplicate JSON field")
			}
			seen[s] = true
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, e = d.Token()
	return e
}

type Config struct {
	Tenant, Site, RunID string
	Frame               world.FrameRef
	Source              world.Source
	Epoch               uint64
	Anchor              time.Time
	TTL                 time.Duration
}
type Converter struct {
	cfg  Config
	last map[string]Sample
}

func New(c Config) (*Converter, error) {
	if c.Tenant == "" || c.Site == "" || !identifier.MatchString(c.RunID) || c.Epoch == 0 || c.Anchor.IsZero() || c.TTL <= 0 || c.TTL > 24*time.Hour {
		return nil, errors.New("invalid sensor conversion configuration")
	}
	if e := c.Frame.Validate(); e != nil {
		return nil, e
	}
	if e := c.Source.Validate(); e != nil {
		return nil, e
	}
	return &Converter{c, map[string]Sample{}}, nil
}
func (c *Converter) Convert(raw []byte) (world.Component, error) {
	s, e := Decode(raw)
	if e != nil {
		return world.Component{}, e
	}
	if s.SiteID != c.cfg.Site || s.Frame != c.cfg.Frame || s.RunID != c.cfg.RunID || s.SensorID != c.cfg.Source.Service {
		return world.Component{}, errors.New("sample scope mismatch")
	}
	if old, ok := c.last[s.SensorID]; ok {
		if s.Sequence <= old.Sequence || s.SampleNS < old.SampleNS || s.Kind != old.Kind || s.PackageSHA256 != old.PackageSHA256 || s.ScenarioVersion != old.ScenarioVersion || s.Seed != old.Seed {
			return world.Component{}, errors.New("sensor sequence, clock or run lineage changed")
		}
	}
	payload, _ := json.Marshal(s)
	p := world.Provenance{Origin: world.Simulated, AppVersion: "world-sensor-v1", ModelID: s.SensorModel, ModelVersion: s.ScenarioVersion, Simulation: &world.SimulationContext{RunID: s.RunID, ScenarioVersion: s.ScenarioVersion, Seed: s.Seed, ClockDomain: "simulation:" + s.RunID}}
	out := world.Component{Key: world.Key{Tenant: c.cfg.Tenant, Site: c.cfg.Site, Entity: s.RunID + "/" + s.SensorID, Type: ComponentType, Source: c.cfg.Source}, Version: world.Version{Epoch: c.cfg.Epoch, Sequence: s.Sequence}, Frame: s.Frame, Timing: world.Timing{SampleTime: c.cfg.Anchor.Add(time.Duration(s.SampleNS)), ClockDomain: "simulation:" + s.RunID, ErrorKnown: true}, TTL: c.cfg.TTL, Provenance: p, Payload: payload}
	if link := s.SensorLink; link != nil {
		if !c.cfg.Anchor.Equal(time.UnixMicro(link.AnchorUnixUS)) {
			return world.Component{}, errors.New("SensorLink anchor differs from converter")
		}
		out.Timing.ErrorBound = time.Duration(link.ClockErrorUS) * time.Microsecond
	}
	c.last[s.SensorID] = s
	return out, nil
}

// FromComponent binds the opaque payload back to authenticated outer metadata.
func FromComponent(c world.Component) (Sample, error) {
	var zero Sample
	if c.Key.Type != ComponentType || c.Tombstone || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.TTL <= 0 {
		return zero, errors.New("not a live sensor component")
	}
	if e := c.Key.Source.Validate(); e != nil {
		return zero, e
	}
	if e := c.Provenance.Validate(); e != nil {
		return zero, e
	}
	if e := c.Timing.Validate(); e != nil {
		return zero, e
	}
	s, e := Decode(c.Payload)
	if e != nil {
		return zero, e
	}
	p := c.Provenance
	if c.Key.Source.Service != s.SensorID || c.Key.Tenant == "" || c.Key.Site != s.SiteID || c.Key.Entity != s.RunID+"/"+s.SensorID || c.Frame != s.Frame || c.Version.Sequence != s.Sequence || p.Origin != world.Simulated || p.Simulation == nil || p.Simulation.RunID != s.RunID || p.Simulation.ScenarioVersion != s.ScenarioVersion || p.Simulation.Seed != s.Seed || p.Simulation.ClockDomain != "simulation:"+s.RunID || c.Timing.ClockDomain != p.Simulation.ClockDomain || p.ModelID != s.SensorModel || p.ModelVersion != s.ScenarioVersion {
		return zero, errors.New("sensor payload does not match authenticated component")
	}
	if link := s.SensorLink; link != nil {
		if !c.Timing.SampleTime.Equal(time.UnixMicro(int64(link.TimestampUS))) || !c.Timing.ErrorKnown || c.Timing.ErrorBound != time.Duration(link.ClockErrorUS)*time.Microsecond {
			return zero, errors.New("SensorLink capture timestamp or clock error differs from component")
		}
	}
	return s, nil
}
