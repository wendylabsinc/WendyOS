// world-go2-cloud-check evaluates one recorded cloud offline. It has no robot
// adapter, network client, clock refresh, task authority or command publisher.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

const maxInput = 1 << 20
const maxOutput = 8 << 20

// Null is permitted only for an explicitly missing range and the optional
// continuation request. All zero-valued uncertainty fields must be present.
type scanInput struct {
	TiltedPose                                   *go2nav.TiltedPose
	Sequence                                     uint64
	CapturedUnixNS                               int64
	WorldEpoch                                   uint64
	Frame                                        string
	Ranges                                       []*float64
	AngleMin, AngleIncrement, RangeMin, RangeMax float64
	Robot, SensorMount                           go2nav.Pose
	MountKnown                                   bool
	PositionUncertaintyM, YawUncertaintyRad      float64
	UncertaintyKnown                             bool
}

type observationInput struct {
	Model            string     `json:"model"`
	SourceDigest     string     `json:"source_digest"`
	Frame            string     `json:"frame"`
	PublisherGID     string     `json:"publisher_gid"`
	SerializedSHA256 string     `json:"serialized_sha256"`
	CapturedUnixNS   int64      `json:"captured_unix_ns"`
	Scan             scanInput  `json:"scan"`
	Ranges           []*float64 `json:"ranges"`
}

type continuationInput struct {
	Pose     go2nav.Pose         `json:"pose"`
	Measured go2nav.BodyMotion   `json:"measured"`
	Prior    []go2nav.Command    `json:"prior"`
	Proposed go2nav.Command      `json:"proposed"`
	Limits   go2nav.MotionLimits `json:"limits"`
}

type request struct {
	Schema       string             `json:"schema"`
	Simulation   bool               `json:"simulation"`
	Observation  observationInput   `json:"observation"`
	Bounds       go2nav.Bounds      `json:"bounds"`
	Profile      go2nav.Profile     `json:"profile"`
	Continuation *continuationInput `json:"continuation"`
}

func complete(v any, t reflect.Type, path string) error {
	if v == nil {
		if path == "request.continuation" || path == "request.observation.ranges[]" || path == "request.observation.scan.Ranges[]" {
			return nil
		}
		return errors.New("absent or null input field: " + path)
	}
	if t.Kind() == reflect.Pointer {
		return complete(v, t.Elem(), path)
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("object required: " + path)
		}
		if len(m) != t.NumField() {
			return errors.New("exact fields required: " + path)
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			x, exists := m[name]
			if !exists {
				return errors.New("missing input field: " + path + "." + name)
			}
			if err := complete(x, f.Type, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		a, ok := v.([]any)
		if !ok || t.Kind() == reflect.Array && len(a) != t.Len() {
			return errors.New("array shape: " + path)
		}
		for _, x := range a {
			if err := complete(x, t.Elem(), path+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func ranges(values []*float64) []float64 {
	out := make([]float64, len(values))
	for i, v := range values {
		if v == nil {
			out[i] = math.Inf(1)
		} else {
			out[i] = *v
		}
	}
	return out
}

func (w observationInput) cloud() go2nav.CloudObservation {
	s := w.Scan
	return go2nav.CloudObservation{Model: w.Model, SourceDigest: w.SourceDigest, Frame: w.Frame,
		PublisherGID: w.PublisherGID, SerializedSHA256: w.SerializedSHA256, CapturedUnixNS: w.CapturedUnixNS,
		Ranges: ranges(w.Ranges), Scan: go2nav.Scan{TiltedPose: s.TiltedPose, Sequence: s.Sequence,
			CapturedUnixNS: s.CapturedUnixNS, WorldEpoch: s.WorldEpoch, Frame: s.Frame, Ranges: ranges(s.Ranges),
			AngleMin: s.AngleMin, AngleIncrement: s.AngleIncrement, RangeMin: s.RangeMin, RangeMax: s.RangeMax,
			Robot: s.Robot, SensorMount: s.SensorMount, MountKnown: s.MountKnown,
			PositionUncertaintyM: s.PositionUncertaintyM, YawUncertaintyRad: s.YawUncertaintyRad, UncertaintyKnown: s.UncertaintyKnown}}
}

func errorText(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func inspect(g *go2nav.Grid, build error, pose go2nav.Pose, c *continuationInput) map[string]any {
	out := map[string]any{"build_error": errorText(build), "current_footprint_clear": false, "continuation_requested": c != nil}
	if build != nil {
		return out
	}
	out["current_footprint_clear"] = g.ClearSweep(go2nav.Point{X: pose.X, Y: pose.Y}, go2nav.Point{X: pose.X, Y: pose.Y}, pose.Yaw)
	if c != nil {
		out["continuation_error"] = errorText(g.CheckContinuation(c.Pose, c.Measured, c.Prior, c.Proposed, c.Limits))
	}
	return out
}

func run(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, maxInput+1))
	if err != nil || len(raw) > maxInput {
		return errors.New("offline input exceeds bound")
	}
	var r request
	if err := authority.DecodeStrictLimit(raw, &r, maxInput); err != nil {
		return errors.New("invalid offline request")
	}
	var shape any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&shape); err != nil {
		return err
	}
	if err := complete(shape, reflect.TypeOf(r), "request"); err != nil {
		return err
	}
	if r.Schema != "wendy.world.go2-cloud-check.v1" || !r.Simulation {
		return errors.New("explicit simulation request required")
	}
	o := r.Observation.cloud()
	g, evidence, err := go2nav.BuildCloud(o, r.Bounds, r.Profile)
	if err != nil {
		return fmt.Errorf("cloud refused: %w", err)
	}
	if r.Continuation != nil && r.Continuation.Pose != o.Scan.Robot {
		return errors.New("continuation must use original paired pose")
	}
	baseline, baselineErr := go2nav.Build(o.Scan, r.Bounds, r.Profile)
	h := sha256.Sum256(raw)
	result := map[string]any{"schema": "wendy.world.go2-cloud-check-result.v1", "simulation": true, "operational_authority": false,
		"input_sha256": hex.EncodeToString(h[:]), "evidence": evidence,
		"cloud": inspect(g, nil, o.Scan.Robot, r.Continuation), "horizontal": inspect(baseline, baselineErr, o.Scan.Robot, r.Continuation),
		"limits": []string{"Offline conditional finite-angular solid-column model; no physical clearance guarantee.", "Supplied digests and publisher identifiers are metadata, not authentication.", "No source clock refresh, live command, cached permission or motion integration occurs."}}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(encoded) > maxOutput {
		return errors.New("offline result exceeds bound")
	}
	_, err = out.Write(append(encoded, '\n'))
	return err
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: world-go2-cloud-check < request.json > result.json")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
