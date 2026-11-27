// Package testtrack supplies explicitly synthetic lineage for transport tests.
// The callers sign the result with real generated identities. These fixtures
// do not stand in for the local archive/camera-inference verification tests.
package testtrack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

func Request(t testing.TB, scope serviceorigin.Scope, frame world.FrameRef, run string, sample, physical time.Time) serviceorigin.PublishRequest {
	t.Helper()
	ref := proofref.Reference{Key: world.Key{Tenant: scope.Tenant, Site: scope.Site, Entity: "sensor/camera-a", Type: sensorrecord.ComponentType, Source: world.Source{Device: scope.Device, App: scope.App, Service: "camera-a", Stream: "camera-a"}}, Version: world.Version{Epoch: 1, Sequence: 1}, EnvelopeSHA256: strings.Repeat("a", 64), RecordSHA256: strings.Repeat("b", 64)}
	member := trackcapture.Member{Proof: ref, SensorID: "camera-a", SampleID: run + "/camera-a/1", CaptureSequence: 1, SampleTime: sample, ValidUntil: sample.Add(time.Second), PhysicalUntil: physical.Add(4 * time.Second), PositionXZ: [2]float64{1, 2}, Covariance: [4]float64{.1, 0, 0, .1}, Image: sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), ContentType: "image/png", Size: 100}, ModelID: "test-model", ModelVersion: "1", ModelSHA256: strings.Repeat("d", 64), CalibrationRevision: "test-cal", CalibrationSHA256: strings.Repeat("e", 64), SceneBootID: "test-scene-boot", CapturedUnixNS: physical.UnixNano(), SimulationAnchorUnixNS: physical.UnixNano()}
	payload := trackcapture.Payload{Schema: trackcapture.ResultSchema, Algorithm: trackcapture.AlgorithmID, AlgorithmVersion: trackcapture.AlgorithmVersion, TrackID: "anonymous-1", Revision: 1, RunID: run, ClockDomain: "simulation:" + run, Frame: frame, ObservedAt: sample, ValidUntil: member.ValidUntil, PositionXZ: member.PositionXZ, Covariance: member.Covariance, Current: []trackcapture.Member{member}}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := resultrecord.Encode(resultrecord.Result{Version: resultrecord.Version, ResultSchema: trackcapture.ResultSchema, ResultJSON: body, Inputs: []proofref.Reference{ref}, RetainUntilUnixMS: physical.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceorigin.PublishRequest{Entity: run + "/track/anonymous-1", ComponentType: resultrecord.ComponentType, Stream: "tracks", Frame: frame, Timing: world.Timing{SampleTime: sample, ClockDomain: payload.ClockDomain, ErrorKnown: true}, TTL: time.Second, Payload: raw, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: trackcapture.AppVersion, ModelID: trackcapture.AlgorithmID, ModelVersion: trackcapture.AlgorithmVersion, Inputs: []world.Input{{ID: ref.ID(), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: run, ScenarioVersion: "test-only", Seed: 1, ClockDomain: payload.ClockDomain}}}}}
	if _, err = trackcapture.ValidatePublication(request); err != nil {
		t.Fatal(err)
	}
	return request
}
