package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type fakeReader struct {
	record    tasking.Record
	blobs     map[string][]byte
	metadata  map[string]evidence.GetResponse
	downloads int
}

func (f *fakeReader) TaskStatus(_ context.Context, id string) (tasking.Record, error) {
	if id != f.record.Spec.ID {
		return tasking.Record{}, fmt.Errorf("unexpected ID")
	}
	return f.record, nil
}
func (f *fakeReader) DownloadEvidence(_ context.Context, hash string, bound int64) ([]byte, evidence.GetResponse, error) {
	f.downloads++
	data := f.blobs[hash]
	if int64(len(data)) > bound {
		return nil, evidence.GetResponse{}, fmt.Errorf("oversized")
	}
	return data, f.metadata[hash], nil
}

func fixtureRecord(t *testing.T, phase mission.Phase) (fixture, nativeSelection, *fakeReader, time.Time) {
	t.Helper()
	b, err := os.ReadFile("../fixtures/native-mission-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]json.RawMessage
	if err = json.Unmarshal(b, &source); err != nil {
		t.Fatal(err)
	}
	var f fixture
	var i authority.SignedInstruction
	var canonical string
	if json.Unmarshal(source["root_public_key"], &f.root) != nil || json.Unmarshal(source["control_context"], &f.control) != nil || json.Unmarshal(source["instruction_canonical"], &canonical) != nil || authority.DecodeStrict([]byte(canonical), &i) != nil {
		t.Fatal("invalid canonical fixture")
	}
	f.runID, f.packageID = f.control.Context.RunID, f.control.Context.PackageSHA256
	var spec tasking.Spec
	if authority.DecodeStrict(i.Payload, &spec) != nil {
		t.Fatal("invalid fixture spec")
	}
	r := tasking.Record{Spec: spec, Status: mission.Status{Version: 3, Phase: phase}, Proof: []byte(canonical), AuthorizationUntilUnixMS: i.ExpiresAtUnixMS}
	n := nativeSelection{schema: "wendy.android.mission-stage.v1", stage: "completed", runID: f.runID, instruction: r.Proof, instructionDigest: digest(r.Proof), byteInstruction: true}
	reader := &fakeReader{record: r, blobs: map[string][]byte{}, metadata: map[string]evidence.GetResponse{}}
	// Deliberately verify after both original role and context have expired.
	now := time.UnixMilli(f.control.Context.NotAfterUnixMS).Add(time.Hour)
	if phase == mission.Succeeded {
		inspection := &tasking.Inspection{TaskID: spec.ID, RunID: f.runID, BootID: "boot", Session: "session", Sequence: 7, CapturedUnixMS: i.IssuedAtUnixMS + 2000, SettledSinceUnixMS: i.IssuedAtUnixMS + 1000, SettledSinceSimulationNS: int64(time.Second), SimulationTimeNS: int64(2 * time.Second), Frame: spec.Frame, Position: spec.Target, Sensors: map[string]bool{"obstacles": true}, ObstacleClear: true, Energy: .9}
		for _, sensor := range []struct{ id, kind, mime string }{{"camera-a", "camera", "image/png"}, {"camera-b", "camera", "image/png"}, {"gas-1", "gas", ""}, {"audio-1", "audio", "audio/wav"}} {
			sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("%s/%s/7", f.runID, sensor.id), SiteID: spec.Site, Frame: spec.Frame, SensorID: sensor.id, Kind: sensor.kind, Sequence: 7, SampleNS: inspection.SimulationTimeNS, RunID: f.runID, ScenarioVersion: "scenario-v1", Seed: 1, PackageSHA256: f.packageID, SensorModel: "synthetic", PositionM: append([]float64(nil), spec.Target[:]...), CalibrationRevision: "calibration-v1"}
			switch sensor.kind {
			case "camera":
				x, y, z := spec.Target[0], spec.Target[1], spec.Target[2]
				sample.Camera = &sensing.Camera{Width: 1, Height: 1, FX: 1, FY: 1, CameraToSite: []float64{1, 0, 0, x, 0, 1, 0, y, 0, 0, 1, z, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Hour)}
			case "gas":
				sample.Gas = &sensing.Gas{ValuePPM: 5, UncertaintyPPM: .1, Species: "test"}
			case "audio":
				sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(100 * time.Millisecond)}
			}
			if sensor.mime != "" {
				data := []byte("bounded test evidence " + sensor.id)
				hash := digest(data)
				sample.Evidence = &sensing.EvidenceRef{SHA256: hash, Size: int64(len(data)), ContentType: sensor.mime}
				reader.blobs[hash] = data
				reader.metadata[hash] = evidence.GetResponse{SHA256: hash, Size: int64(len(data)), ContentType: sensor.mime}
			}
			inspection.Samples = append(inspection.Samples, sample)
		}
		reader.record.Result = inspection
	}
	return f, n, reader, now
}

func TestHistoricalProofAndEveryEvidenceBlob(t *testing.T) {
	f, n, api, now := fixtureRecord(t, mission.Succeeded)
	out, err := verify(context.Background(), api, f, n, api.record.Spec.ID, mission.Succeeded, now)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Verified || !out.NativeInstructionMatch || len(out.Evidence) != 3 || api.downloads != 3 || out.ProofSHA256 != n.instructionDigest {
		t.Fatalf("incomplete result: %#v", out)
	}
	data, _ := json.Marshal(out)
	if bytes.Contains(data, []byte("private_key")) || bytes.Contains(data, []byte("credentials")) {
		t.Fatal("report contains credential material")
	}
}

func TestRejectsProofScopeStatusAndEvidenceChanges(t *testing.T) {
	cases := map[string]func(*fixture, *nativeSelection, *fakeReader){
		"root":          func(f *fixture, _ *nativeSelection, _ *fakeReader) { f.root = make(ed25519.PublicKey, 32) },
		"context":       func(f *fixture, _ *nativeSelection, _ *fakeReader) { f.control.Context.Assignee = "different/assignee" },
		"native run":    func(_ *fixture, n *nativeSelection, _ *fakeReader) { n.runID = "other-run" },
		"native digest": func(_ *fixture, n *nativeSelection, _ *fakeReader) { n.instructionDigest = digest([]byte("different")) },
		"native instruction": func(_ *fixture, n *nativeSelection, _ *fakeReader) {
			n.instruction = append([]byte(" "), n.instruction...)
		},
		"wrong phase":     func(_ *fixture, _ *nativeSelection, r *fakeReader) { r.record.Status.Phase = mission.Running },
		"zero version":    func(_ *fixture, _ *nativeSelection, r *fakeReader) { r.record.Status.Version = 0 },
		"unsigned target": func(_ *fixture, _ *nativeSelection, r *fakeReader) { r.record.Spec.Target[0] += .1 },
		"wrong package": func(_ *fixture, _ *nativeSelection, r *fakeReader) {
			r.record.Result.Samples[0].PackageSHA256 = digest([]byte("other"))
		},
		"duplicate sample": func(_ *fixture, _ *nativeSelection, r *fakeReader) {
			r.record.Result.Samples[1] = r.record.Result.Samples[0]
		},
		"missing evidence":  func(_ *fixture, _ *nativeSelection, r *fakeReader) { r.record.Result.Samples[0].Evidence = nil },
		"unsettled capture": func(_ *fixture, _ *nativeSelection, r *fakeReader) { r.record.Result.Position[0] += .5 },
		"corrupt bytes": func(_ *fixture, _ *nativeSelection, r *fakeReader) {
			for hash := range r.blobs {
				r.blobs[hash][0] ^= 1
				break
			}
		},
		"wrong metadata": func(_ *fixture, _ *nativeSelection, r *fakeReader) {
			for hash, m := range r.metadata {
				m.ContentType = "application/octet-stream"
				r.metadata[hash] = m
				break
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, n, api, now := fixtureRecord(t, mission.Succeeded)
			change(&f, &n, api)
			if _, err := verify(context.Background(), api, f, n, api.record.Spec.ID, mission.Succeeded, now); err == nil {
				t.Fatal("accepted changed verification input")
			}
		})
	}
}

func TestNativeStageFormatsAndInstructionEquality(t *testing.T) {
	for _, platform := range []string{"apple", "android"} {
		t.Run(platform, func(t *testing.T) {
			f, expected, api, now := fixtureRecord(t, mission.Canceled)
			entry := map[string]any{"id": api.record.Spec.ID, "instruction_sha256": expected.instructionDigest}
			stage := map[string]any{"stage": "stop", "run_id": f.runID, "invocation_nonce": "invocation", "task_id": api.record.Spec.ID}
			if platform == "android" {
				stage["schema"] = "wendy.android.mission-stage.v1"
				entry["instruction"] = base64.StdEncoding.EncodeToString(expected.instruction)
				stage["entry"] = entry
			} else {
				stage["schema"] = "wendy.apple.mission-model-test.v1"
				var reordered map[string]any
				// UseNumber avoids changing the original integer timestamps.
				d := json.NewDecoder(bytes.NewReader(expected.instruction))
				d.UseNumber()
				if err := d.Decode(&reordered); err != nil {
					t.Fatal(err)
				}
				entry["instruction"] = reordered
				stage["entries"] = []any{map[string]any{"id": "unrelated"}, entry}
				stage["stop_entries"] = []any{}
			}
			raw, _ := json.Marshal(stage)
			native, err := selectNative(raw, api.record.Spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = verify(context.Background(), api, f, native, api.record.Spec.ID, mission.Canceled, now); err != nil {
				t.Fatal(err)
			}
			if api.downloads != 0 {
				t.Fatal("canceled verification downloaded evidence")
			}
		})
	}
}

func TestOtherOperatorStopHasNoMissionInstructionComparison(t *testing.T) {
	f, _, api, now := fixtureRecord(t, mission.Canceled)
	stage := map[string]any{"schema": "wendy.apple.mission-model-test.v1", "stage": "stop", "run_id": f.runID, "invocation_nonce": "invocation", "entries": []any{}, "stop_entries": []any{map[string]any{"task_id": api.record.Spec.ID, "instruction_sha256": digest([]byte("stop proof"))}}}
	raw, _ := json.Marshal(stage)
	n, err := selectNative(raw, api.record.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := verify(context.Background(), api, f, n, api.record.Spec.ID, mission.Canceled, now)
	if err != nil || out.NativeInstructionMatch {
		t.Fatalf("stop-only history: %v", err)
	}
	if _, err = selectNative(raw, "absent"); err == nil {
		t.Fatal("accepted absent task")
	}
}

func TestLoadFixturePinsLoopbackAndWriteNeverOverwrites(t *testing.T) {
	f, _, _, _ := fixtureRecord(t, mission.Canceled)
	dir := t.TempDir()
	write := func(name string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.credentials.URL = "http://untrusted.invalid:9999"
	f.credentials.Audience = "node-1"
	f.credentials.AllowInsecureSimulation = true
	f.credentials.Grant.Grant.Tenant = f.control.Context.Tenant
	f.credentials.Grant.Grant.Site = f.control.Context.Site
	f.credentials.Grant.Grant.Domain = "simulation"
	write("ready.json", map[string]any{"mission_schema": "wendy.native-mission.site.v1", "run_id": f.runID, "package_sha256": f.packageID, "node_ports": []int{12345, 12346, 12347}})
	write("site-trust.json", map[string]any{"root_public_key": f.root})
	write("mission-context.json", f.control)
	write("viewer-1.json", f.credentials)
	loaded, err := loadFixture(filepath.Join(dir, "ready.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.credentials.URL != "http://127.0.0.1:12345" {
		t.Fatal("did not pin loopback")
	}
	path := filepath.Join(dir, "verification.json")
	if err = writeNew(path, report{Verified: true}); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	if err = writeNew(path, report{Verified: false}); err == nil {
		t.Fatal("overwrote report")
	}
	retained, _ := os.ReadFile(path)
	stat, _ := os.Stat(path)
	if !bytes.Equal(original, retained) || stat.Mode().Perm() != 0600 {
		t.Fatal("report modified or permissions too broad")
	}
}

func TestAndroidExternalStopSeparatesStopAndMissionProof(t *testing.T) {
	f, _, api, now := fixtureRecord(t, mission.Canceled)
	b, err := os.ReadFile("../fixtures/native-mission-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtureJSON map[string]json.RawMessage
	if err = json.Unmarshal(b, &fixtureJSON); err != nil {
		t.Fatal(err)
	}
	var stop string
	if err = json.Unmarshal(fixtureJSON["stop_instruction_canonical"], &stop); err != nil {
		t.Fatal(err)
	}
	stage := map[string]any{"schema": "wendy.android.mission-stage.v1", "stage": "stop-external", "run_id": f.runID, "invocation_nonce": "invocation", "task_id": api.record.Spec.ID, "no_authored_mission_entry": true,
		"entry": map[string]any{"task_id": api.record.Spec.ID, "state": "acknowledged", "instruction": base64.StdEncoding.EncodeToString([]byte(stop)), "instruction_sha256": digest([]byte(stop))}}
	raw, _ := json.Marshal(stage)
	n, err := selectNative(raw, api.record.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.instruction) != 0 || n.stopInstruction == nil {
		t.Fatal("external stop confused with authored mission")
	}
	out, err := verify(context.Background(), api, f, n, api.record.Spec.ID, mission.Canceled, now)
	if err != nil || out.NativeInstructionMatch {
		t.Fatalf("external stop failed: %v", err)
	}
	n.stopInstruction.Signature[0] ^= 1
	if _, err = verify(context.Background(), api, f, n, api.record.Spec.ID, mission.Canceled, now); err == nil {
		t.Fatal("accepted changed external stop signature")
	}
	stage["result"] = "failed"
	raw, _ = json.Marshal(stage)
	if _, err = selectNative(raw, api.record.Spec.ID); err == nil {
		t.Fatal("accepted failed native stage")
	}
}

func TestRejectsDuplicateNativeJSONKeys(t *testing.T) {
	if _, err := selectNative([]byte(`{"schema":"a","schema":"b"}`), "task"); err == nil {
		t.Fatal("accepted duplicate fields")
	}
}
