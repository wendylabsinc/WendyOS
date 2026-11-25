//go:build integration

// Build within production using the parent Wendy module; this lab driver uses existing task/evidence APIs.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"golang.org/x/sys/unix"
)

func check(e error) {
	if e != nil {
		panic(e)
	}
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func read(p string, v any) { b, e := os.ReadFile(p); check(e); check(json.Unmarshal(b, v)) }
func raw(v any) []byte     { b, e := json.Marshal(v); check(e); return b }
func save(p string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	check(e)
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	check(e)
	_, e = f.Write(append(b, '\n'))
	check(e)
	check(f.Close())
}

type Input struct {
	Version            int        `json:"version"`
	TriggerJSON        []byte     `json:"trigger_json"`
	TriggerSHA256      string     `json:"trigger_sha256"`
	Target             [3]float64 `json:"target"`
	EnvelopeSHA256     string     `json:"envelope_sha256"`
	ResultSchema       string     `json:"result_schema"`
	SampleValidUntil   time.Time  `json:"sample_valid_until"`
	PhysicalValidUntil time.Time  `json:"physical_valid_until"`
}
type Handoff struct {
	Version                int                                   `json:"version"`
	ProposalID             string                                `json:"proposal_id"`
	MissionInput           Input                                 `json:"mission_input"`
	OriginalEnvelopeSHA256 string                                `json:"original_envelope_sha256"`
	NavigationTarget       [3]float64                            `json:"navigation_target"`
	Destination            *missioncontrol.InspectionDestination `json:"inspection_destination"`
	MinimumDurationMS      int64                                 `json:"minimum_duration_ms"`
}

func observe(dir string) {
	var cfg struct {
		Tenant, Site, Device, RunID string
		Frame                       world.FrameRef
	}
	read(filepath.Join(dir, "observation-config.json"), &cfg)
	sensors := []string{"camera-a", "camera-b", "gas-1", "audio-1", "lidar-atlas"}
	records := []world.Component{}
	validated := []resultrecord.ValidatedInput{}
	captured := map[world.Key]fusion.CapturedSample{}
	sources := map[string]world.Source{}
	media := map[string][]byte{}
	var now time.Time
	for _, id := range sensors {
		path := filepath.Join(dir, "captures", id+".json")
		b, e := os.ReadFile(path)
		check(e)
		var item struct {
			Capture sensorrecord.Capture `json:"capture"`
			Media   []byte               `json:"media"`
		}
		check(json.Unmarshal(b, &item))
		s, e := item.Capture.NormalizedSample()
		check(e)
		if s.RunID != cfg.RunID || s.Frame != cfg.Frame {
			panic("source scope changed")
		}
		p, e := sensorrecord.Publication(item.Capture, 3*time.Second)
		check(e)
		source := world.Source{Device: cfg.Device, App: "com.wendy.world.localinspection", Service: id, Stream: id}
		sources[id] = source
		c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: p.Entity, Type: p.ComponentType, Source: source}, Version: world.Version{Epoch: 1, Sequence: s.Sequence}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Provenance: p.Provenance, Payload: p.Payload}
		ref := proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: hash(b), RecordSHA256: hash(raw(c))}
		check(ref.Validate())
		sampleUntil, physicalUntil, e := sensorrecord.LiveDeadlines(c)
		check(e)
		if !time.Now().Before(physicalUntil) {
			panic("local source capture expired before inference")
		}
		validated = append(validated, resultrecord.ValidatedInput{Reference: ref, Component: c, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil})
		records = append(records, c)
		captured[c.Key] = fusion.CapturedSample{Sample: s, ProofSHA256: ref.EnvelopeSHA256, SceneBootID: item.Capture.SceneBootID, SimulationAnchor: time.Unix(0, item.Capture.SimulationAnchorUnixNS)}
		if now.IsZero() {
			now = c.Timing.SampleTime
		} else if now != c.Timing.SampleTime {
			panic("fixed source batch differs")
		}
		if s.Evidence != nil {
			if hash(item.Media) != s.Evidence.SHA256 || int64(len(item.Media)) != s.Evidence.Size {
				panic("source media mismatch")
			}
			media[s.Evidence.SHA256] = item.Media
		}
	}
	result, e := fusion.Evaluate(context.Background(), fusion.Config{Tenant: cfg.Tenant, Site: cfg.Site, RunID: cfg.RunID, ClockDomain: "simulation:" + cfg.RunID, Frame: cfg.Frame, Now: func() time.Time { return now }, Sources: sources, OptionalSensors: map[string]string{"lidar-atlas": "lidar"}, CaptureType: sensorrecord.ComponentType, DecodeCapture: func(c world.Component) (fusion.CapturedSample, error) {
		v, ok := captured[c.Key]
		if !ok {
			return v, errors.New("unknown local capture")
		}
		return v, nil
	}}, records, func(_ context.Context, r sensing.EvidenceRef) ([]byte, error) {
		b := media[r.SHA256]
		if int64(len(b)) != r.Size || hash(b) != r.SHA256 {
			return nil, errors.New("exact media unavailable")
		}
		return bytes.Clone(b), nil
	})
	check(e)
	save(filepath.Join(dir, "fusion-evaluation.json"), result)
	if result.State != fusion.HazardDetected {
		panic("actual local hazard rule did not detect a hazard: " + result.Reason)
	}
	p, e := fusioncapture.BuildPublication(result, validated, "hazard", time.Now().Add(time.Minute))
	check(e)
	_, e = fusioncapture.ValidateReplicaPublication(p)
	check(e)
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: cfg.Device, App: "com.wendy.world.localinspection", Service: "fusion", Stream: "hazard"}}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Provenance: p.Provenance, Payload: p.Payload}
	// This is an explicitly unsigned local fixture wrapper, never an enrolled Agent proof.
	envelope := raw(map[string]any{"version": 1, "origin": "local-simulation-fixture-not-enrolled-agent", "component": c})
	f, e := os.OpenFile(filepath.Join(dir, "local-inference.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	check(e)
	_, e = f.Write(envelope)
	check(e)
	check(f.Close())
	trigger, e := tasking.TriggerFromComponent(c)
	check(e)
	generic, e := resultrecord.Decode(c.Payload)
	check(e)
	var typed struct {
		PhysicalValidUntil time.Time `json:"physical_valid_until"`
	}
	check(json.Unmarshal(generic.ResultJSON, &typed))
	save(filepath.Join(dir, "mission-input.json"), Input{1, raw(trigger), trigger.SHA256, result.Hazard.PositionM, hash(envelope), fusion.HazardType, result.Hazard.ValidUntil, typed.PhysicalValidUntil})
	save(filepath.Join(dir, "hazard.json"), json.RawMessage(generic.ResultJSON))
	save(filepath.Join(dir, "observation-receipt.json"), map[string]any{"actual_fusion_evaluated": true, "source_origin": "owned local MuJoCo originals", "native_agent_origin_verified": false, "proof_wrapper": "unsigned local fixture identities; not native certificate proofs", "original_envelope_sha256": hash(envelope), "original_sample_time": now, "sources": sensors})
}
func api(dir, role string) (*client.Client, client.Config) {
	var c client.Config
	read(filepath.Join(dir, "commissioned", role+".json"), &c)
	a, e := client.New(c)
	check(e)
	return a, c
}
func monotonic() time.Duration {
	var t unix.Timespec
	check(unix.ClockGettime(unix.CLOCK_MONOTONIC, &t))
	return time.Duration(t.Sec)*time.Second + time.Duration(t.Nsec)
}
func authorize(dir string) {
	a, c := api(dir, "operator")
	var h Handoff
	read(filepath.Join(dir, "commissioned/handoff.json"), &h)
	var contextBound missioncontrol.SignedContext
	read(filepath.Join(dir, "commissioned/mission-context.json"), &contextBound)
	var pins missionclock.Pins
	read(filepath.Join(dir, "commissioned/mission-clock-trust.json"), &pins)
	check(authority.VerifyMissionControlContext(contextBound, pins.RootPublicKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view, e := a.Proposal(ctx, h.ProposalID)
	check(e)
	if view.Expired || view.AdapterValidated {
		panic("proposal not reviewable")
	}
	params, e := missioncontrol.DecodeInspectParameters(view.Record.Proposal.Parameters)
	check(e)
	if params.Target != h.NavigationTarget || params.TriggerSHA256 != h.MissionInput.TriggerSHA256 || !missioncontrol.EqualInspectionDestination(params.InspectionDestination, h.Destination) {
		panic("reviewed proposal differs")
	}
	clock, e := a.ObserveMissionClock(ctx, pins, "mission", monotonic)
	check(e)
	interval, e := clock.Interval()
	check(e)
	policy := contextBound.Context
	duration := int64(30000)
	if h.MinimumDurationMS > duration {
		duration = h.MinimumDurationMS
	}
	if duration > policy.MaxDurationMS {
		panic("route duration exceeds existing context")
	}
	var trigger tasking.TriggerRef
	check(json.Unmarshal(h.MissionInput.TriggerJSON, &trigger))
	s := tasking.Spec{Trigger: &trigger, InspectionDestination: h.Destination, ControlContext: &contextBound, Proposal: &missioncontrol.ProposalRef{Audience: c.Audience, ID: h.ProposalID, BodySHA256: view.Record.BodySHA256}, ID: "local-inspection", Type: policy.Type, Site: policy.Site, Assignee: policy.Assignee, Domain: mission.Simulation, Class: policy.Class, Mode: policy.Mode, Frame: policy.Frame, NotAfterUnixMS: interval.LowerUnixMS + duration, Area: tasking.Bounds{Min: policy.Area.Min, Max: policy.Area.Max}, RequiredSensors: policy.RequiredSensors, MinimumEnergy: policy.MinimumEnergy, Fallback: policy.Fallback, Target: h.NavigationTarget, SpeedMPS: policy.SpeedMPS, ToleranceM: policy.ToleranceM, StopSpeedMPS: policy.StopSpeedMPS}
	check(tasking.ValidateControlSpec(s, &contextBound, policy.RunID))
	instruction, e := authority.SignInstruction(c.Grant, ed25519.PrivateKey(c.PrivateKey), c.Audience, "mission", raw(s), time.UnixMilli(interval.LowerUnixMS), time.Duration(duration)*time.Millisecond)
	check(e)
	evidence, e := clock.Evidence()
	check(e)
	save(filepath.Join(dir, "operator-review.json"), map[string]any{"authorization": "explicit local simulation qualification requested by operator", "proposal": view, "spec": s, "clock": evidence, "no_motion_retry": true})
	save(filepath.Join(dir, "operator-instruction.json"), instruction)
	var accepted tasking.Record
	check(a.DoWithMissionClock(ctx, clock, "mission", "POST", "/v1/tasks", raw(instruction), &accepted))
	if tasking.SpecDigest(accepted.Spec) != tasking.SpecDigest(s) {
		panic("accepted task differs")
	}
	save(filepath.Join(dir, "accepted-task.json"), accepted)
}
func selectTask(records []tasking.Record, h Handoff, expected string) (tasking.Record, error) {
	if expected == "" {
		return tasking.Record{}, errors.New("explicit expected task ID required")
	}
	var trigger tasking.TriggerRef
	if err := json.Unmarshal(h.MissionInput.TriggerJSON, &trigger); err != nil {
		return tasking.Record{}, err
	}
	for _, r := range records {
		if r.Spec.ID != expected {
			continue
		}
		if r.Spec.Proposal == nil || r.Spec.Proposal.ID != h.ProposalID || r.Spec.Trigger == nil ||
			trigger.SHA256 != h.MissionInput.TriggerSHA256 || !bytes.Equal(raw(r.Spec.Trigger), raw(trigger)) ||
			r.Spec.Target != h.NavigationTarget || !missioncontrol.EqualInspectionDestination(r.Spec.InspectionDestination, h.Destination) {
			return tasking.Record{}, errors.New("expected task differs from selected handoff")
		}
		if r.Result == nil || r.Status.Phase != mission.Succeeded {
			return tasking.Record{}, errors.New("expected task has no successful inspection result")
		}
		return r, nil
	}
	return tasking.Record{}, errors.New("expected task is absent")
}

func status(dir string, fetch bool, expected string) {
	a, c := api(dir, "evidence-reader")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var listing struct {
		Tasks []tasking.Record `json:"tasks"`
	}
	check(a.Do(ctx, "read", "GET", "/v1/tasks", nil, &listing))
	if !fetch {
		check(json.NewEncoder(os.Stdout).Encode(listing))
		return
	}
	var handoff Handoff
	read(filepath.Join(dir, "commissioned/handoff.json"), &handoff)
	r, e := selectTask(listing.Tasks, handoff, expected)
	check(e)
	var gateway struct {
		RootPublicKey []byte                      `json:"root_public_key"`
		Revocations   authority.SignedRevocations `json:"revocations"`
	}
	read(filepath.Join(dir, "commissioned/gateway.json"), &gateway)
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: gateway.RootPublicKey, Tenant: c.Grant.Grant.Tenant, Site: c.Grant.Grant.Site, Domain: "simulation", Audience: c.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(64)})
	check(e)
	check(v.UpdateRevocations(gateway.Revocations))
	var instruction authority.SignedInstruction
	check(json.Unmarshal(r.Proof, &instruction))
	_, e = v.VerifyInstructionHistoryForAudience(instruction, "mission", c.Audience)
	check(e)
	var signedSpec tasking.Spec
	check(json.Unmarshal(instruction.Payload, &signedSpec))
	if tasking.SpecDigest(signedSpec) != tasking.SpecDigest(r.Spec) {
		panic("task differs from operator proof")
	}
	var relay struct {
		Entries []struct {
			Status *authority.SignedTaskStatus `json:"status"`
		} `json:"entries"`
	}
	check(a.Do(ctx, "read", "GET", "/v1/task-relay", nil, &relay))
	var signed *authority.SignedTaskStatus
	for _, entry := range relay.Entries {
		if entry.Status != nil && entry.Status.TaskID == r.Spec.ID && entry.Status.StatusVersion == r.Status.Version {
			signed = entry.Status
		}
	}
	if signed == nil {
		panic("terminal gateway signature not yet available")
	}
	_, e = v.VerifyTaskStatus(*signed)
	check(e)
	var signedRecord tasking.Record
	check(json.Unmarshal(signed.Record, &signedRecord))
	if !bytes.Equal(raw(signedRecord), raw(r)) {
		panic("signed task result differs")
	}
	result := r.Result
	if result.TaskID != r.Spec.ID || result.Frame != r.Spec.Frame || result.Trigger == nil || r.Spec.Trigger == nil || !bytes.Equal(raw(result.Trigger), raw(r.Spec.Trigger)) || len(result.Samples) != 4 {
		panic("inspection result binding differs")
	}
	check(os.Mkdir(filepath.Join(dir, "result-media"), 0700))
	mediaRows := []any{}
	kinds := map[string]int{}
	for _, s := range result.Samples {
		check(s.Validate())
		if s.RunID != result.RunID || s.SampleNS != result.SimulationTimeNS || s.Frame != result.Frame || s.Sequence != result.Sequence {
			panic("incoherent inspection originals")
		}
		kinds[s.Kind]++
		if s.Evidence != nil {
			b, m, e := a.DownloadEvidence(ctx, s.Evidence.SHA256, s.Evidence.Size)
			check(e)
			if m.ContentType != s.Evidence.ContentType || m.Size != s.Evidence.Size {
				panic("evidence metadata differs")
			}
			suffix := map[string]string{"image/png": ".png", "audio/wav": ".wav"}[m.ContentType]
			if suffix == "" {
				panic("unexpected media")
			}
			path := filepath.Join(dir, "result-media", s.SensorID+suffix)
			check(os.WriteFile(path, b, 0600))
			mediaRows = append(mediaRows, map[string]any{"sensor_id": s.SensorID, "path": path, "reference": s.Evidence, "retention_unix_ms": m.RetainUntilUnixMS})
		}
	}
	if kinds["camera"] != 2 || kinds["gas"] != 1 || kinds["audio"] != 1 {
		panic("inspection source set differs")
	}
	save(filepath.Join(dir, "result.json"), r)
	save(filepath.Join(dir, "signed-result.json"), signed)
	save(filepath.Join(dir, "result-verification.json"), map[string]any{"operator_signature_verified": true, "gateway_result_signature_verified": true, "exact_task_trigger_bound": true, "four_coherent_originals": true, "complete_media_digests_verified": true, "media": mediaRows, "result_sha256": hash(raw(result)), "scope": "actual signed local simulation gateway/executor; selected inference uses explicitly local fixture origin"})
}
func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, r)
			os.Exit(1)
		}
	}()
	if len(os.Args) < 3 || len(os.Args) > 4 || (os.Args[1] == "fetch") != (len(os.Args) == 4) {
		panic("observe|authorize|status DIRECTORY; fetch DIRECTORY EXPECTED_TASK_ID")
	}
	switch os.Args[1] {
	case "observe":
		observe(os.Args[2])
	case "authorize":
		authorize(os.Args[2])
	case "status":
		status(os.Args[2], false, "")
	case "fetch":
		status(os.Args[2], true, os.Args[3])
	default:
		panic("unknown operation")
	}
}
