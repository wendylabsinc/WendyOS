// Package ingest translates versioned simulator output into World records.
// The caller supplies authenticated publisher identity and a persisted epoch.
package ingest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// SimulationSample is the versioned sim.run JSONL contract. It remains in the
// payload so original relative time and package identity survive translation.
type SimulationSample struct {
	Schema        string       `json:"schema_version"`
	ID            string       `json:"id"`
	Site          string       `json:"site_id"`
	FrameRevision string       `json:"frame_revision"`
	Kind          string       `json:"kind"`
	Source        world.Source `json:"source"`
	Sequence      uint64       `json:"sequence"`
	Timing        struct {
		ClockDomain string `json:"clock_domain"`
		SampleNS    int64  `json:"sample_ns"`
	} `json:"timing"`
	Provenance struct {
		Mode            string `json:"mode"`
		RunID           string `json:"run_id"`
		Seed            uint64 `json:"seed"`
		PackageRevision string `json:"package_revision"`
		PackageSHA256   string `json:"package_sha256"`
		MuJoCoVersion   string `json:"mujoco_version"`
		SensorModel     string `json:"sensor_model"`
	} `json:"provenance"`
	Value struct {
		Position      []float64 `json:"position_m"`
		Concentration *float64  `json:"concentration,omitempty"`
		Unit          string    `json:"unit,omitempty"`
		Species       string    `json:"species,omitempty"`
		Noise         *float64  `json:"noise_stddev_ppm,omitempty"`
	} `json:"value"`
}

type Config struct {
	Tenant string
	Site   string
	Frame  world.FrameRef
	Source world.Source
	Epoch  uint64
	RunID  string
	// Anchor maps simulation-relative time onto the node's process clock for
	// real-time ingestion. It is not a statement of physical capture UTC.
	Anchor time.Time
	TTL    time.Duration
}

type Converter struct {
	config   Config
	sequence uint64
	sampleNS int64
}

func New(c Config) (*Converter, error) {
	if c.Tenant == "" || c.Site == "" || c.RunID == "" || c.Epoch == 0 || c.Anchor.IsZero() || c.TTL <= 0 || c.TTL > 24*time.Hour {
		return nil, errors.New("invalid simulation ingestion config")
	}
	if err := c.Source.Validate(); err != nil {
		return nil, err
	}
	if err := c.Frame.Validate(); err != nil {
		return nil, err
	}
	return &Converter{config: c}, nil
}

// Convert is sequential. It rejects a changed clock/run/frame and nonmonotonic
// samples before advancing its sequence. Input size is bounded independently
// of the scanner used by the caller.
func (c *Converter) Convert(line []byte) (world.Component, error) {
	var s SimulationSample
	if len(line) > 65536 {
		return world.Component{}, errors.New("simulation sample exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return world.Component{}, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return world.Component{}, errors.New("expected one JSON sample")
	}
	if s.Schema != "wendy.sim.observation.v0" || s.Site != c.config.Site || s.FrameRevision != c.config.Frame.Revision {
		return world.Component{}, errors.New("incompatible simulation schema, site or frame")
	}
	if s.Sequence <= c.sequence || s.Timing.SampleNS < 0 || s.Timing.SampleNS > int64(24*time.Hour) || s.Timing.SampleNS < c.sampleNS {
		return world.Component{}, errors.New("simulation sample order regressed")
	}
	if s.Provenance.Mode != "simulated" || s.Provenance.RunID != c.config.RunID || s.Timing.ClockDomain != "simulation:"+c.config.RunID {
		return world.Component{}, errors.New("simulation provenance mismatch")
	}
	if s.ID != fmt.Sprintf("%s/%d", c.config.RunID, s.Sequence) || s.Provenance.PackageRevision == "" || s.Provenance.MuJoCoVersion == "" || s.Provenance.SensorModel == "" {
		return world.Component{}, errors.New("incomplete simulation lineage")
	}
	hash, err := hex.DecodeString(s.Provenance.PackageSHA256)
	if err != nil || len(hash) != 32 {
		return world.Component{}, errors.New("invalid package digest")
	}
	if err := s.Source.Validate(); err != nil {
		return world.Component{}, err
	}
	if len(s.Value.Position) != 3 {
		return world.Component{}, errors.New("position must have three coordinates")
	}
	for _, v := range s.Value.Position {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return world.Component{}, errors.New("invalid position")
		}
	}
	switch s.Kind {
	case "pose":
		if s.Value.Concentration != nil || s.Value.Noise != nil || s.Value.Unit != "" || s.Value.Species != "" {
			return world.Component{}, errors.New("pose contains gas fields")
		}
	case "gas":
		if s.Value.Concentration == nil || s.Value.Noise == nil || s.Value.Unit != "ppm" || s.Value.Species == "" || !nonnegative(*s.Value.Concentration) || !nonnegative(*s.Value.Noise) {
			return world.Component{}, errors.New("invalid gas measurement")
		}
	default:
		return world.Component{}, errors.New("unsupported simulated sensor kind")
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return world.Component{}, err
	}
	component := world.Component{
		Key:     world.Key{Tenant: c.config.Tenant, Site: c.config.Site, Entity: c.config.RunID + "/" + s.Kind, Type: "wendy." + s.Kind + ".v1", Source: c.config.Source},
		Version: world.Version{Epoch: c.config.Epoch, Sequence: s.Sequence}, Frame: c.config.Frame,
		Timing: world.Timing{SampleTime: c.config.Anchor.Add(time.Duration(s.Timing.SampleNS)), ClockDomain: s.Timing.ClockDomain, ErrorKnown: true}, TTL: c.config.TTL,
		Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "world-ingest-v1", ModelID: s.Provenance.SensorModel, ModelVersion: s.Provenance.MuJoCoVersion, Simulation: &world.SimulationContext{RunID: c.config.RunID, ScenarioVersion: s.Provenance.PackageRevision, Seed: s.Provenance.Seed, ClockDomain: s.Timing.ClockDomain}}, Payload: payload,
	}
	c.sequence = s.Sequence
	c.sampleNS = s.Timing.SampleNS
	return component, nil
}

func nonnegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
