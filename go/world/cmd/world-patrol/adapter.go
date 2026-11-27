package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusiondemo"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type adapter struct {
	physics, nav, dir, runID string
	frame                    world.FrameRef
	client                   *http.Client
	fuser                    *fusiondemo.Evaluator
	latest                   *fusiondemo.Output
	lastCaptureNS            int64
	blocked                  atomic.Bool
	policyDigest             string
	beforeMotion             func() error
	inspectionGoal           func() bool
	beforeCapture            func() error
}

func (a *adapter) request(base, path string, body, out any) error {
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return errors.New("adapter response too large")
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("%s: %s: %.300s", path, response.Status, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
func (a *adapter) Plan(p patrol.Point) (patrol.Route, error) {
	var r patrol.Route
	err := a.request(a.nav, "/route", map[string]any{"x": p.X, "y": p.Y, "yaw": p.Yaw, "policy_digest": a.policyDigest}, &r)
	return r, err
}
func (a *adapter) Goal(id string, p patrol.Point) error {
	if a.beforeMotion != nil {
		if err := a.beforeMotion(); err != nil {
			return err
		}
	}
	var result struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	body := map[string]any{"id": id, "x": p.X, "y": p.Y, "yaw": p.Yaw, "policy_digest": a.policyDigest}
	if a.inspectionGoal != nil && a.inspectionGoal() {
		body["observation_mode"] = patroltour.NavigationMode
	}
	if err := a.request(a.nav, "/goal", body, &result); err != nil {
		return err
	}
	if !result.Accepted {
		return fmt.Errorf("goal refused: %s", result.Reason)
	}
	return nil
}
func (a *adapter) Cancel() error { return a.request(a.nav, "/cancel", struct{}{}, nil) }
func (a *adapter) Capture() (patrol.Capture, error) {
	if a.beforeCapture != nil {
		if err := a.beforeCapture(); err != nil {
			return patrol.Capture{}, err
		}
	}
	var c struct {
		Samples    []json.RawMessage `json:"samples"`
		Media      map[string][]byte `json:"media"`
		TimeS      float64           `json:"simulation_time_s"`
		CapturedMS int64             `json:"captured_unix_ms"`
	}
	requested := time.Now().UnixMilli()
	if err := a.request(a.physics, "/capture", struct{}{}, &c); err != nil {
		return patrol.Capture{}, err
	}
	if math.IsNaN(c.TimeS) || math.IsInf(c.TimeS, 0) || c.TimeS <= 0 {
		return patrol.Capture{}, errors.New("invalid original capture simulation time")
	}
	if c.CapturedMS < requested || c.CapturedMS > time.Now().UnixMilli() || time.Now().UnixMilli()-c.CapturedMS > 2000 {
		return patrol.Capture{}, errors.New("capture is not fresh")
	}
	required := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio", "lidar-go2": "lidar"}
	tick := fusiondemo.Tick{Media: c.Media}
	refs := []string{}
	seq := uint64(0)
	for _, raw := range c.Samples {
		s, err := sensing.Decode(raw)
		if err != nil {
			return patrol.Capture{}, err
		}
		if float64(s.SampleNS)/1e9 > c.TimeS+0.000001 || c.TimeS-float64(s.SampleNS)/1e9 > .15 {
			return patrol.Capture{}, errors.New("sample outside original capture window")
		}
		if required[s.SensorID] != s.Kind || s.RunID != a.runID || s.Frame != a.frame || s.SampleNS <= a.lastCaptureNS || seq != 0 && seq != s.Sequence {
			return patrol.Capture{}, errors.New("capture scope or sequence mismatch")
		}
		delete(required, s.SensorID)
		if tick.NowNS != 0 && (s.SampleNS-tick.NowNS > 150000000 || tick.NowNS-s.SampleNS > 150000000) {
			return patrol.Capture{}, errors.New("capture skew exceeds 150ms")
		}
		tick.NowNS = max(tick.NowNS, s.SampleNS)
		seq = s.Sequence
		if s.Evidence != nil {
			data, ok := c.Media[s.Evidence.SHA256]
			sum := sha256.Sum256(data)
			if !ok || int64(len(data)) != s.Evidence.Size || hex.EncodeToString(sum[:]) != s.Evidence.SHA256 {
				return patrol.Capture{}, errors.New("evidence missing or corrupt")
			}
			refs = append(refs, s.Evidence.SHA256)
		}
		if s.SensorID == "lidar-go2" {
			converter, err := sensing.New(sensing.Config{Tenant: fusiondemo.Tenant, Site: fusiondemo.Site, RunID: a.runID, Frame: a.frame, Source: world.Source{Device: "go2", App: fusiondemo.App, Service: s.SensorID, Stream: "samples"}, Epoch: 1, Anchor: fusiondemo.Anchor, TTL: fusiondemo.TTL})
			if err != nil {
				return patrol.Capture{}, err
			}
			component, err := converter.Convert(raw)
			if err != nil {
				return patrol.Capture{}, err
			}
			tick.Received = append(tick.Received, component)
		} else {
			tick.LocalSamples = append(tick.LocalSamples, raw)
		}
	}
	if len(required) != 0 {
		return patrol.Capture{}, errors.New("camera, LiDAR, gas or audio capture missing")
	}
	if a.beforeCapture != nil {
		if err := a.beforeCapture(); err != nil {
			return patrol.Capture{}, err
		}
	}
	output, err := a.fuser.Tick(context.Background(), tick)
	if err != nil {
		return patrol.Capture{}, err
	}
	// Keep each complete observation and its verified originals for later inspection.
	name := fmt.Sprintf("capture-%012d.json", seq)
	if err = atomicJSON(filepath.Join(a.dir, "captures", name), c); err != nil {
		return patrol.Capture{}, err
	}
	for _, s := range output.Observations {
		if s.Evidence != nil {
			ext := ".bin"
			switch s.Evidence.ContentType {
			case "image/png":
				ext = ".png"
			case "audio/wav":
				ext = ".wav"
			}
			if err = os.WriteFile(filepath.Join(a.dir, "evidence", s.Evidence.SHA256+ext), c.Media[s.Evidence.SHA256], 0600); err != nil {
				return patrol.Capture{}, err
			}
		}
	}
	manifest, _ := json.Marshal(map[string]any{"schema": "wendy.nav.patrol.capture.v1", "run_id": a.runID, "captured_unix_ms": c.CapturedMS, "samples": c.Samples})
	digest := sha256.Sum256(manifest)
	manifestID := hex.EncodeToString(digest[:])
	if err = os.WriteFile(filepath.Join(a.dir, "evidence", manifestID+".json"), manifest, 0600); err != nil {
		return patrol.Capture{}, err
	}
	refs = append(refs, manifestID)
	a.latest = &output
	a.lastCaptureNS = tick.NowNS
	result := patrol.Capture{TimeS: float64(tick.NowNS) / 1e9, Evidence: refs, ManifestSHA256: manifestID, SampleCount: len(c.Samples)}
	if output.Result.Hazard != nil {
		h := output.Result.Hazard
		result.Findings = append(result.Findings, patrol.Finding{ID: h.InputDigest, TimeS: result.TimeS, Text: h.Label, Evidence: refs})
	}
	// Independent gas observation remains reportable when multimodal fusion lacks visual support.
	for _, s := range output.Observations {
		if s.Gas != nil && s.Gas.ValuePPM >= 50 && output.Result.Hazard == nil {
			result.Findings = append(result.Findings, patrol.Finding{ID: s.ID, TimeS: result.TimeS, Text: fmt.Sprintf("Elevated simulated gas: %.1f ppm", s.Gas.ValuePPM), Evidence: refs})
		}
	}
	return result, nil
}
