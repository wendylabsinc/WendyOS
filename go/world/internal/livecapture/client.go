// Package livecapture reads bounded immutable snapshots from the synthetic live scene.
// It never grants authority to actuate hardware.
package livecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Schema = "wendy.sim.live.v1"
const MaxResponseBytes = 1 << 20
const MaxObservations = 16

var ErrBootChanged = errors.New("live scene restarted; explicit client restart and new publisher epoch required")
var ErrStale = errors.New("live capture is stale or future-dated")

type Config struct {
	URL, Token, RunID, Site string
	Frame                   world.FrameRef
	Now                     func() time.Time
	MaxAge                  time.Duration
	Planar                  *PlanarConfig
}
type Provenance struct {
	Mode          string `json:"mode"`
	PackageSHA256 string `json:"package_sha256"`
	RobotModel    string `json:"robot_model"`
	GasModel      string `json:"gas_model"`
}
type State struct {
	RunID                   string          `json:"run_id,omitempty"`
	RobotID                 string          `json:"robot_id,omitempty"`
	SchemaVersion           string          `json:"schema_version"`
	Synthetic               bool            `json:"synthetic"`
	BootID                  string          `json:"boot_id"`
	Session                 string          `json:"session"`
	Sequence                uint64          `json:"sequence"`
	SiteID                  string          `json:"site_id"`
	Frame                   world.FrameRef  `json:"frame"`
	Position                [3]float64      `json:"position_m"`
	Velocity                [3]float64      `json:"velocity_mps"`
	SampleUnixMS            int64           `json:"sample_unix_ms"`
	SimulationTimeS         float64         `json:"simulation_time_s"`
	Sensors                 map[string]bool `json:"sensors"`
	ObstacleClear           bool            `json:"obstacle_clear"`
	GasPPM                  float64         `json:"gas_ppm"`
	Energy                  float64         `json:"energy"`
	CommandedVelocity       float64         `json:"commanded_velocity_mps"`
	CommandedVelocityVector [3]float64      `json:"-"`
	LeaseRemainingMS        int64           `json:"lease_remaining_ms"`
	WatchdogStopped         bool            `json:"watchdog_stopped"`
	Provenance              Provenance      `json:"provenance"`
}
type Media struct {
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}
type Snapshot struct {
	SchemaVersion    string           `json:"schema_version"`
	RunID            string           `json:"run_id"`
	BootID           string           `json:"boot_id"`
	Sequence         uint64           `json:"sequence"`
	SampleUnixMS     int64            `json:"sample_unix_ms"`
	SimulationTimeNS int64            `json:"simulation_time_ns"`
	State            State            `json:"state"`
	Observations     []sensing.Sample `json:"observations"`
	Media            []Media          `json:"media"`
}
type Client struct {
	mu                      sync.Mutex
	cfg                     Config
	http                    *http.Client
	boot                    string
	sequence                uint64
	sampleMS, simNS         int64
	digest                  [32]byte
	packageDigest, scenario string
	seed                    uint64
	fault                   error
}
type HTTPError struct{ Status int }

func (e HTTPError) Error() string { return fmt.Sprintf("live scene HTTP %d", e.Status) }
func New(c Config) (*Client, error) {
	if c.Planar != nil {
		if err := c.Planar.validate(); err != nil {
			return nil, err
		}
		owned := *c.Planar
		c.Planar = &owned
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || len(c.Token) < 32 || len(c.Token) > 256 || strings.ContainsAny(c.Token, "\r\n") || c.RunID == "" || c.Site == "" {
		return nil, errors.New("invalid explicit simulation capture configuration")
	}
	for _, r := range c.Token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return nil, errors.New("invalid capture token")
		}
	}
	if err = c.Frame.Validate(); err != nil {
		return nil, err
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxAge == 0 {
		c.MaxAge = 500 * time.Millisecond
	}
	if c.MaxAge < time.Millisecond || c.MaxAge > 500*time.Millisecond {
		return nil, errors.New("capture maximum age must be 1..500ms")
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	// A scene read token is scoped to this explicit simulation endpoint.
	// Ambient proxy settings or a process-global transport must not receive it.
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableCompression: true, MaxResponseHeaderBytes: 4096, ResponseHeaderTimeout: time.Second, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}
	return &Client{cfg: c, http: &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("capture redirects forbidden") }}}, nil
}
func (c *Client) Capture(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fault != nil {
		return Snapshot{}, c.fault
	}
	path := "/v1/samples"
	if c.cfg.Planar != nil {
		path = "/v1/inspection/samples"
	}
	r, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+path, nil)
	if err != nil {
		return Snapshot{}, err
	}
	r.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	response, err := c.http.Do(r)
	if err != nil {
		return Snapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Snapshot{}, HTTPError{response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := Decode(raw, c.cfg)
	if err != nil {
		if !errors.Is(err, ErrStale) {
			c.fault = err
		}
		return Snapshot{}, err
	}
	if c.boot != "" && snapshot.BootID != c.boot {
		c.fault = ErrBootChanged
		return Snapshot{}, c.fault
	}
	encoded, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(encoded)
	if snapshot.Sequence < c.sequence || snapshot.SampleUnixMS < c.sampleMS || snapshot.SimulationTimeNS < c.simNS {
		c.fault = errors.New("live capture sequence or clock regressed")
		return Snapshot{}, c.fault
	}
	if snapshot.Sequence == c.sequence && c.sequence != 0 && digest != c.digest {
		c.fault = errors.New("live capture reused sequence with different content")
		return Snapshot{}, c.fault
	}
	// Wall timestamps are quantized to milliseconds, so distinct physics
	// captures may share one. Strict sequence and simulation-time progression
	// establish novelty; Decode still applies the original wall-time age bound.
	// Wall-clock rollback remains rejected above.
	if snapshot.Sequence > c.sequence && c.sequence != 0 && snapshot.SimulationTimeNS <= c.simNS {
		c.fault = errors.New("new capture did not advance simulation time")
		return Snapshot{}, c.fault
	}
	for _, s := range snapshot.Observations {
		if c.packageDigest != "" && (s.PackageSHA256 != c.packageDigest || s.ScenarioVersion != c.scenario || s.Seed != c.seed) {
			c.fault = errors.New("live scene lineage changed within a boot")
			return Snapshot{}, c.fault
		}
		c.packageDigest, c.scenario, c.seed = s.PackageSHA256, s.ScenarioVersion, s.Seed
	}
	c.boot, c.sequence, c.sampleMS, c.simNS, c.digest = snapshot.BootID, snapshot.Sequence, snapshot.SampleUnixMS, snapshot.SimulationTimeNS, digest
	return snapshot, nil
}
func required(m map[string]json.RawMessage, names ...string) error {
	for _, n := range names {
		b, ok := m[n]
		if !ok || strings.TrimSpace(string(b)) == "null" {
			return fmt.Errorf("required capture field %s missing or null", n)
		}
	}
	return nil
}
func object(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, required(m, names...)
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Decode validates the complete coherent snapshot. This decoder may be used by
// tests or trusted adapters; Client adds cross-response boot and ordering fences.
func Decode(raw []byte, c Config) (Snapshot, error) {
	var s Snapshot
	if err := authority.DecodeStrictLimit(raw, &s, MaxResponseBytes); err != nil {
		return s, err
	}
	fields, err := object(raw, "schema_version", "run_id", "boot_id", "sequence", "sample_unix_ms", "simulation_time_ns", "state", "observations", "media")
	if err != nil {
		return s, err
	}
	st, err := object(fields["state"], "schema_version", "synthetic", "boot_id", "session", "sequence", "site_id", "frame", "position_m", "velocity_mps", "sample_unix_ms", "simulation_time_s", "sensors", "obstacle_clear", "gas_ppm", "energy", "commanded_velocity_mps", "lease_remaining_ms", "watchdog_stopped", "provenance")
	if err != nil {
		return s, err
	}
	for _, key := range []string{"position_m", "velocity_mps"} {
		var a []json.RawMessage
		if err = json.Unmarshal(st[key], &a); err != nil || len(a) != 3 {
			return s, errors.New("capture coordinates require three values")
		}
		for _, v := range a {
			if strings.TrimSpace(string(v)) == "null" {
				return s, errors.New("null capture coordinate")
			}
		}
	}
	if _, err = object(st["frame"], "id", "revision"); err != nil {
		return s, err
	}
	if _, err = object(st["provenance"], "mode", "package_sha256", "robot_model", "gas_model"); err != nil {
		return s, err
	}
	sensors, err := object(st["sensors"], "gas", "local-obstacles")
	if err != nil {
		return s, err
	}
	for id := range sensors {
		if id != "gas" && id != "local-obstacles" && !sensing.LidarSensorID(id) {
			return s, errors.New("unknown local sensor status")
		}
	}
	if len(sensors) > MaxObservations-2 {
		return s, errors.New("too many local sensor statuses")
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	age := c.MaxAge
	if age == 0 {
		age = 500 * time.Millisecond
	}
	if s.SchemaVersion != Schema || s.RunID != c.RunID || s.BootID == "" || len(s.BootID) > 128 || s.Sequence == 0 || s.Sequence > 1<<53-1 {
		return s, errors.New("capture identity or schema mismatch")
	}
	if s.SampleUnixMS <= 0 || s.SampleUnixMS > now.UnixMilli() || now.Sub(time.UnixMilli(s.SampleUnixMS)) > age {
		return s, ErrStale
	}
	if s.SimulationTimeNS < 0 || s.SimulationTimeNS > int64(24*time.Hour) || len(s.Observations) > MaxObservations || len(s.Media) > 3 {
		return s, errors.New("capture exceeds simulation bounds")
	}
	state := s.State
	schema := "wendy.sim.actuator.v0"
	if c.Planar != nil {
		schema = PlanarStateSchema
	}
	if state.SchemaVersion != schema || !state.Synthetic || state.BootID != s.BootID || state.Session == "" || len(state.Session) > 128 || state.Sequence > 1<<53-1 || state.SiteID != c.Site || state.Frame != c.Frame || state.SampleUnixMS != s.SampleUnixMS || !finite(state.SimulationTimeS) || state.SimulationTimeS < 0 || math.Abs(float64(s.SimulationTimeNS)-math.Round(state.SimulationTimeS*1e9)) > 1 || !finite(state.GasPPM) || state.GasPPM < 0 || !finite(state.Energy) || state.Energy < 0 || state.Energy > 1 || !finite(state.CommandedVelocity) || math.Abs(state.CommandedVelocity) > .5 || state.LeaseRemainingMS < 0 || state.LeaseRemainingMS > 500 {
		return s, errors.New("inconsistent synthetic actuator snapshot")
	}
	for i := range state.Position {
		if !finite(state.Position[i]) || !finite(state.Velocity[i]) {
			return s, errors.New("nonfinite capture coordinates")
		}
	}
	if c.Planar != nil {
		if state.RunID != s.RunID {
			return s, errors.New("planar capture state run mismatch")
		}
		if err := c.Planar.check(state); err != nil {
			return s, err
		}
		if len(s.Observations) != 4 {
			return s, errors.New("planar inspection requires four original observations")
		}
	} else if math.Abs(state.Position[1]-.2) > 1e-6 || math.Abs(state.Position[2]) > 1e-6 || math.Abs(state.Velocity[1]) > 1e-6 || math.Abs(state.Velocity[2]) > 1e-6 || math.Abs(state.Velocity[0]) > .501 {
		return s, errors.New("unsupported synthetic robot kinematics")
	}
	var rawSamples []json.RawMessage
	if err = json.Unmarshal(fields["observations"], &rawSamples); err != nil {
		return s, err
	}
	seen := map[string]bool{}
	refs := map[string]sensing.EvidenceRef{}
	packageDigest, scenario := "", ""
	var seed uint64
	for i, rawSample := range rawSamples {
		sample, err := sensing.Decode(rawSample)
		if err != nil {
			return s, err
		}
		expectedKind := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}[sample.SensorID]
		if sensing.LidarSensorID(sample.SensorID) {
			expectedKind = "lidar"
		}
		if seen[sample.SensorID] || expectedKind == "" || sample.Kind != expectedKind || sample.RunID != s.RunID || sample.SiteID != c.Site || sample.Frame != c.Frame || sample.Sequence != s.Sequence || sample.SampleNS != s.SimulationTimeNS || sample.PackageSHA256 != state.Provenance.PackageSHA256 {
			return s, errors.New("observation disagrees with capture scope or sequence")
		}
		if sample.Kind == "gas" && !state.Sensors["gas"] {
			return s, errors.New("unavailable gas sensor supplied observation")
		}
		if available, known := state.Sensors[sample.SensorID]; sample.Kind == "lidar" && known && !available {
			return s, errors.New("unavailable lidar sensor supplied observation")
		}
		if sample.SensorID == "gas-1" || sample.SensorID == "audio-1" || sample.SensorID == "camera-b" {
			want := state.Position
			if sample.SensorID == "camera-b" {
				if c.Planar != nil {
					want[1] += .45
				} else {
					want[1] += 1.8
					want[2] += 2
				}
			}
			for axis, value := range want {
				if math.Abs(sample.PositionM[axis]-value) > 1e-6 {
					return s, errors.New("sensor position disagrees with captured robot pose")
				}
			}
		}
		if packageDigest != "" && (sample.PackageSHA256 != packageDigest || sample.ScenarioVersion != scenario || sample.Seed != seed) {
			return s, errors.New("mixed snapshot lineage")
		}
		packageDigest, scenario, seed = sample.PackageSHA256, sample.ScenarioVersion, sample.Seed
		seen[sample.SensorID] = true
		s.Observations[i] = sample
		if sample.Evidence != nil {
			if old, ok := refs[sample.Evidence.SHA256]; ok && old != *sample.Evidence {
				return s, errors.New("inconsistent shared media metadata")
			}
			refs[sample.Evidence.SHA256] = *sample.Evidence
		}
	}
	if !seen["camera-a"] || !seen["camera-b"] || !seen["audio-1"] || seen["gas-1"] != state.Sensors["gas"] {
		return s, errors.New("capture is missing an available sensor")
	}
	if state.Provenance.Mode != "simulated" || !sensing.DigestValid(state.Provenance.PackageSHA256) || state.Provenance.RobotModel == "" || state.Provenance.GasModel == "" {
		return s, errors.New("missing synthetic state lineage")
	}
	var rawMedia []json.RawMessage
	if err = json.Unmarshal(fields["media"], &rawMedia); err != nil {
		return s, err
	}
	seenMedia := map[string]bool{}
	for i, m := range s.Media {
		if _, err = object(rawMedia[i], "sha256", "data"); err != nil {
			return s, err
		}
		ref, ok := refs[m.SHA256]
		hash := sha256.Sum256(m.Data)
		if !ok || seenMedia[m.SHA256] || int64(len(m.Data)) != ref.Size || hex.EncodeToString(hash[:]) != ref.SHA256 {
			return s, errors.New("capture media digest/size mismatch or extra media")
		}
		seenMedia[m.SHA256] = true
	}
	if len(refs) != len(seenMedia) {
		return s, errors.New("capture media missing")
	}
	return s, nil
}
func (s Snapshot) Evidence(ref sensing.EvidenceRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	for _, m := range s.Media {
		if m.SHA256 == ref.SHA256 {
			sum := sha256.Sum256(m.Data)
			if int64(len(m.Data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
				return nil, errors.New("capture media integrity failure")
			}
			return append([]byte(nil), m.Data...), nil
		}
	}
	return nil, errors.New("capture evidence unavailable")
}

// Component preserves relative simulation time inside the payload while assigning
// the immutable capture timestamp, never the time of a later poll or publication.
func (s Snapshot) Component(sensorID string, cfg sensing.Config) (world.Component, error) {
	if s.RunID != cfg.RunID || s.SampleUnixMS <= 0 {
		return world.Component{}, errors.New("capture run mismatch")
	}
	for _, sample := range s.Observations {
		if sample.SensorID == sensorID {
			if sample.Sequence != s.Sequence || sample.SampleNS != s.SimulationTimeNS {
				return world.Component{}, errors.New("capture ordering mismatch")
			}
			cfg.Anchor = time.UnixMilli(s.SampleUnixMS).Add(-time.Duration(sample.SampleNS))
			converter, err := sensing.New(cfg)
			if err != nil {
				return world.Component{}, err
			}
			raw, err := json.Marshal(sample)
			if err != nil {
				return world.Component{}, err
			}
			return converter.Convert(raw)
		}
	}
	return world.Component{}, errors.New("sensor absent from capture")
}
