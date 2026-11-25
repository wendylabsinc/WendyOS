package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Public test keys only. This fixture pins Go's wire bytes for native clients.
func TestNativeMissionCanonicalFixture(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	opKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{42}, 32))
	serverKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{43}, 32))
	g := Grant{Version: 1, ID: "native-operator", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "operator-native", Kind: "operator", Source: world.Source{Device: "native", App: "mission-console", Service: "operator", Stream: "tasks"}, PublicKey: opKey.Public().(ed25519.PublicKey), Scopes: []string{"read", "mission", "stop"}, NotBeforeUnixMS: now.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}
	op, err := SignGrant(g, root)
	if err != nil {
		t.Fatal(err)
	}
	g.ID = "mission-clock"
	g.Principal = "gateway-clock"
	g.Kind = "device"
	g.PublicKey = serverKey.Public().(ed25519.PublicKey)
	g.Scopes = []string{"mission-clock-server"}
	g.PublishAudience = "node-1"
	server, err := SignGrant(g, root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(Config{RootPublicKey: root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "node-1", Now: func() time.Time { return now }, Replay: NewMemoryReplay(100)})
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := SignRevocations(Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, root)
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	request, err := SignMissionClockRequest(op, opKey, missionclock.Request{Tenant: "tenant", Site: "office", Audience: "node-1", RunID: "run-native", ClockDomain: "simulation:run-native", Purpose: "mission", Nonce: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := v.IssueMissionClock(request, server, serverKey, "run-native", "simulation:run-native", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	pins := missionclock.Pins{RootPublicKey: root.Public().(ed25519.PublicKey), GatewayPrincipal: g.Principal, GatewayKeySHA256: KeyID(g.PublicKey), RunID: "run-native", ClockDomain: "simulation:run-native"}
	control, err := SignMissionControlContext(missioncontrol.Context{Version: 1, Tenant: "tenant", Site: "office", Audience: "node-1", RunID: "run-native", PackageSHA256: strings.Repeat("ef", 32), Assignee: "robot/navigation", Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Type: tasking.InspectType, Class: mission.Move, Mode: mission.Autonomous, Area: missioncontrol.Bounds{Min: [3]float64{-2, 0, -1}, Max: [3]float64{2, 1, 1}}, RequiredSensors: []string{"obstacles"}, MinimumEnergy: .1, Fallback: mission.Hold, SpeedMPS: .25, ToleranceM: .1, StopSpeedMPS: .05, MaxDurationMS: 60000, NotBeforeUnixMS: now.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	c := control.Context
	spec := tasking.Spec{ControlContext: &control, Proposal: &missioncontrol.ProposalRef{Audience: "node-1", ID: "proposal-native", BodySHA256: strings.Repeat("12", 32)}, ID: "mission-native", Type: c.Type, Site: c.Site, Assignee: c.Assignee, Domain: mission.Simulation, Class: c.Class, Mode: c.Mode, Frame: c.Frame, NotAfterUnixMS: now.Add(30 * time.Second).UnixMilli(), Area: tasking.Bounds{Min: c.Area.Min, Max: c.Area.Max}, RequiredSensors: c.RequiredSensors, MinimumEnergy: c.MinimumEnergy, Fallback: c.Fallback, Target: [3]float64{1.25, .2, 0}, SpeedMPS: c.SpeedMPS, ToleranceM: c.ToleranceM, StopSpeedMPS: c.StopSpeedMPS}
	payload, _ := json.Marshal(spec)
	instruction, err := SignInstruction(op, opKey, "node-1", "mission", payload, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(instruction)
	transport, err := SignRequest(op, opKey, "node-1", "mission", body, "native-transport-nonce-0001", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := SignInstruction(op, opKey, "node-1", "stop", json.RawMessage(`{"task_id":"mission-native"}`), now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	canonical := func(value any) string {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	signing := func(kind string, value any) string {
		b, e := signingBytes(kind, value)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	out := map[string]any{"version": 1, "purpose": "public deterministic cross-language test keys; simulation only", "now_unix_ms": now.UnixMilli(), "root_public_key": pins.RootPublicKey, "operator_private_key": []byte(opKey), "operator_grant": op, "clock_server_grant": server, "mission_clock_trust": pins, "clock_request": request, "clock_request_canonical": canonical(request), "clock_request_signing_bytes": signing("mission-clock-request", missionClockPayload(request.Version, request.Grant, request.Request)), "clock_observation": observation, "clock_observation_canonical": canonical(observation), "clock_observation_signing_bytes": signing("mission-clock-observation", missionClockPayload(observation.Version, observation.Grant, observation.Observation)), "control_context": control, "control_context_canonical": canonical(control), "control_context_sha256": missioncontrol.Digest(control), "control_context_signing_bytes": signing("mission-control-context", control.Context), "spec": spec, "spec_canonical": string(payload), "instruction": instruction, "instruction_canonical": string(body), "instruction_signing_bytes": signing("instruction", instructionPayload(instruction)), "transport": transport, "transport_canonical": canonical(transport), "transport_signing_bytes": signing("request", requestPayload(transport)), "stop_instruction": stop, "stop_instruction_canonical": canonical(stop), "stop_instruction_signing_bytes": signing("instruction", instructionPayload(stop))}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := "../../tests/fixtures/native-mission-v1.json"
	if os.Getenv("UPDATE_NATIVE_MISSION_FIXTURE") == "1" {
		if err = os.WriteFile(path, b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Fatal("native mission canonical fixture changed")
	}
	if _, err = v.VerifyInstruction(instruction, "mission"); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyMissionClockObservation(request, observation, pins); err != nil {
		t.Fatal(err)
	}
}
