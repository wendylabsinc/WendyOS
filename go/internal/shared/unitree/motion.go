package unitree

// Wire IDs/shape from unitree_webrtc_connect@e0abc5780761539eff89a382a79319e5cf6ad1f4
// constants.py and msgs/pub_sub.py. Do not copy/run its sportmode example: it
// changes modes and stands/moves the robot. Firmware acceptance is unverified.
import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Same limits as the Go2 simulator and its example apps. The physical Go2
// ignores forward requests below about 0.5 m/s, so lower caps stop it walking.
const MotionForwardLimit = 0.8
const MotionLateralLimit = 0.5
const MotionYawLimit = 1.0
const sportTopic = "rt/api/sport/request"
const avoidanceTopic = "rt/api/obstacles_avoid/request"

// SetObstacleAvoidance uses Unitree's ObstaclesAvoidClient.SwitchSet API.
// This changes only avoidance, not speed, gait, posture or locomotion mode.
func (c *Connection) SetObstacleAvoidance(ctx context.Context, enabled bool) error {
	parameter, _ := json.Marshal(map[string]bool{"enable": enabled})
	return c.apiRequest(ctx, avoidanceTopic, 1001, string(parameter))
}

func ValidMotion(forward, lateral, yaw float64) bool {
	within := func(v, limit float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= limit }
	return within(forward, MotionForwardLimit) && within(lateral, MotionLateralLimit) && within(yaw, MotionYawLimit)
}

// Move accepts only bounded planar velocity, never posture or mode changes.
// Like Unitree's SportClient.Move, velocity updates use policy.noreply. Success
// means the update was sent, not that the robot accepted or executed it.
// Zero velocity still uses acknowledged StopMove.
func (c *Connection) Move(ctx context.Context, forward, lateral, yaw float64) error {
	if !ValidMotion(forward, lateral, yaw) {
		return errors.New("Go2 velocity outside limits")
	}
	if forward == 0 && lateral == 0 && yaw == 0 {
		return c.Stop(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, motionAckTimeout)
	defer cancel()
	parameter, _ := json.Marshal(map[string]float64{"x": forward, "y": lateral, "z": yaw})
	for {
		_, _, err := c.sendSportRequest(ctx, 1008, string(parameter))
		if !errors.Is(err, errCommandBacklog) {
			return err
		}
		// Let a previous data-channel message drain before sending the newest
		// velocity. Do not hold motionMu here: Stop must be able to pass us.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("Go2 disconnected")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (c *Connection) Stop(ctx context.Context) error { return c.sportRequest(ctx, 1003, "") }

// StandUp uses RecoveryStand, which also works from lying down or a fall.
func (c *Connection) StandUp(ctx context.Context) error { return c.sportRequest(ctx, 1006, "") }
func (c *Connection) LieDown(ctx context.Context) error { return c.sportRequest(ctx, 1005, "") }
func (c *Connection) Hello(ctx context.Context) error   { return c.sportRequest(ctx, 1016, "") }

const motionAckTimeout = 200 * time.Millisecond

var errCommandBacklog = errors.New("Go2 command channel is backlogged")

// PostureAckTimeout allows the robot to finish a stand, lie-down or wave before
// acknowledging it; Move and StopMove retain the shorter motionAckTimeout.
const PostureAckTimeout = 3 * time.Second

func sportAckTimeout(api uint64) time.Duration {
	if api == 1008 || api == 1003 {
		return motionAckTimeout
	}
	return PostureAckTimeout
}

type pendingSportRequest struct {
	api      uint64
	response chan int
}

// sportRequest sends one request and waits for its acknowledgement. Requests
// are sent one at a time, but the wait happens outside motionMu, so a stop is
// not held up behind a posture change waiting for its slow acknowledgement.
func (c *Connection) sportRequest(ctx context.Context, api uint64, parameter string) error {
	return c.apiRequest(ctx, sportTopic, api, parameter)
}

func (c *Connection) apiRequest(ctx context.Context, topic string, api uint64, parameter string) error {
	ctx, cancel := context.WithTimeout(ctx, sportAckTimeout(api))
	defer cancel()
	id, response, err := c.sendAPIRequest(ctx, topic, api, parameter)
	if err != nil {
		return err
	}
	defer func() { c.responseMu.Lock(); delete(c.pending, id); c.responseMu.Unlock() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("Go2 disconnected before command acknowledgement")
	case code := <-response:
		if code != 0 {
			return errors.New("Go2 refused the teleop command")
		}
		return nil
	}
}

func (c *Connection) sendSportRequest(ctx context.Context, api uint64, parameter string) (uint64, chan int, error) {
	return c.sendAPIRequest(ctx, sportTopic, api, parameter)
}

func (c *Connection) sendAPIRequest(ctx context.Context, topic string, api uint64, parameter string) (uint64, chan int, error) {
	c.motionMu.Lock()
	defer c.motionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if !c.Connected() || c.channel == nil {
		return 0, nil, errors.New("Go2 command channel unavailable")
	}
	noReply := topic == sportTopic && api == 1008
	if noReply && c.channel.BufferedAmount() != 0 {
		return 0, nil, errCommandBacklog
	}
	// Keep numeric identifiers exactly representable by browser/robot JSON parsers.
	id := c.requestID.Add(1) % 2147483648
	var response chan int
	if !noReply {
		response = make(chan int, 1)
		c.responseMu.Lock()
		if c.pending == nil {
			c.pending = make(map[uint64]pendingSportRequest)
		}
		c.pending[id] = pendingSportRequest{api: api, response: response}
		c.responseMu.Unlock()
	}
	payload := struct {
		Type  string `json:"type"`
		Topic string `json:"topic"`
		Data  struct {
			Header struct {
				Identity struct {
					ID  uint64 `json:"id"`
					API uint64 `json:"api_id"`
				} `json:"identity"`
				Policy *struct {
					NoReply bool `json:"noreply"`
				} `json:"policy,omitempty"`
			} `json:"header"`
			Parameter string `json:"parameter"`
		} `json:"data"`
	}{Type: "req", Topic: topic}
	payload.Data.Header.Identity.ID, payload.Data.Header.Identity.API = id, api
	if noReply {
		payload.Data.Header.Policy = &struct {
			NoReply bool `json:"noreply"`
		}{true}
	}
	payload.Data.Parameter = parameter
	raw, _ := json.Marshal(payload)
	if err := c.channel.SendText(string(raw)); err != nil {
		c.responseMu.Lock()
		delete(c.pending, id)
		c.responseMu.Unlock()
		return 0, nil, errors.New("Go2 command send failed")
	}
	return id, response, nil
}

func (c *Connection) motionResponse(raw []byte) {
	var reply struct {
		Type  string `json:"type"`
		Topic string `json:"topic"`
		Data  struct {
			Header struct {
				Identity struct {
					ID  uint64 `json:"id"`
					API uint64 `json:"api_id"`
				} `json:"identity"`
				Status struct {
					Code *int `json:"code"`
				} `json:"status"`
			} `json:"header"`
		} `json:"data"`
	}
	// The reference future_resolver routes API replies by identity, not by
	// the request topic. Do not assume firmware echoes that topic on replies.
	if json.Unmarshal(raw, &reply) != nil || reply.Type != "res" || reply.Data.Header.Status.Code == nil {
		return
	}
	c.responseMu.Lock()
	defer c.responseMu.Unlock()
	if pending, ok := c.pending[reply.Data.Header.Identity.ID]; ok && pending.api == reply.Data.Header.Identity.API {
		select {
		case pending.response <- *reply.Data.Header.Status.Code:
		default:
		}
	}
}
