package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// A structural native person result, not a new inference or signed origin.
// The ordinary reader supplies the independently verified original in use.
func personInput(t *testing.T, in commissionInput, now time.Time) missionInput {
	t.Helper()
	sample := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	until := sample.Add(time.Second)
	source := world.Source{Device: in.Device, App: "com.wendy.world.sharedinspection", Service: "people-front", Stream: "people-front"}
	key := world.Key{Tenant: in.Tenant, Site: in.Site, Entity: "sensor/people-front", Type: "wendy.sensor.capture.v1", Source: source}
	ref := map[string]any{"key": key, "version": world.Version{Epoch: 1, Sequence: 1}, "envelope_sha256": strings.Repeat("c", 64), "record_sha256": strings.Repeat("d", 64)}
	member := map[string]any{"proof": ref, "sensor_id": "people-front", "sample_id": in.RunID + "/people-front/1", "capture_sequence": 1, "member_index": 0, "sample_time": sample, "valid_until": until, "physical_until": now.Add(time.Second), "position_xz": [2]float64{-3, 2}, "covariance": [4]float64{.1, 0, 0, .1}, "image": map[string]any{}, "model_id": "pose", "model_version": "1", "model_sha256": strings.Repeat("e", 64), "calibration_revision": "shared", "calibration_sha256": strings.Repeat("f", 64), "scene_boot_id": "boot", "captured_unix_ns": now.UnixNano(), "simulation_anchor_unix_ns": int64(1767225600000000000)}
	body, _ := json.Marshal(map[string]any{"schema": "wendy.person.track.v1", "algorithm": "camera-ankle-two-observation-tracking", "algorithm_version": "1", "track_id": "track-1", "revision": 1, "run_id": in.RunID, "clock_domain": "simulation:" + in.RunID, "frame": in.Frame, "observed_at": sample, "valid_until": until, "position_xz": [2]float64{-3, 2}, "covariance": [4]float64{.1, 0, 0, .1}, "ambiguous": false, "motion_known": false, "current": []any{member}})
	payload, _ := json.Marshal(map[string]any{"version": 1, "result_schema": "wendy.person.track.v1", "result_json": body, "inputs": []any{ref}, "retain_until_unix_ms": now.Add(time.Minute).UnixMilli()})
	c := world.Component{Key: world.Key{Tenant: in.Tenant, Site: in.Site, Entity: in.RunID + "/track/track-1", Type: missioncontrol.NativeInferenceType, Source: world.Source{Device: in.Device, App: source.App, Service: "people", Stream: "tracks"}}, Version: world.Version{Epoch: 2, Sequence: 1}, Frame: in.Frame, Timing: world.Timing{SampleTime: sample, ClockDomain: "simulation:" + in.RunID, ErrorKnown: true}, TTL: time.Second, Payload: payload, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-live-people-v1", ModelID: "camera-ankle-two-observation-tracking", ModelVersion: "1", Inputs: []world.Input{{ID: "sha256:" + strings.Repeat("c", 64), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: in.RunID, ScenarioVersion: "shared-office-perception-v1", Seed: 7, ClockDomain: "simulation:" + in.RunID}}}}}
	trigger, e := tasking.TriggerFromComponent(c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(trigger)
	return missionInput{1, raw, trigger.SHA256, [3]float64{-3, .24, 2}, strings.Repeat("a", 64), "wendy.person.track.v1", until, now.Add(time.Second)}
}

// Like personInput, this is structural proposal context, not a new inference
// or an origin-signature fixture. The elevated Y deliberately proves that
// preparing a hazard does not silently flatten its original location.
func hazardInput(t *testing.T, in commissionInput, now time.Time) missionInput {
	t.Helper()
	sample := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	until := sample.Add(time.Second)
	refs, inputs := []any{}, []any{}
	ancestry := []world.Input{}
	for i, sensor := range []string{"audio-1", "camera-a", "camera-b", "gas-1"} {
		source := world.Source{Device: in.Device, App: "com.wendy.world.sharedinspection", Service: sensor, Stream: sensor}
		version := world.Version{Epoch: 1, Sequence: uint64(i + 1)}
		proof := strings.Repeat(string("bcde"[i]), 64)
		key := world.Key{Tenant: in.Tenant, Site: in.Site, Entity: "sensor/" + sensor, Type: "wendy.sensor.capture.v1", Source: source}
		refs = append(refs, map[string]any{"key": key, "version": version, "envelope_sha256": proof, "record_sha256": strings.Repeat("f", 64)})
		inputs = append(inputs, map[string]any{"sample_id": in.RunID + "/" + sensor + "/1", "sensor_id": sensor,
			"kind": strings.Split(sensor, "-")[0], "source": source, "version": version,
			"sample_time": sample, "capture_start": sample, "valid_until": until,
			"capture": map[string]any{"proof_sha256": proof, "scene_boot_id": "boot", "simulation_anchor": sample.Add(-time.Second)}})
		ancestry = append(ancestry, world.Input{ID: "sha256:" + proof, Origin: world.Simulated,
			Simulation: &world.SimulationContext{RunID: in.RunID, ScenarioVersion: "shared-office-perception-v1", Seed: 7, ClockDomain: "simulation:" + in.RunID}})
	}
	target := [3]float64{-3, .65, 2}
	body, _ := json.Marshal(map[string]any{"label": "possible-gas-hazard", "confidence": .8,
		"confidence_meaning": "rule support", "position_m": target, "uncertainty_m": .5,
		"location_basis": "structural test", "gas_value_ppm": 90, "gas_uncertainty_ppm": 5,
		"gas_species": "simulated", "cameras": []any{}, "audio": map[string]any{},
		"inputs": inputs, "evidence": []any{}, "valid_until": until,
		"input_digest": strings.Repeat("1", 64), "physical_valid_until": now.Add(time.Second)})
	payload, _ := json.Marshal(map[string]any{"version": 1, "result_schema": "wendy.hazard.v1", "result_json": body,
		"inputs": refs, "retain_until_unix_ms": now.Add(time.Minute).UnixMilli()})
	c := world.Component{Key: world.Key{Tenant: in.Tenant, Site: in.Site, Entity: in.RunID + "/possible-gas-hazard",
		Type: missioncontrol.NativeInferenceType, Source: world.Source{Device: in.Device, App: "com.wendy.world.sharedinspection", Service: "fusion", Stream: "hazard"}},
		Version: world.Version{Epoch: 2, Sequence: 1}, Frame: in.Frame,
		Timing: world.Timing{SampleTime: sample, ClockDomain: "simulation:" + in.RunID, ErrorKnown: true}, TTL: time.Second, Payload: payload,
		Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-fusion-v2", ModelID: "synthetic-gas-warning-rule", ModelVersion: "2", Inputs: ancestry}}
	trigger, e := tasking.TriggerFromComponent(c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(trigger)
	return missionInput{1, raw, trigger.SHA256, target, strings.Repeat("a", 64), "wendy.hazard.v1", until, now.Add(time.Second)}
}

func TestSelectedReaderInputProposesOnlyFixedContextOverExistingSignedHTTP(t *testing.T) {
	for name, makeInput := range map[string]func(*testing.T, commissionInput, time.Time) missionInput{"person": personInput, "hazard": hazardInput} {
		t.Run(name, func(t *testing.T) { selectedReaderInputProposes(t, makeInput) })
	}
}

func selectedReaderInputProposes(t *testing.T, makeInput func(*testing.T, commissionInput, time.Time) missionInput) {
	t.Helper()
	in, office, _ := inputFixture(t)
	now := time.Now()
	files, e := commission(in, office, now)
	if e != nil {
		t.Fatal(e)
	}
	cfg := files["proposer.json"].(client.Config)
	input := makeInput(t, in, now)
	gateway := files["gateway.json"].(map[string]any)
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: gateway["root_public_key"].(ed25519.PublicKey), Tenant: in.Tenant, Site: in.Site, Domain: "simulation", Audience: audience, Now: time.Now, Replay: authority.NewMemoryReplay(100)})
	if e != nil {
		t.Fatal(e)
	}
	if e = v.UpdateRevocations(gateway["revocations"].(authority.SignedRevocations)); e != nil {
		t.Fatal(e)
	}
	request, e := proposalRequest(cfg, input, "inspect-selected", now)
	if e != nil {
		t.Fatal(e)
	}
	params, e := missioncontrol.DecodeInspectParameters(request.Parameters)
	if e != nil || params.Target != input.Target || params.TriggerSHA256 != input.TriggerSHA256 {
		t.Fatal(params, e)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/proposals" {
			t.Error("motion endpoint called")
		}
		var got proposals.Request
		_ = json.NewDecoder(r.Body).Decode(&got)
		body, _ := json.Marshal(got)
		raw, e := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		if e != nil || json.Unmarshal(raw, &signed) != nil {
			t.Error("missing existing signed request")
		}
		if signed.Grant.Grant.Kind != "agent" || signed.Grant.Grant.Principal != cfg.Grant.Grant.Principal {
			t.Error("wrong role")
		}
		if _, e = v.Verify(signed, "propose", body); e != nil {
			t.Error("signed proposer request refused", e)
		}
		_ = json.NewEncoder(w).Encode(proposals.View{Record: proposals.Record{Proposal: got, Body: body, BodySHA256: hash(body)}})
	}))
	defer server.Close()
	cfg.URL = server.URL
	api, e := client.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, e := submitProposal(context.Background(), api, request, input)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 1 || got.ProposalID != request.ID || got.OriginalEnvelopeSHA256 != input.EnvelopeSHA256 || !bytes.Equal(got.MissionInput.TriggerJSON, input.TriggerJSON) {
		t.Fatal("handoff changed originals")
	}
	input.Target[0]++
	if _, e = proposalRequest(cfg, input, "other", now); e == nil {
		t.Fatal("reconstructed target accepted")
	}
}

func TestHazardProposalRejectsSchemaTargetAndDigestSubstitution(t *testing.T) {
	in, office, now := inputFixture(t)
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := files["proposer.json"].(client.Config)
	original := hazardInput(t, in, now)
	for _, mode := range []string{"person-schema", "unknown-schema", "floor-projection", "digest"} {
		t.Run(mode, func(t *testing.T) {
			input := original
			switch mode {
			case "person-schema":
				input.ResultSchema = "wendy.person.track.v1"
			case "unknown-schema":
				input.ResultSchema = "wendy.other.v1"
			case "floor-projection":
				input.Target[1] = .24
			case "digest":
				input.TriggerSHA256 = strings.Repeat("0", 64)
			}
			if _, err := proposalRequest(cfg, input, "changed-context", now); err == nil {
				t.Fatal("changed hazard context accepted")
			}
		})
	}
}

type uncertainProposal struct{ calls int }

func (p *uncertainProposal) ProposeTask(context.Context, proposals.Request) (proposals.View, error) {
	p.calls++
	return proposals.View{}, context.DeadlineExceeded
}
func TestUncertainProposalHasNoRetryOrHandoff(t *testing.T) {
	p := &uncertainProposal{}
	got, e := submitProposal(context.Background(), p, proposals.Request{}, missionInput{})
	if e == nil || p.calls != 1 || got.Version != 0 {
		t.Fatal("uncertain proposal changed into success")
	}
}
