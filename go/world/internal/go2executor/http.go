package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/go2project"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const maxResponse = 256 << 10

type HTTPAdapter struct {
	endpoint, token string
	client          *http.Client
	receiptMu       sync.Mutex
	receipt         *BridgeCommandReceipt
}

// BridgeResponseError retains only a numeric HTTP status and a known diagnostic
// enum. It never contains response bodies, tokens, URLs or arbitrary exceptions.
type BridgeResponseError struct {
	Status int    `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func (e *BridgeResponseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("bridge response refused: HTTP %d (%s)", e.Status, e.Reason)
	}
	return fmt.Sprintf("bridge response refused: HTTP %d", e.Status)
}

// This static list mirrors sim/go2_bridge/__main__.py. Unknown response text is
// discarded; adding a bridge reason requires an explicit decoder change.
func knownBridgeRefusal(reason string) bool {
	switch reason {
	case "request_refused", "command_source_nonadvancing", "command_budget_exhausted",
		"command_task_identity", "combined_axis_command", "capture_not_served",
		"served_capture_stale", "session_mismatch", "clock_fault_latched",
		"clock_continuity", "decision_expired", "explicit_stop",
		"publication_outcome_unknown", "publication_completed_late",
		"runtime_unavailable", "runtime_identity", "runtime_owner_lost",
		"runtime_not_ready", "runtime_project", "runtime_project_epoch",
		"runtime_time_regression", "automatic_control_unsupported",
		"sensor_unavailable", "sensor_stale", "sensor_pair_skew",
		"sensor_identity_changed", "sensor_epoch_changed", "sensor_startup_rejected",
		"unsupported_body_tilt", "odometry_epoch_age_exceeded",
		"planar_uncertainty_exceeded", "tilted_height_outside_commissioning",
		"tilted_pair_uncertainty_exceeded":
		return true
	case "cloud_profile_mismatch", "cloud_capture_invalid", "cloud_capture_changed", "cloud_evidence_unavailable":
		return true
	}
	return false
}

func bridgeResponseError(response *http.Response) error {
	out := &BridgeResponseError{Status: response.StatusCode}
	if response.StatusCode != http.StatusConflict || response.ContentLength > 1024 || response.Header.Get("Content-Encoding") != "" {
		return out
	}
	// This reads the already received error response under the original request
	// deadline. It makes no diagnostic HTTP request and cannot renew authority.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1025))
	if err != nil || len(raw) > 1024 {
		return out
	}
	var body struct {
		Code   string `json:"error"`
		Reason string `json:"reason"`
	}
	if authority.DecodeStrict(raw, &body) == nil && body.Code == "bridge_refused" && knownBridgeRefusal(body.Reason) {
		out.Reason = body.Reason
	}
	return out
}

func NewHTTPAdapter(endpoint, token string) (*HTTPAdapter, error) {
	u, err := go2project.Endpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if len(token) < 32 || len(token) > 256 || strings.ContainsAny(token, "\r\n\t ") {
		return nil, errors.New("bounded private bridge token required")
	}
	return &HTTPAdapter{endpoint: u, token: token, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 8192}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("bridge redirects forbidden") }}}, nil
}
func (a *HTTPAdapter) call(ctx context.Context, path string, input any) ([]byte, error) {
	limit := 250 * time.Millisecond
	responseBound := maxResponse
	if path == "/v1/diagnostics" {
		limit = 100 * time.Millisecond
		responseBound = maxStartupDiagnosticBytes
	}
	if path == "/v1/session" {
		limit = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", a.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+a.token)
	response, err := a.client.Do(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > int64(responseBound) || response.Header.Get("Content-Encoding") != "" {
		return nil, bridgeResponseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(responseBound)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > responseBound {
		return nil, errors.New("bridge response exceeds bound")
	}
	return data, nil
}
func (a *HTTPAdapter) Session(ctx context.Context, b tasking.Go2Binding) (State, error) {
	data, e := a.call(ctx, "/v1/session", struct {
		Binding tasking.Go2Binding `json:"binding"`
	}{b})
	if e != nil {
		return State{}, e
	}
	return DecodeState(data)
}
func (a *HTTPAdapter) State(ctx context.Context, s string) (State, error) {
	data, e := a.call(ctx, "/v1/state", struct {
		Session string `json:"session"`
	}{s})
	if e != nil {
		return State{}, e
	}
	return DecodeState(data)
}
func (a *HTTPAdapter) Begin(ctx context.Context, r BeginRequest) error {
	data, e := a.call(ctx, "/v1/begin", r)
	if e != nil {
		return e
	}
	return accepted(data)
}
func (a *HTTPAdapter) Stop(ctx context.Context, r StopRequest) error {
	data, e := a.call(ctx, "/v1/stop", r)
	if e != nil {
		return e
	}
	return accepted(data)
}
func accepted(data []byte) error {
	var r struct {
		Accepted bool `json:"accepted"`
	}
	if authority.DecodeStrict(data, &r) != nil || !r.Accepted {
		return errors.New("bridge acknowledgment invalid")
	}
	return nil
}
func (a *HTTPAdapter) Command(ctx context.Context, r CommandRequest) error {
	a.receiptMu.Lock()
	a.receipt = nil
	a.receiptMu.Unlock()
	started := time.Now()
	data, e := a.call(ctx, "/v1/command", r)
	completed := time.Now()
	if e != nil {
		return e
	}
	var ack BridgeCommandAcknowledgment
	if authority.DecodeStrict(data, &ack) != nil || !acknowledgmentMatches(ack, r) {
		return errors.New("bridge fixed timestamp acknowledgment invalid")
	}
	a.receiptMu.Lock()
	a.receipt = &BridgeCommandReceipt{Request: r, Acknowledgment: ack, RequestStartedUnixNS: started.UnixNano(), ResponseCompletedUnixNS: completed.UnixNano(), RoundTripElapsedNS: completed.Sub(started).Nanoseconds()}
	a.receiptMu.Unlock()
	return nil
}

func (a *HTTPAdapter) LastCommandReceipt() (BridgeCommandReceipt, bool) {
	a.receiptMu.Lock()
	defer a.receiptMu.Unlock()
	if a.receipt == nil {
		return BridgeCommandReceipt{}, false
	}
	return *a.receipt, true
}

type wirePose struct {
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	Yaw float64 `json:"yaw"`
}

func (p wirePose) pose() go2nav.Pose { return go2nav.Pose{X: p.X, Y: p.Y, Yaw: p.Yaw} }

type wireScan struct {
	TiltedPose           *go2nav.TiltedPose `json:"tilted_pose,omitempty"`
	Sequence             uint64             `json:"sequence"`
	CapturedUnixNS       int64              `json:"captured_unix_ns"`
	WorldEpoch           uint64             `json:"world_epoch"`
	Frame                string             `json:"frame"`
	Ranges               []*float64         `json:"ranges"`
	AngleMin             float64            `json:"angle_min"`
	AngleIncrement       float64            `json:"angle_increment"`
	RangeMin             float64            `json:"range_min"`
	RangeMax             float64            `json:"range_max"`
	Robot                wirePose           `json:"robot"`
	SensorMount          wirePose           `json:"sensor_mount"`
	MountKnown           bool               `json:"mount_known"`
	PositionUncertaintyM float64            `json:"position_uncertainty_m"`
	YawUncertaintyRad    float64            `json:"yaw_uncertainty_rad"`
	UncertaintyKnown     bool               `json:"uncertainty_known"`
}

type wireCloud struct {
	Model               string     `json:"model"`
	SourceDigest        string     `json:"source_digest"`
	Frame               string     `json:"frame"`
	PublisherGID        string     `json:"publisher_gid"`
	SerializedSHA256    string     `json:"serialized_sha256"`
	CapturedUnixNS      int64      `json:"captured_unix_ns"`
	Ranges              []*float64 `json:"ranges"`
	Sequence            uint64     `json:"sequence"`
	ObservedEpoch       uint64     `json:"observed_epoch"`
	ScanSHA256          string     `json:"scan_sha256"`
	OdomSHA256          string     `json:"odom_sha256"`
	EpochSHA256         string     `json:"epoch_sha256"`
	MountSHA256         string     `json:"mount_sha256"`
	CloudMountSHA256    string     `json:"cloud_mount_sha256"`
	EpochPublisher      string     `json:"epoch_publisher"`
	MountPublisher      string     `json:"mount_publisher"`
	CloudMountPublisher string     `json:"cloud_mount_publisher"`
}

func requiredObject(raw []byte, out any) error {
	return requiredObjectLimit(raw, out, authority.MaxEnvelopeBytes)
}
func requiredObjectLimit(raw []byte, out any, limit int) error {
	if err := authority.DecodeStrictLimit(raw, out, limit); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	b, _ := json.Marshal(out)
	var expected map[string]json.RawMessage
	_ = json.Unmarshal(b, &expected)
	if len(fields) != len(expected) {
		return errors.New("bridge fields missing")
	}
	for k := range expected {
		if len(fields[k]) == 0 || bytes.Equal(bytes.TrimSpace(fields[k]), []byte("null")) {
			return errors.New("bridge field absent or null")
		}
	}
	return nil
}

// decodeRootCheckedCloud is used only after DecodeState's strict whole-body
// pass has rejected duplicate fields, excessive depth and trailing data. Check
// this fixed wire shape directly instead of serializing all 9,000 ranges again
// merely to discover the struct's field names. Numeric/null range validation
// still follows decoding, including conversion of missing returns to +Inf.
func decodeRootCheckedCloud(raw []byte, out *wireCloud) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	expected := [...]string{"model", "source_digest", "frame", "publisher_gid", "serialized_sha256", "captured_unix_ns", "ranges", "sequence", "observed_epoch", "scan_sha256", "odom_sha256", "epoch_sha256", "mount_sha256", "cloud_mount_sha256", "epoch_publisher", "mount_publisher", "cloud_mount_publisher"}
	if len(fields) != len(expected) {
		return errors.New("bridge fields missing")
	}
	for _, key := range expected {
		if len(fields[key]) == 0 || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return errors.New("bridge field absent or null")
		}
	}
	return json.Unmarshal(raw, out)
}

func numericArray(raw []byte, n int) error {
	var a []*float64
	if json.Unmarshal(raw, &a) != nil || len(a) != n {
		return errors.New("bridge numeric array has wrong shape")
	}
	for _, v := range a {
		if v == nil || !finite(*v) {
			return errors.New("bridge numeric entry unavailable")
		}
	}
	return nil
}
func DecodeState(raw []byte) (State, error) {
	var s State
	if len(raw) > maxResponse {
		return s, errors.New("bridge state exceeds bound")
	}
	// Decode nested scalar shapes explicitly; Go fixed arrays otherwise accept
	// short arrays and JSON null can silently invent zero pose coordinates.
	var fields map[string]json.RawMessage
	if err := authority.DecodeStrictLimit(raw, &fields, maxResponse); err != nil {
		return s, err
	}
	poseRaw, scanRaw := fields["pose"], fields["scan"]
	cloudRaw, hasCloud := fields["cloud"]
	delete(fields, "pose")
	delete(fields, "scan")
	delete(fields, "cloud")
	base, _ := json.Marshal(fields)
	if err := requiredObject(base, &s); err != nil {
		return s, err
	}
	for k, n := range map[string]int{"body_velocity": 3, "previous_command": 3, "pose_covariance": 36, "twist_covariance": 36} {
		if err := numericArray(fields[k], n); err != nil {
			return s, err
		}
	}
	var p wirePose
	if err := requiredObject(poseRaw, &p); err != nil {
		return s, err
	}
	s.Pose = p.pose()
	var scan wireScan
	if err := requiredObject(scanRaw, &scan); err != nil {
		return s, err
	}
	var scanFields map[string]json.RawMessage
	_ = json.Unmarshal(scanRaw, &scanFields)
	if err := requiredObject(scanFields["robot"], &scan.Robot); err != nil {
		return s, err
	}
	if err := requiredObject(scanFields["sensor_mount"], &scan.SensorMount); err != nil {
		return s, err
	}
	if scan.TiltedPose != nil {
		if err := requiredObject(scanFields["tilted_pose"], scan.TiltedPose); err != nil {
			return s, err
		}
		var tf map[string]json.RawMessage
		_ = json.Unmarshal(scanFields["tilted_pose"], &tf)
		for k, n := range map[string]int{"quaternion_wxyz": 4, "odom_position_xyz": 3, "mount_translation_xyz": 3, "mount_quaternion_wxyz": 4} {
			if err := numericArray(tf[k], n); err != nil {
				return s, err
			}
		}
	}
	if len(scan.Ranges) < 180 || len(scan.Ranges) > 8192 {
		return s, errors.New("scan size outside bound")
	}
	ranges := make([]float64, len(scan.Ranges))
	for i, r := range scan.Ranges {
		if r == nil {
			ranges[i] = math.Inf(1)
		} else if !finite(*r) {
			return s, errors.New("nonfinite JSON range")
		} else {
			ranges[i] = *r
		}
	}
	s.Scan = go2nav.Scan{TiltedPose: scan.TiltedPose, Sequence: scan.Sequence, CapturedUnixNS: scan.CapturedUnixNS, WorldEpoch: scan.WorldEpoch, Frame: scan.Frame, Ranges: ranges, AngleMin: scan.AngleMin, AngleIncrement: scan.AngleIncrement, RangeMin: scan.RangeMin, RangeMax: scan.RangeMax, Robot: scan.Robot.pose(), SensorMount: scan.SensorMount.pose(), MountKnown: scan.MountKnown, PositionUncertaintyM: scan.PositionUncertaintyM, YawUncertaintyRad: scan.YawUncertaintyRad, UncertaintyKnown: scan.UncertaintyKnown}
	if hasCloud {
		var cloud wireCloud
		if err := decodeRootCheckedCloud(cloudRaw, &cloud); err != nil {
			return s, err
		}
		if len(cloud.Ranges) != go2nav.CloudRayCount {
			return s, errors.New("cloud size outside bound")
		}
		ranges := make([]float64, len(cloud.Ranges))
		for i, r := range cloud.Ranges {
			if r == nil {
				ranges[i] = math.Inf(1)
			} else if !finite(*r) {
				return s, errors.New("nonfinite JSON cloud range")
			} else {
				ranges[i] = *r
			}
		}
		s.Cloud = &CloudState{Model: cloud.Model, SourceDigest: cloud.SourceDigest, Frame: cloud.Frame,
			PublisherGID: cloud.PublisherGID, SerializedSHA256: cloud.SerializedSHA256,
			CapturedUnixNS: cloud.CapturedUnixNS, Ranges: ranges, Sequence: cloud.Sequence,
			ObservedEpoch: cloud.ObservedEpoch, ScanSHA256: cloud.ScanSHA256, OdomSHA256: cloud.OdomSHA256,
			EpochSHA256: cloud.EpochSHA256, MountSHA256: cloud.MountSHA256, CloudMountSHA256: cloud.CloudMountSHA256,
			EpochPublisher: cloud.EpochPublisher, MountPublisher: cloud.MountPublisher,
			CloudMountPublisher: cloud.CloudMountPublisher}
		if err := validateCloudState(s); err != nil {
			return s, err
		}
	}
	return s, nil
}
