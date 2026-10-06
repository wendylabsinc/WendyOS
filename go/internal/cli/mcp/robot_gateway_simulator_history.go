package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const simulatorCapturePeriod = time.Second / 30
const simulatorHistoryLease = 5 * time.Second
const simulatorHistoryBytes = 1 << 20

type simulatorPoseFrame struct {
	Sequence uint64          `json:"sequence"`
	Captured float64         `json:"captured_ms"`
	State    json.RawMessage `json:"state"`
}

type simulatorPoseHistory struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	changed  chan struct{}
	lastRead time.Time
	sequence uint64
	frames   []simulatorPoseFrame
	bytes    int
	err      error
}

func (h *simulatorPoseHistory) pause() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	h.frames, h.bytes = nil, 0
}

// Capture independently of the slower host tool round trip. Each session has
// one bounded observer request in flight; it stops on pause, idle or expiry.
func (g *RobotGateway) readSimulatorPoseHistory(ctx context.Context, id string, after uint64) ([]byte, error) {
	scene, err := g.authorizedSimulatorScene(ctx, id)
	if err != nil {
		return nil, err
	}
	h := &scene.history
	for {
		h.mu.Lock()
		if h.ctx != nil && h.ctx.Err() != nil && h.done != nil {
			done := h.done
			h.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		h.lastRead = time.Now()
		if h.ctx == nil {
			h.ctx, h.cancel = context.WithCancel(scene.ctx)
			h.done, h.changed = make(chan struct{}), make(chan struct{})
			h.err = nil
			go g.captureSimulatorPoses(id, scene, h.ctx, h.done)
		}
		if len(h.frames) > 0 {
			frames := make([]simulatorPoseFrame, 0, len(h.frames))
			for _, frame := range h.frames {
				if frame.Sequence > after {
					frames = append(frames, frame)
				}
			}
			// Always include the final pose for a caught-up reader.
			if len(frames) == 0 {
				frames = append(frames, h.frames[len(h.frames)-1])
			}
			packet := struct {
				Frames   []simulatorPoseFrame `json:"frames"`
				Sequence uint64               `json:"sequence"`
				Dropped  bool                 `json:"dropped"`
			}{frames, h.sequence, after > 0 && h.frames[0].Sequence > after+1}
			h.mu.Unlock()
			return json.Marshal(packet)
		}
		if h.err != nil {
			err := h.err
			h.mu.Unlock()
			return nil, err
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-scene.ctx.Done():
			return nil, fmt.Errorf("the viewer session ended. Choose Reconnect view")
		case <-ctx.Done():
			h.pause()
			return nil, ctx.Err()
		}
	}
}

func (g *RobotGateway) captureSimulatorPoses(id string, scene *gatewaySimulatorScene, ctx context.Context, done chan struct{}) {
	h := &scene.history
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.cancel()
		h.ctx, h.cancel, h.done = nil, nil, nil
		h.frames, h.bytes = nil, 0
		close(h.changed)
		close(done)
	}()
	identity := ""
	for {
		started := time.Now()
		h.mu.Lock()
		idle := time.Since(h.lastRead) >= simulatorHistoryLease
		h.mu.Unlock()
		if idle || ctx.Err() != nil {
			return
		}
		captureCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		body, err := g.readSimulatorScene(captureCtx, id, "state")
		cancel()
		var state struct {
			SceneID                string `json:"scene_id"`
			Epoch, Generation      uint64
			Time                   float64
			Positions, Quaternions []float64
		}
		if err == nil {
			if len(body) > simulatorHistoryBytes || json.Unmarshal(body, &state) != nil || state.SceneID == "" || len(state.Positions) == 0 || len(state.Quaternions)*3 != len(state.Positions)*4 {
				err = fmt.Errorf("invalid simulator pose history frame")
			}
		}
		h.mu.Lock()
		if ctx.Err() != nil {
			h.mu.Unlock()
			return
		}
		h.err = err
		if err == nil {
			current := fmt.Sprintf("%s:%d:%d", state.SceneID, state.Epoch, state.Generation)
			if current != identity || (len(h.frames) > 0 && state.Time < frameSimulationTime(h.frames[len(h.frames)-1])) {
				h.frames, h.bytes, identity = nil, 0, current
			}
			h.sequence++
			h.frames = append(h.frames, simulatorPoseFrame{h.sequence, float64(time.Since(scene.created)) / float64(time.Millisecond), json.RawMessage(body)})
			h.bytes += len(body)
			for len(h.frames) > 90 || h.bytes > simulatorHistoryBytes {
				h.bytes -= len(h.frames[0].State)
				h.frames = h.frames[1:]
			}
		}
		close(h.changed)
		h.changed = make(chan struct{})
		h.mu.Unlock()
		delay := max(time.Duration(0), simulatorCapturePeriod-time.Since(started))
		if err != nil {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func frameSimulationTime(frame simulatorPoseFrame) float64 {
	var state struct{ Time float64 }
	_ = json.Unmarshal(frame.State, &state)
	return state.Time
}
