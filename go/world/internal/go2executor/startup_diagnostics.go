package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

const maxStartupDiagnosticBytes = 8 << 10

// BridgeDiagnostics is a rejected observation, never accepted state or motion
// authority. Nullable evidence stays unknown; it is not converted into zero.
type BridgeDiagnostics struct {
	Schema               string          `json:"schema"`
	Simulation           bool            `json:"simulation"`
	OperationalAuthority bool            `json:"operational_authority"`
	RejectedSensor       *RejectedSensor `json:"rejected_sensor"`
}
type RejectedSensor struct {
	Schema                   string      `json:"schema"`
	Topic                    string      `json:"topic"`
	Reason                   string      `json:"reason"`
	WorldEpoch               *uint64     `json:"world_epoch"`
	PublisherGID             *string     `json:"publisher_gid"`
	CapturedUnixNS           *int64      `json:"captured_unix_ns"`
	QuaternionWXYZ           *[4]float64 `json:"quaternion_wxyz"`
	NormalizedQuaternionWXYZ *[4]float64 `json:"normalized_quaternion_wxyz"`
	OdomPositionXYZ          *[3]float64 `json:"odom_position_xyz"`
	RollRad                  *float64    `json:"roll_rad"`
	PitchRad                 *float64    `json:"pitch_rad"`
}

var diagnosticReason = regexp.MustCompile(`^[a-z_]{1,64}$`)
var diagnosticGID = regexp.MustCompile(`^[0-9a-f]{48}$`)

// requireDiagnosticKeys accepts explicit null only for listed nullable fields.
func requireDiagnosticKeys(raw []byte, keys []string, nullable map[string]bool) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if e := authority.DecodeStrict(raw, &fields); e != nil {
		return nil, e
	}
	if len(fields) != len(keys) {
		return nil, errors.New("diagnostic fields missing")
	}
	for _, k := range keys {
		v, ok := fields[k]
		if !ok || (!nullable[k] && bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return nil, errors.New("diagnostic evidence absent")
		}
	}
	return fields, nil
}
func DecodeBridgeDiagnostics(raw []byte) (*BridgeDiagnostics, error) {
	if len(raw) == 0 || len(raw) > maxStartupDiagnosticBytes {
		return nil, errors.New("startup diagnostic exceeds bound")
	}
	var d BridgeDiagnostics
	if e := authority.DecodeStrict(raw, &d); e != nil {
		return nil, e
	}
	fields, e := requireDiagnosticKeys(raw, []string{"schema", "simulation", "operational_authority", "rejected_sensor"}, map[string]bool{"rejected_sensor": true})
	if e != nil {
		return nil, e
	}
	if d.Schema != "wendy.world.go2-bridge-diagnostics.v1" || !d.Simulation || d.OperationalAuthority {
		return nil, errors.New("diagnostic domain invalid")
	}
	if d.RejectedSensor == nil {
		return &d, nil
	}
	r := d.RejectedSensor
	optional := map[string]bool{"world_epoch": true, "publisher_gid": true, "captured_unix_ns": true, "quaternion_wxyz": true, "normalized_quaternion_wxyz": true, "odom_position_xyz": true, "roll_rad": true, "pitch_rad": true}
	f, e := requireDiagnosticKeys(fields["rejected_sensor"], []string{"schema", "topic", "reason", "world_epoch", "publisher_gid", "captured_unix_ns", "quaternion_wxyz", "normalized_quaternion_wxyz", "odom_position_xyz", "roll_rad", "pitch_rad"}, optional)
	if e != nil {
		return nil, e
	}
	if r.Schema != "wendy.world.go2-rejected-sensor.v1" || !diagnosticReason.MatchString(r.Reason) || (r.WorldEpoch != nil && *r.WorldEpoch > math.MaxInt64) {
		return nil, errors.New("rejected observation labels invalid")
	}
	switch r.Topic {
	case "odom", "scan", "epoch", "mount", "unknown":
	default:
		return nil, errors.New("rejected observation topic invalid")
	}
	if (r.PublisherGID != nil && !diagnosticGID.MatchString(*r.PublisherGID)) || (r.CapturedUnixNS != nil && *r.CapturedUnixNS <= 0) {
		return nil, errors.New("rejected observation metadata invalid")
	}
	for k, n := range map[string]int{"quaternion_wxyz": 4, "normalized_quaternion_wxyz": 4, "odom_position_xyz": 3} {
		if !bytes.Equal(bytes.TrimSpace(f[k]), []byte("null")) {
			if e := numericArray(f[k], n); e != nil {
				return nil, e
			}
		}
	}
	if r.QuaternionWXYZ != nil {
		for _, v := range *r.QuaternionWXYZ {
			if math.Abs(v) > 1 {
				return nil, errors.New("raw quaternion outside diagnostic bound")
			}
		}
	}
	if r.NormalizedQuaternionWXYZ != nil {
		if _, _, _, e := go2nav.QuaternionRPY(*r.NormalizedQuaternionWXYZ); e != nil {
			return nil, e
		}
	}
	if r.OdomPositionXYZ != nil {
		for _, v := range *r.OdomPositionXYZ {
			if math.Abs(v) > 100 {
				return nil, errors.New("rejected position outside bound")
			}
		}
	}
	if (r.RollRad != nil && (!finite(*r.RollRad) || math.Abs(*r.RollRad) > math.Pi)) || (r.PitchRad != nil && (!finite(*r.PitchRad) || math.Abs(*r.PitchRad) > math.Pi/2)) {
		return nil, errors.New("rejected tilt outside bound")
	}
	return &d, nil
}
func (a *HTTPAdapter) StartupDiagnostics(ctx context.Context) (*BridgeDiagnostics, error) {
	data, e := a.call(ctx, "/v1/diagnostics", struct{}{})
	if e != nil {
		return nil, e
	}
	return DecodeBridgeDiagnostics(data)
}
func copyBridgeDiagnostics(d *BridgeDiagnostics) *BridgeDiagnostics {
	if d == nil {
		return nil
	}
	b, e := json.Marshal(d)
	if e != nil {
		return nil
	}
	out, e := DecodeBridgeDiagnostics(b)
	if e != nil {
		return nil
	}
	return out
}
func (c *Controller) captureStartupDiagnostics() {
	adapter, ok := c.adapter.(interface {
		StartupDiagnostics(context.Context) (*BridgeDiagnostics, error)
	})
	if !ok || c.faultSnapshot == nil {
		return
	}
	// Only after the existing latch and bounded brake attempt. No retries or
	// additional state/control calls, and the original failure always survives.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	d, e := adapter.StartupDiagnostics(ctx)
	if e != nil || ctx.Err() != nil {
		return
	}
	c.faultSnapshot.BridgeDiagnostics = copyBridgeDiagnostics(d)
}
