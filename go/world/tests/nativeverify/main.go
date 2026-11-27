// nativeverify checks a retained native mission result against one owned local
// simulation fixture. It only reads status/evidence; it cannot submit or stop.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const maxReport = 4 << 20

type options struct{ ready, native, taskID, phase, out string }
type fixture struct {
	runID, packageID string
	port             int
	root             ed25519.PublicKey
	control          missioncontrol.SignedContext
	credentials      client.Config
}
type nativeSelection struct {
	schema, stage, runID, contextDigest string
	instruction                         json.RawMessage
	instructionDigest                   string
	byteInstruction                     bool
	stopInstruction                     *authority.SignedInstruction
}
type evidenceResult struct {
	SensorID    string `json:"sensor_id"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}
type report struct {
	Schema                 string           `json:"schema"`
	Verified               bool             `json:"verified"`
	VerifiedAt             string           `json:"verified_at"`
	NativeSchema           string           `json:"native_schema"`
	NativeStage            string           `json:"native_stage"`
	RunID                  string           `json:"run_id"`
	PackageSHA256          string           `json:"package_sha256"`
	ContextSHA256          string           `json:"context_sha256"`
	TaskID                 string           `json:"task_id"`
	Audience               string           `json:"audience"`
	Assignee               string           `json:"assignee"`
	Phase                  mission.Phase    `json:"phase"`
	ProofSHA256            string           `json:"proof_sha256"`
	NativeInstructionMatch bool             `json:"native_instruction_matched"`
	Record                 tasking.Record   `json:"record"`
	Evidence               []evidenceResult `json:"evidence"`
	Limits                 []string         `json:"limits"`
}
type reader interface {
	TaskStatus(context.Context, string) (tasking.Record, error)
	DownloadEvidence(context.Context, string, int64) ([]byte, evidence.GetResponse, error)
}

func main() {
	var o options
	flag.StringVar(&o.ready, "ready", "", "owned native mission fixture ready.json")
	flag.StringVar(&o.native, "native-report", "", "raw Apple or Android stage JSON")
	flag.StringVar(&o.taskID, "task-id", "", "exact task ID")
	flag.StringVar(&o.phase, "expected-phase", "", "succeeded or canceled")
	flag.StringVar(&o.out, "out", "", "new verification report path (never overwritten)")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "native verification failed:", err)
		os.Exit(1)
	}
	fmt.Println("Native mission proof and requested final result verified.")
}

func run(o options) error {
	if o.ready == "" || o.native == "" || o.out == "" || o.taskID == "" || len(o.taskID) > 256 || (o.phase != "succeeded" && o.phase != "canceled") {
		return errors.New("ready, native-report, task-id, expected-phase succeeded|canceled, and a new out path are required")
	}
	if _, err := os.Lstat(o.out); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output path already exists or is inaccessible")
	}
	f, err := loadFixture(o.ready)
	if err != nil {
		return err
	}
	raw, err := readBounded(o.native, maxReport)
	if err != nil {
		return fmt.Errorf("native report: %w", err)
	}
	native, err := selectNative(raw, o.taskID)
	if err != nil {
		return err
	}
	c, err := client.New(f.credentials)
	if err != nil {
		return errors.New("invalid local viewer credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := verify(ctx, c, f, native, o.taskID, mission.Phase(o.phase), time.Now())
	if err != nil {
		return err
	}
	return writeNew(o.out, result)
}

func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open required input")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) == 0 || len(b) > limit {
		return nil, errors.New("input empty, unreadable, or exceeds bound")
	}
	return b, nil
}

func object(raw []byte) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := authority.DecodeStrictLimit(raw, &out, maxReport); err != nil || out == nil {
		return nil, errors.New("invalid JSON object")
	}
	return out, nil
}
func stringField(m map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(m[key], &value)
	return value
}
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func loadFixture(path string) (fixture, error) {
	var f fixture
	raw, err := readBounded(path, 128<<10)
	if err != nil {
		return f, err
	}
	r, err := object(raw)
	if err != nil || stringField(r, "mission_schema") != "wendy.native-mission.site.v1" {
		return f, errors.New("explicit native mission fixture required")
	}
	f.runID, f.packageID = stringField(r, "run_id"), stringField(r, "package_sha256")
	var ports []int
	if json.Unmarshal(r["node_ports"], &ports) != nil || len(ports) != 3 || ports[0] < 1 || ports[0] > 65535 || f.runID == "" || !sensing.DigestValid(f.packageID) {
		return f, errors.New("invalid fixture scope or node ports")
	}
	f.port = ports[0]
	dir := filepath.Dir(path)
	var trust struct {
		RootPublicKey []byte                      `json:"root_public_key"`
		Revocations   authority.SignedRevocations `json:"revocations"`
	}
	for _, item := range []struct {
		name string
		out  any
	}{{"site-trust.json", &trust}, {"mission-context.json", &f.control}, {"viewer-1.json", &f.credentials}} {
		b, err := readBounded(filepath.Join(dir, item.name), 128<<10)
		if err != nil || authority.DecodeStrictLimit(b, item.out, 128<<10) != nil {
			return f, fmt.Errorf("invalid fixture %s", item.name)
		}
	}
	f.root = ed25519.PublicKey(trust.RootPublicKey)
	c, g := f.control.Context, f.credentials.Grant.Grant
	if authority.VerifyMissionControlContext(f.control, f.root) != nil || c.RunID != f.runID || c.PackageSHA256 != f.packageID || c.Audience != "node-1" || c.Type != missioncontrol.InspectType || g.Tenant != c.Tenant || g.Site != c.Site || g.Domain != "simulation" || f.credentials.Audience != c.Audience || !f.credentials.AllowInsecureSimulation {
		return f, errors.New("fixture root, viewer, and commissioned context disagree")
	}
	// Ignore container DNS and platform emulator aliases. No remote endpoint can
	// be selected through the imported credential URL.
	f.credentials.URL = "http://127.0.0.1:" + strconv.Itoa(f.port)
	return f, nil
}

func selectNative(raw []byte, taskID string) (nativeSelection, error) {
	var n nativeSelection
	m, err := object(raw)
	if err != nil {
		return n, err
	}
	n.schema, n.stage, n.runID = stringField(m, "schema"), stringField(m, "stage"), stringField(m, "run_id")
	n.contextDigest = stringField(m, "contextSHA256")
	if (present(m["error"]) && stringField(m, "error") != "") || stringField(m, "result") == "failed" {
		return n, errors.New("native stage reports a failure")
	}
	if n.runID == "" || n.stage == "" || stringField(m, "invocation_nonce") == "" {
		return n, errors.New("native report lacks run, stage, or invocation")
	}
	var selected map[string]json.RawMessage
	switch n.schema {
	case "wendy.android.mission-stage.v1":
		if stringField(m, "task_id") != taskID {
			return n, errors.New("Android report task mismatch")
		}
		selected, err = object(m["entry"])
		if err != nil {
			return n, errors.New("Android saved entry task mismatch")
		}
		if n.stage == "stop-external" {
			var noAuthored bool
			_ = json.Unmarshal(m["no_authored_mission_entry"], &noAuthored)
			if !noAuthored || stringField(selected, "task_id") != taskID || stringField(selected, "state") != "acknowledged" {
				return n, errors.New("Android external stop entry mismatch")
			}
			body, err := base64.StdEncoding.Strict().DecodeString(stringField(selected, "instruction"))
			var stop authority.SignedInstruction
			var payload struct {
				TaskID string `json:"task_id"`
			}
			if err != nil || authority.DecodeStrict(body, &stop) != nil || stop.Operation != "stop" || authority.DecodeStrict(stop.Payload, &payload) != nil || payload.TaskID != taskID || digest(body) != stringField(selected, "instruction_sha256") {
				return n, errors.New("invalid saved Android external stop instruction")
			}
			n.stopInstruction = &stop
			return n, nil
		}
		if stringField(selected, "id") != taskID {
			return n, errors.New("Android saved entry task mismatch")
		}
		n.byteInstruction = true
	case "wendy.apple.mission-model-test.v1":
		var entries, stops []json.RawMessage
		if json.Unmarshal(m["entries"], &entries) != nil || len(entries) > 128 || json.Unmarshal(m["stop_entries"], &stops) != nil || len(stops) > 128 {
			return n, errors.New("invalid Apple journal entries")
		}
		for _, raw := range entries {
			e, eerr := object(raw)
			if eerr != nil {
				return n, eerr
			}
			if stringField(e, "id") == taskID {
				if selected != nil {
					return n, errors.New("duplicate Apple task entry")
				}
				selected = e
			}
		}
		if selected == nil {
			// Another operator may stop a task absent from this journal. A stop
			// digest is never compared to the original mission instruction.
			matches := 0
			for _, raw := range stops {
				e, eerr := object(raw)
				if eerr != nil {
					return n, eerr
				}
				if stringField(e, "task_id") == taskID {
					matches++
				}
			}
			if matches != 1 {
				return n, errors.New("task absent or ambiguous in Apple journal")
			}
			return n, nil
		}
	default:
		return n, errors.New("use a raw Apple/Android mission stage report, not a runner summary")
	}
	if present(selected["instruction"]) {
		n.instruction = selected["instruction"]
		n.instructionDigest = stringField(selected, "instruction_sha256")
		if n.byteInstruction {
			var encoded string
			if json.Unmarshal(n.instruction, &encoded) != nil {
				return n, errors.New("Android instruction must be base64")
			}
			n.instruction, err = base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return n, errors.New("invalid saved instruction encoding")
			}
		}
		if len(n.instruction) > authority.MaxEnvelopeBytes || !sensing.DigestValid(n.instructionDigest) {
			return n, errors.New("invalid saved instruction bounds or digest")
		}
	} else if present(selected["instruction_sha256"]) {
		return n, errors.New("saved instruction digest without instruction")
	}
	return n, nil
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func verify(ctx context.Context, api reader, f fixture, native nativeSelection, taskID string, phase mission.Phase, now time.Time) (report, error) {
	var out report
	c := f.control.Context
	if (phase != mission.Succeeded && phase != mission.Canceled) || native.runID != f.runID || (native.contextDigest != "" && native.contextDigest != missioncontrol.Digest(f.control)) {
		return out, errors.New("native report or expected phase differs from fixture")
	}
	r, err := api.TaskStatus(ctx, taskID)
	if err != nil {
		return out, errors.New("cannot read assignee task status")
	}
	if r.Spec.ID != taskID || r.Spec.Assignee != c.Assignee || r.Status.Version == 0 || r.Status.Phase != phase || r.Spec.ControlContext == nil {
		return out, errors.New("task identity, final phase, or commissioned scope mismatch")
	}
	var instruction authority.SignedInstruction
	if authority.DecodeStrict(r.Proof, &instruction) != nil {
		return out, errors.New("invalid original instruction proof")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root, Tenant: c.Tenant, Site: c.Site, Domain: "simulation", Audience: c.Audience, Now: func() time.Time { return now }, Replay: authority.NewMemoryReplay(16)})
	if err != nil {
		return out, errors.New("invalid pinned verifier")
	}
	identity, err := v.VerifyInstructionHistoryForAudience(instruction, "mission", c.Audience)
	if err != nil || authority.VerifyMissionControlContext(*r.Spec.ControlContext, f.root) != nil {
		return out, errors.New("original mission or control context signature invalid")
	}
	if native.stopInstruction != nil {
		if phase != mission.Canceled {
			return out, errors.New("external stop stage requires a canceled result")
		}
		if _, err := v.VerifyInstructionHistoryForAudience(*native.stopInstruction, "stop", c.Audience); err != nil {
			return out, errors.New("saved external stop signature invalid")
		}
	}
	var signedSpec tasking.Spec
	if authority.DecodeStrict(instruction.Payload, &signedSpec) != nil || tasking.ValidateControlPayload(instruction.Payload, signedSpec) != nil {
		return out, errors.New("invalid canonical signed task spec")
	}
	specBytes, _ := json.Marshal(r.Spec)
	if !bytes.Equal(specBytes, instruction.Payload) || tasking.ValidateControlSpec(r.Spec, &f.control, f.runID) != nil || tasking.ControlProofTime(r.Spec, instruction.IssuedAtUnixMS, instruction.ExpiresAtUnixMS) != nil || r.Spec.NotAfterUnixMS > identity.NotAfter.UnixMilli() || r.AuthorizationUntilUnixMS > identity.NotAfter.UnixMilli() || r.AuthorizationUntilUnixMS < r.Spec.NotAfterUnixMS {
		return out, errors.New("record differs from signed instruction policy or lifetime")
	}
	proof, _ := json.Marshal(instruction)
	matched := false
	if len(native.instruction) > 0 {
		saved := native.instruction
		if !native.byteInstruction {
			// Apple exports a JSON object, which can reorder object keys. Restore
			// the protocol's typed canonical order, retaining every signed field.
			var i authority.SignedInstruction
			var s tasking.Spec
			if authority.DecodeStrict(saved, &i) != nil || authority.DecodeStrict(i.Payload, &s) != nil {
				return out, errors.New("invalid Apple saved instruction")
			}
			i.Payload, _ = json.Marshal(s)
			saved, _ = json.Marshal(i)
		}
		if !bytes.Equal(saved, proof) || digest(saved) != native.instructionDigest {
			return out, errors.New("native immutable instruction differs from assignee proof")
		}
		matched = true
	}
	verifiedEvidence := []evidenceResult{}
	if phase == mission.Succeeded {
		refs, err := inspectResult(r, f, instruction.IssuedAtUnixMS, now)
		if err != nil {
			return out, err
		}
		for _, ref := range refs {
			data, meta, err := api.DownloadEvidence(ctx, ref.SHA256, ref.Size)
			if err != nil {
				return out, fmt.Errorf("inspection evidence %s download: %w", ref.SHA256, err)
			}
			if int64(len(data)) != ref.Size || digest(data) != ref.SHA256 || meta.SHA256 != ref.SHA256 || meta.Size != ref.Size || meta.ContentType != ref.ContentType {
				return out, errors.New("inspection evidence download, digest, or metadata mismatch")
			}
			verifiedEvidence = append(verifiedEvidence, ref)
		}
	} else if r.Result != nil {
		return out, errors.New("canceled task unexpectedly has an inspection result")
	}
	out = report{Schema: "wendy.native-mission-verification.v1", Verified: true, VerifiedAt: now.UTC().Format(time.RFC3339Nano), NativeSchema: native.schema, NativeStage: native.stage, RunID: f.runID, PackageSHA256: f.packageID, ContextSHA256: missioncontrol.Digest(f.control), TaskID: taskID, Audience: c.Audience, Assignee: c.Assignee, Phase: phase, ProofSHA256: digest(proof), NativeInstructionMatch: matched, Record: r, Evidence: verifiedEvidence, Limits: []string{"Simulation fixture only; no hardware authority or safety-controller claim.", "Historical operator/context signatures do not prove historical revocation state or admission.", "Final status is read directly from the owned loopback assignee HTTP endpoint; this response has no independent assignee signature."}}
	return out, nil
}

func inspectResult(r tasking.Record, f fixture, issued int64, now time.Time) ([]evidenceResult, error) {
	i, s := r.Result, r.Spec
	if i == nil || i.TaskID != s.ID || i.RunID != f.runID || i.Frame != s.Frame || i.BootID == "" || i.Session == "" || i.Sequence == 0 || i.CapturedUnixMS < issued || i.CapturedUnixMS > now.UnixMilli() || i.CapturedUnixMS > s.NotAfterUnixMS || i.SettledSinceUnixMS <= 0 || i.SettledSinceUnixMS > i.CapturedUnixMS || i.SettledSinceSimulationNS < 0 || i.SettledSinceSimulationNS > i.SimulationTimeNS || !i.ObstacleClear || !finite(i.Energy) || i.Energy < s.MinimumEnergy || i.Energy > 1 || len(i.Samples) != 4 {
		return nil, errors.New("invalid captured inspection identity, time, or sensor set")
	}
	trigger, _ := json.Marshal(i.Trigger)
	signedTrigger, _ := json.Marshal(s.Trigger)
	if !bytes.Equal(trigger, signedTrigger) {
		return nil, errors.New("inspection changed signed trigger")
	}
	distance, speed := 0.0, 0.0
	for j := range i.Position {
		if !finite(i.Position[j]) || !finite(i.Velocity[j]) || i.Position[j] < s.Area.Min[j] || i.Position[j] > s.Area.Max[j] {
			return nil, errors.New("inspection outside commissioned area")
		}
		distance += math.Pow(i.Position[j]-s.Target[j], 2)
		speed += math.Pow(i.Velocity[j], 2)
	}
	if math.Sqrt(distance) > s.ToleranceM || math.Sqrt(speed) > s.StopSpeedMPS {
		return nil, errors.New("inspection did not settle at the signed target")
	}
	for _, name := range s.RequiredSensors {
		if !i.Sensors[name] {
			return nil, errors.New("inspection required sensor unavailable")
		}
	}
	expected := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}
	refs := []evidenceResult{}
	for index, sample := range i.Samples {
		if sample.Validate() != nil || expected[sample.SensorID] != sample.Kind || sample.RunID != f.runID || sample.PackageSHA256 != f.packageID || sample.SiteID != s.Site || sample.Frame != s.Frame || sample.Sequence != i.Sequence || sample.SampleNS != i.SimulationTimeNS {
			return nil, errors.New("inspection sample differs from pinned scene lineage")
		}
		if index > 0 && (sample.ScenarioVersion != i.Samples[0].ScenarioVersion || sample.Seed != i.Samples[0].Seed) {
			return nil, errors.New("inspection mixes scene versions or seeds")
		}
		if sample.Kind == "gas" || sample.Kind == "audio" {
			for j, position := range sample.PositionM {
				if math.Abs(position-i.Position[j]) > .002 {
					return nil, errors.New("local inspection sensor differs from captured robot pose")
				}
			}
		}
		if sample.Audio != nil && (sample.SampleNS-sample.Audio.DurationNS < i.SettledSinceSimulationNS || sample.Audio.DurationNS > int64(500*time.Millisecond) || time.Duration(i.CapturedUnixMS-i.SettledSinceUnixMS)*time.Millisecond < time.Duration(sample.Audio.DurationNS)) {
			return nil, errors.New("inspection audio predates settled capture")
		}
		delete(expected, sample.SensorID)
		if sample.Evidence != nil {
			e := sample.Evidence
			refs = append(refs, evidenceResult{SensorID: sample.SensorID, SHA256: e.SHA256, Size: e.Size, ContentType: e.ContentType})
		}
	}
	if len(expected) != 0 || len(refs) != 3 {
		return nil, errors.New("inspection must include both camera blobs and one audio blob")
	}
	return refs, nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func writeNew(path string, value report) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(b)+1 > maxReport {
		return errors.New("verification report exceeds bound")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create new verification report")
	}
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		return errors.New("cannot persist complete verification report")
	}
	return nil
}
