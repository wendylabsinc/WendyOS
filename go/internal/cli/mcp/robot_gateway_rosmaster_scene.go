package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
)

const rosmasterSceneID = "rosmaster-r2-v1"

// Adapt the existing R2 observer API without changing or restarting its runtime.
// Status contains controller ownership, so only pose fields leave this adapter.
type rosmasterSceneTransport struct {
	base                     http.RoundTripper
	mu                       sync.Mutex
	wheelbase, track, radius float64
}

func (t *rosmasterSceneTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || (req.URL.Path != "/api/scene" && req.URL.Path != "/api/scene/state") {
		return nil, fmt.Errorf("unsupported ROSMaster observer request")
	}
	upstream := req.Clone(req.Context())
	url := *req.URL
	upstream.URL = &url
	if req.URL.Path == "/api/scene/state" {
		upstream.URL.Path = "/api/status"
	}
	res, err := t.base.RoundTrip(upstream)
	if err != nil || res.StatusCode != http.StatusOK {
		return res, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return nil, fmt.Errorf("ROSMaster observer response exceeds its limit")
	}
	var output any
	if req.URL.Path == "/api/scene" {
		var world map[string]any
		if err := json.Unmarshal(body, &world); err != nil {
			return nil, err
		}
		geometry, ok := world["geometry"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ROSMaster geometry is unavailable")
		}
		numbers := make([]float64, 3)
		for _, key := range []string{"length", "width"} {
			value, ok := geometry[key].(float64)
			if !ok || value <= 0 || value > 10 {
				return nil, fmt.Errorf("invalid ROSMaster geometry")
			}
		}
		for i, key := range []string{"wheelbase", "track", "wheel_radius"} {
			value, ok := geometry[key].(float64)
			if !ok || value <= 0 || value > 10 {
				return nil, fmt.Errorf("invalid ROSMaster geometry")
			}
			numbers[i] = value
		}
		t.mu.Lock()
		t.wheelbase, t.track, t.radius = numbers[0], numbers[1], numbers[2]
		t.mu.Unlock()
		shape := map[string]any{}
		for _, key := range []string{"wheelbase", "track", "wheel_radius", "length", "width"} {
			shape[key] = geometry[key]
		}
		half, ok := world["room_half_size"].(float64)
		if !ok || half <= 0 || half > 100 {
			return nil, fmt.Errorf("invalid ROSMaster room geometry")
		}
		lidar, ok := world["lidar_offset"].([]any)
		if !ok || len(lidar) != 3 {
			return nil, fmt.Errorf("invalid ROSMaster sensor geometry")
		}
		for _, item := range lidar {
			value, ok := item.(float64)
			if !ok || math.Abs(value) > 10 {
				return nil, fmt.Errorf("invalid ROSMaster sensor geometry")
			}
		}
		obstacles, ok := world["obstacles"].([]any)
		if !ok || len(obstacles) > 10000 {
			return nil, fmt.Errorf("invalid ROSMaster obstacle geometry")
		}
		boxes := make([]map[string]float64, 0, len(obstacles))
		for _, item := range obstacles {
			obstacle, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid ROSMaster obstacle geometry")
			}
			box := map[string]float64{}
			for _, key := range []string{"x", "y", "width", "depth", "height"} {
				value, ok := obstacle[key].(float64)
				if !ok || math.Abs(value) > 1000 || (key != "x" && key != "y" && value <= 0) {
					return nil, fmt.Errorf("invalid ROSMaster obstacle geometry")
				}
				box[key] = value
			}
			boxes = append(boxes, box)
		}
		output = map[string]any{"profile": "rosmaster-r2", "id": rosmasterSceneID, "geometry": shape, "lidar_offset": lidar, "room_half_size": half, "obstacles": boxes}
	} else {
		var status struct {
			Epoch   int    `json:"epoch"`
			Mode    string `json:"mode"`
			Healthy bool   `json:"healthy"`
			State   struct {
				X, Y, Yaw, Elapsed float64
				Steering           []float64 `json:"steering_angles"`
				Wheels             []float64 `json:"wheel_positions"`
			} `json:"state"`
		}
		if err := json.Unmarshal(body, &status); err != nil {
			return nil, err
		}
		t.mu.Lock()
		wheelbase, track, radius := t.wheelbase, t.track, t.radius
		t.mu.Unlock()
		if wheelbase == 0 || len(status.State.Steering) != 2 || len(status.State.Wheels) != 4 {
			return nil, fmt.Errorf("ROSMaster scene geometry must load before poses")
		}
		if status.Mode != "idle" && status.Mode != "driving" && status.Mode != "paused" {
			return nil, fmt.Errorf("invalid ROSMaster observer mode")
		}
		values := append([]float64{status.State.X, status.State.Y, status.State.Yaw, status.State.Elapsed}, status.State.Steering...)
		values = append(values, status.State.Wheels...)
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e12 {
				return nil, fmt.Errorf("invalid ROSMaster observer pose")
			}
		}
		positions := []float64{0, 0, 0, status.State.X, status.State.Y, 0}
		quaternions := []float64{0, 0, 0, 1, 0, 0, math.Sin(status.State.Yaw / 2), math.Cos(status.State.Yaw / 2)}
		for i, offset := range [][2]float64{{wheelbase, track / 2}, {wheelbase, -track / 2}, {0, track / 2}, {0, -track / 2}} {
			c, s := math.Cos(status.State.Yaw), math.Sin(status.State.Yaw)
			positions = append(positions, status.State.X+c*offset[0]-s*offset[1], status.State.Y+s*offset[0]+c*offset[1], radius)
			yaw := status.State.Yaw
			if i < 2 {
				yaw += status.State.Steering[i]
			}
			// Z steering followed by Y wheel rotation, in Three.js XYZW order.
			sy, cy, sw, cw := math.Sin(yaw/2), math.Cos(yaw/2), math.Sin(status.State.Wheels[i]/2), math.Cos(status.State.Wheels[i]/2)
			quaternions = append(quaternions, -sy*sw, cy*sw, sy*cw, cy*cw)
		}
		output = map[string]any{"scene_id": rosmasterSceneID, "epoch": status.Epoch, "generation": 1, "time": status.State.Elapsed, "mode": status.Mode, "valid": status.Healthy, "positions": positions, "quaternions": quaternions}
	}
	body, err = json.Marshal(output)
	if err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header = res.Header.Clone()
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	res.Header.Set("Content-Type", "application/json")
	res.Header.Del("Content-Encoding")
	return res, nil
}
