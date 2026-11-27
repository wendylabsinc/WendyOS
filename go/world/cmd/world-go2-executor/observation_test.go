package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

func commandObservation(sequence, capture uint64) *go2executor.CommandDiagnostic {
	return &go2executor.CommandDiagnostic{Request: go2executor.CommandRequest{TaskID: "task", ExecutorEpoch: 1, Sequence: sequence, CaptureSequence: capture}, ReceiptState: "unavailable", Outcome: "adapter_call_succeeded"}
}

func TestObservationCommandExactCaptureJoinAndDedup(t *testing.T) {
	l := observationLogState{}
	s := go2executor.State{CaptureSequence: 10}
	command := commandObservation(1, 10)
	events := l.events(s, true, command, "task", "run", 1)
	if len(events) != 1 || events[0]["command"] != command || events[0]["command_observation_join"] != "exact_capture_sequence" {
		t.Fatal("matching attempt did not join its consumed observation")
	}
	if again := l.events(s, true, command, "task", "run", 1); len(again) != 0 {
		t.Fatal("duplicate capture/command emitted again")
	}
	s.CaptureSequence = 11
	events = l.events(s, true, command, "task", "run", 1)
	if len(events) != 1 || events[0]["command"] != nil {
		t.Fatal("old acknowledgment was attached to a new observation")
	}
	command = commandObservation(2, 10)
	events = l.events(s, true, command, "task", "run", 1)
	if len(events) != 1 || events[0]["event"] != "executor_command" || events[0]["command_observation_join"] != "separate_event_no_current_capture_join" {
		t.Fatal("new attempt against older capture invented a current observation join")
	}
	s.CaptureSequence = 12
	command = commandObservation(3, 11)
	events = l.events(s, true, command, "task", "run", 1)
	if len(events) != 2 || events[0]["command"] != nil || events[1]["event"] != "executor_command" {
		t.Fatal("different current capture and command were conflated")
	}
}

func TestCommandLogScopeAndCaptureCaps(t *testing.T) {
	l := observationLogState{}
	command := commandObservation(1, 1)
	command.Request.TaskID = "another-task"
	if events := l.events(go2executor.State{}, false, command, "task", "run", 1); len(events) != 0 {
		t.Fatal("another task's command logged")
	}
	command.Request.TaskID = "task"
	command.Request.ExecutorEpoch = 2
	if events := l.events(go2executor.State{}, false, command, "task", "run", 1); len(events) != 0 {
		t.Fatal("another generation's command logged")
	}
	limits := 0
	for seq := uint64(1); seq <= maxObservationEvents+5; seq++ {
		for _, event := range l.events(go2executor.State{CaptureSequence: seq}, true, commandObservation(seq, seq), "task", "run", 1) {
			if event["event"] == "executor_diagnostic_limit" {
				if event["task_id"] != "task" || event["run_id"] != "run" || event["executor_epoch"] != uint64(1) {
					t.Fatal("limit record lost run scope")
				}
				limits++
			}
		}
	}
	if l.observations != maxObservationEvents || l.commands != maxCommandEvents || limits != 2 {
		t.Fatal("event caps were not explicit, bounded and one-shot")
	}
}

func TestObservationRetainsFullConsumedAttitudeAndNoRays(t *testing.T) {
	pose := &go2nav.TiltedPose{QuaternionWXYZ: [4]float64{.99, .01, .02, .03}, OdomPositionXYZ: [3]float64{1, 2, .3}, OdomCapturedUnixNS: 123, PairTranslationBoundM: .01, PairAttitudeBoundRad: .02}
	s := go2executor.State{CaptureSequence: 1, RollRad: .04, PitchRad: .05, OdomCapturedUnixNS: 123, ScanCapturedUnixNS: 125, Pose: go2nav.Pose{X: 1, Y: 2, Yaw: .06}, Scan: go2nav.Scan{TiltedPose: pose, Ranges: []float64{math.NaN()}}}
	event := observationEvent(s, "task", "run", 1)
	pose.OdomPositionXYZ[2] = 99
	var out bytes.Buffer
	if _, err := writeObservation(&out, event, maxObservationLogBytes); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["roll_rad"] != .04 || value["pitch_rad"] != .05 || value["tilted_pose"].(map[string]any)["odom_position_xyz"].([]any)[2] != .3 || bytes.Contains(out.Bytes(), []byte(`"ranges"`)) {
		t.Fatal("full consumed orientation changed or unbounded raw scan leaked into compact stream")
	}
	legacy := observationEvent(go2executor.State{}, "task", "run", 1)
	data, _ := json.Marshal(legacy)
	if !bytes.Contains(data, []byte(`"tilted_pose":null`)) {
		t.Fatal("legacy missing attitude was fabricated")
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestObservationSerializationBoundsAndFailure(t *testing.T) {
	var out bytes.Buffer
	if _, err := writeObservation(&out, map[string]any{"value": "larger than remaining"}, 1); !errors.Is(err, errObservationLimit) || out.Len() != 0 {
		t.Fatal("byte bound allowed partial diagnostic")
	}
	if _, err := writeObservation(&out, map[string]any{"value": math.NaN()}, maxObservationLogBytes); err == nil || out.Len() != 0 {
		t.Fatal("nonfinite observation invented JSON")
	}
	if _, err := writeObservation(shortWriter{}, map[string]any{"event": "test"}, maxObservationLogBytes); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short diagnostic write reported success")
	}
}

func TestCloudObservationMetadataIsDetachedAndOmitsRays(t *testing.T) {
	cloud := &go2executor.CloudState{Model: "coherent-multiring-slab-v1", SourceDigest: "source", Frame: "utlidar_lidar", PublisherGID: "publisher", SerializedSHA256: "cloud-hash", CapturedUnixNS: 123, Sequence: 7, ObservedEpoch: 28, ScanSHA256: "scan-hash", OdomSHA256: "odom-hash", EpochSHA256: "epoch-hash", MountSHA256: "mount-hash", CloudMountSHA256: "cloud-mount-hash", EpochPublisher: "epoch-publisher", MountPublisher: "mount-publisher", CloudMountPublisher: "cloud-mount-publisher", Ranges: []float64{math.NaN()}}
	event := observationEvent(go2executor.State{Cloud: cloud}, "task", "run", 1)
	cloud.SerializedSHA256 = "changed"
	metadata := event["cloud"].(map[string]any)
	if len(metadata) != 16 || metadata["serialized_sha256"] != "cloud-hash" || metadata["sequence"] != uint64(7) || metadata["captured_unix_ns"] != int64(123) {
		t.Fatal("cloud provenance omitted or aliased mutable state")
	}
	var out bytes.Buffer
	if _, err := writeObservation(&out, event, maxObservationLogBytes); err != nil || bytes.Contains(out.Bytes(), []byte(`"ranges"`)) {
		t.Fatal("cloud rays entered compact observation log", err)
	}
	if _, ok := observationEvent(go2executor.State{}, "task", "run", 1)["cloud"]; ok {
		t.Fatal("legacy state acquired fabricated cloud metadata")
	}
}
