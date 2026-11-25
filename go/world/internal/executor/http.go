package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type NodeGateway struct{ Client *client.Client }

func (n NodeGateway) Begin(ctx context.Context) (uint64, error) {
	var response struct {
		Epoch uint64 `json:"executor_epoch"`
	}
	err := n.Client.Do(ctx, "execute", "POST", "/v1/tasks/executor/start", []byte("{}"), &response)
	return response.Epoch, err
}
func (n NodeGateway) Tasks(ctx context.Context) ([]tasking.Record, error) {
	var response struct {
		Tasks []tasking.Record `json:"tasks"`
	}
	err := n.Client.Do(ctx, "read", "GET", "/v1/tasks", nil, &response)
	return response.Tasks, err
}
func (n NodeGateway) Evaluate(ctx context.Context, id string, epoch uint64, t tasking.Telemetry) (tasking.Decision, error) {
	body, err := json.Marshal(map[string]any{"task_id": id, "executor_epoch": epoch, "telemetry": t})
	if err != nil {
		return tasking.Decision{}, err
	}
	var result tasking.Decision
	err = n.Client.Do(ctx, "execute", "POST", "/v1/tasks/evaluate", body, &result)
	return result, err
}

type HTTPAdapter struct {
	URL, Token string
	http       *http.Client
	decode     func([]byte) (State, error)
}

func NewHTTPAdapter(address, token string) (*HTTPAdapter, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || len(token) < 32 || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid isolated simulation adapter URL or token")
	}
	return &HTTPAdapter{URL: strings.TrimSuffix(address, "/"), Token: token, http: &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (a *HTTPAdapter) call(ctx context.Context, method, path string, input any) (State, error) {
	var b []byte
	var err error
	if input != nil {
		b, err = json.Marshal(input)
		if err != nil {
			return State{}, err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, a.URL+path, bytes.NewReader(b))
	if err != nil {
		return State{}, err
	}
	r.Header.Set("Authorization", "Bearer "+a.Token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(r)
	if err != nil {
		return State{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return State{}, err
	}
	if len(body) > 65536 {
		return State{}, errors.New("adapter response too large")
	}
	if resp.StatusCode != 200 {
		return State{}, fmt.Errorf("adapter HTTP %d", resp.StatusCode)
	}
	if a.decode != nil {
		return a.decode(body)
	}
	return decodeState(body)
}

// Decode through raw fields before fixed arrays. encoding/json otherwise fills
// omitted/short arrays and null numeric entries with zero, which could invent a
// measured target completion. Extra diagnostic fields are permitted.
func decodeState(body []byte) (State, error) {
	state, err := decodeStateFields(body, false)
	if err != nil {
		return State{}, err
	}
	return state, validateState(state)
}

func decodeStateFields(body []byte, planar bool) (State, error) {
	var fields map[string]json.RawMessage
	if err := authority.DecodeStrict(body, &fields); err != nil {
		return State{}, err
	}
	for _, key := range []string{"session", "boot_id", "sequence", "position_m", "velocity_mps", "commanded_velocity_mps", "sample_unix_ms", "simulation_time_s", "frame", "sensors", "obstacle_clear", "energy", "synthetic"} {
		v, exists := fields[key]
		if !exists || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return State{}, fmt.Errorf("missing adapter state field %s", key)
		}
	}
	for _, key := range []string{"position_m", "velocity_mps"} {
		var values []json.RawMessage
		if err := json.Unmarshal(fields[key], &values); err != nil || len(values) != 3 {
			return State{}, fmt.Errorf("%s must contain three numbers", key)
		}
		for _, v := range values {
			var number *float64
			if err := json.Unmarshal(v, &number); err != nil || number == nil {
				return State{}, fmt.Errorf("invalid %s entry", key)
			}
		}
	}
	var sensors map[string]json.RawMessage
	if err := json.Unmarshal(fields["sensors"], &sensors); err != nil {
		return State{}, err
	}
	for _, key := range []string{"gas", "local-obstacles"} {
		var available *bool
		if err := json.Unmarshal(sensors[key], &available); err != nil || available == nil {
			return State{}, fmt.Errorf("missing sensor availability %s", key)
		}
	}
	var state State
	if planar {
		var values []json.RawMessage
		if json.Unmarshal(fields["commanded_velocity_mps"], &values) != nil || len(values) != 3 {
			return State{}, errors.New("planar commanded velocity requires three numbers")
		}
		for _, v := range values {
			var number *float64
			if json.Unmarshal(v, &number) != nil || number == nil {
				return State{}, errors.New("invalid planar commanded velocity")
			}
		}
		wire := struct {
			*State
			Commanded [3]float64 `json:"commanded_velocity_mps"`
		}{State: &state}
		if err := json.Unmarshal(body, &wire); err != nil {
			return State{}, err
		}
		state.PlanarCommandedVelocity = wire.Commanded
		state.CommandedVelocity = math.Hypot(wire.Commanded[0], wire.Commanded[2])
	} else if err := json.Unmarshal(body, &state); err != nil {
		return State{}, err
	}
	return state, nil
}
func (a *HTTPAdapter) Session(ctx context.Context) (State, error) {
	return a.call(ctx, "POST", "/v1/session", map[string]any{})
}
func (a *HTTPAdapter) State(ctx context.Context) (State, error) {
	return a.call(ctx, "GET", "/v1/state", nil)
}
func (a *HTTPAdapter) Command(ctx context.Context, session string, seq uint64, v float64, lease time.Duration) error {
	_, err := a.call(ctx, "POST", "/v1/command", map[string]any{"session": session, "sequence": seq, "velocity_mps": v, "lease_ms": lease.Milliseconds()})
	return err
}

// CommandUntil uses a distinct endpoint so an older adapter cannot silently
// ignore the absolute deadline. The simulator also enforces it on arrival.
func (a *HTTPAdapter) CommandUntil(ctx context.Context, session string, seq uint64, v float64, lease time.Duration, notAfterUnixMS int64) error {
	now := time.Now()
	if lease < time.Millisecond || lease > 500*time.Millisecond || notAfterUnixMS <= now.UnixMilli() || notAfterUnixMS-now.UnixMilli() > 500 {
		return errors.New("expired or invalid supervised command deadline")
	}
	bounded, cancel := context.WithDeadline(ctx, time.UnixMilli(notAfterUnixMS))
	defer cancel()
	_, err := a.call(bounded, "POST", "/v1/command/bounded", map[string]any{"session": session, "sequence": seq, "velocity_mps": v, "lease_ms": lease.Milliseconds(), "not_after_unix_ms": notAfterUnixMS})
	return err
}
func (a *HTTPAdapter) Stop(ctx context.Context, session string, seq uint64) error {
	_, err := a.call(ctx, "POST", "/v1/stop", map[string]any{"session": session, "sequence": seq})
	return err
}
