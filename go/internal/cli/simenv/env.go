// Package simenv translates a Wendy Data campaign and the device's resolved
// sources into a simulator-environment description: an env.json interface
// manifest and a MuJoCo scene.xml scaffold.
//
// The package is pure and free of input and output. It performs no network,
// no filesystem, no cobra and no gRPC work. It accepts already-fetched data as
// plain Go values and returns bytes, so the surrounding command line interface
// command (a separate task) owns every side effect and this logic stays unit
// testable in isolation.
//
// The env.json shape deliberately reads beside the simulator compatibility
// manifests under go/simulator/<kind>/compatibility.json: snake_case keys, a
// schema version, a profile, a physics block and an observations list.
package simenv

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SchemaVersion is the env.json schema revision this package emits.
const SchemaVersion = 1

// DefaultTimestepSeconds is the MuJoCo integration timestep written into the
// scene scaffold. It mirrors the value the Go2 virtual runtime uses in
// go/simulator/go2/compatibility.json.
const DefaultTimestepSeconds = 0.002

// Observation is one simulated interface the environment exposes to a policy.
//
// Kind is one of "image", "depth" or "vector". Dims is nil when the dimension
// is not knowable from the campaign and the resolved sources alone, in which
// case a matching entry is added to Env.Warnings so the operator sets it by
// hand rather than trusting an invented shape.
type Observation struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Source is the resolved source identifier the observation is drawn from,
	// for example "v4l2:/dev/video0" or a ROS 2 per-topic identifier.
	Source string `json:"source"`
	// Dims is the observation tensor shape, nil when unknown. For an image it
	// is [height, width, channels]; for a vector it is [length].
	Dims  []int  `json:"dims,omitempty"`
	Dtype string `json:"dtype,omitempty"`
	Unit  string `json:"unit,omitempty"`
	// PixelFormat records the camera pixel format noted from a matched
	// VideoDevice, for image observations only. There is no dedicated pixel
	// format field in the compatibility manifests, so it is carried here.
	PixelFormat string  `json:"pixel_format,omitempty"`
	RateHz      float64 `json:"rate_hz,omitempty"`
}

// Action describes the application action stream a policy drives. Low and High
// hold the per-channel control range, aligned with Channels by index.
type Action struct {
	Stream   string    `json:"stream"`
	Channels []string  `json:"channels"`
	Low      []float64 `json:"low"`
	High     []float64 `json:"high"`
	Unit     string    `json:"unit,omitempty"`
}

// Env is the full simulator-environment interface manifest. It marshals to
// env.json and is the single value both renderers consume.
type Env struct {
	SchemaVersion int           `json:"schema_version"`
	Campaign      string        `json:"campaign"`
	Revision      string        `json:"revision,omitempty"`
	Profile       string        `json:"profile"`
	ControlRateHz float64       `json:"control_rate_hz,omitempty"`
	Observations  []Observation `json:"observations"`
	Action        *Action       `json:"action,omitempty"`
	Warnings      []string      `json:"warnings,omitempty"`
}

// RenderEnvJSON renders the environment as stable, indented JSON shaped like
// the simulator compatibility manifests. The output round-trips through
// encoding/json back to an equal Env.
func RenderEnvJSON(env Env) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Keep the literal characters in warnings and identifiers; HTML escaping
	// would mangle a ROS 2 topic or a range expression for no benefit here.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return nil, fmt.Errorf("render env.json: %w", err)
	}
	return buf.Bytes(), nil
}
