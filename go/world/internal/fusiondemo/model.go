// Package fusiondemo runs participant-local simulation fusion over the existing
// durable world and evidence stores. The transport supplies only observations
// delivered to this participant; this package performs no networking.
package fusiondemo

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	MaxLineBytes  = 16 << 20
	MaxMediaBytes = 8 << 20
	Tenant        = "simulation"
	Site          = "studio-01"
	App           = "world-office-sensors"
	TTL           = 2 * time.Second
)

var Anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type Sensor struct {
	SensorID string `json:"sensor_id"`
	Kind     string `json:"kind"`
	Owner    string `json:"owner"`
}

func (s Sensor) Source() world.Source {
	return world.Source{Device: s.Owner, App: App, Service: s.SensorID, Stream: "samples"}
}

type Config struct {
	CameraQuorum  []string       `json:"camera_quorum,omitempty"`
	Version       int            `json:"version"`
	ParticipantID string         `json:"participant_id"`
	RunID         string         `json:"run_id"`
	Frame         world.FrameRef `json:"frame"`
	Sensors       []Sensor       `json:"sensors"`
}

func (c Config) Validate() error {
	if c.Version != 1 || !identifier.MatchString(c.ParticipantID) || !identifier.MatchString(c.RunID) || c.Frame.Validate() != nil || len(c.Sensors) < 5 || len(c.Sensors) > 16 {
		return errors.New("invalid participant configuration")
	}
	required := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}
	if len(c.CameraQuorum) > 0 {
		if len(c.CameraQuorum) < 2 || len(c.CameraQuorum) > 6 {
			return errors.New("invalid camera quorum size")
		}
		delete(required, "camera-a")
		delete(required, "camera-b")
		for _, id := range c.CameraQuorum {
			if !identifier.MatchString(id) || required[id] != "" {
				return errors.New("invalid quorum camera")
			}
			required[id] = "camera"
		}
	}
	seen := map[string]bool{}
	participant := c.ParticipantID == "hub"
	for _, s := range c.Sensors {
		if seen[s.SensorID] || !identifier.MatchString(s.Owner) || s.Source().Validate() != nil {
			return errors.New("invalid or duplicate sensor catalog entry")
		}
		seen[s.SensorID] = true
		if kind, ok := required[s.SensorID]; ok {
			if s.Kind != kind || s.Owner != "hub" {
				return errors.New("base observations must belong to hub")
			}
		} else if !sensing.LidarSensorID(s.SensorID) || s.Kind != "lidar" || s.Owner != strings.TrimPrefix(s.SensorID, "lidar-") {
			return errors.New("LiDAR owner must match sensor ID")
		}
		participant = participant || s.Owner == c.ParticipantID
	}
	for id := range required {
		if !seen[id] {
			return errors.New("required observation absent from catalog")
		}
	}
	if !participant {
		return errors.New("participant absent from catalog")
	}
	return nil
}

type Tick struct {
	NowNS        int64             `json:"now_ns"`
	LocalSamples []json.RawMessage `json:"local_samples"`
	Received     []world.Component `json:"received"`
	Media        map[string][]byte `json:"media"`
}
type Missing struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	SourceNode string `json:"source_node"`
	Reason     string `json:"reason"`
}
type ObservationStatus struct {
	ID         string  `json:"id"`
	SourceNode string  `json:"source_node"`
	Delivery   string  `json:"delivery"`
	AgeS       float64 `json:"age_s"`
}
type Output struct {
	ParticipantID       string              `json:"participant_id"`
	NowNS               int64               `json:"now_ns"`
	Version             world.Version       `json:"version"`
	Claim               *world.Component    `json:"claim,omitempty"`
	Result              fusion.Result       `json:"result"`
	Observations        []sensing.Sample    `json:"observations"`
	ObservationStatus   []ObservationStatus `json:"observation_status"`
	MissingObservations []Missing           `json:"missing_observations"`
	ReplicaRecords      []world.Component   `json:"replica_records"`
	Media               map[string][]byte   `json:"media"`
	Applied             int                 `json:"applied"`
	Duplicates          int                 `json:"duplicates"`
	Obsolete            int                 `json:"obsolete"`
}
