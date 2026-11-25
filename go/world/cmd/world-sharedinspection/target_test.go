package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

func sourceState(in commissionInput, now time.Time) executor.State {
	return executor.State{SchemaVersion: executor.PlanarStateSchema, RobotID: "atlas", RunID: in.RunID, Site: in.Site,
		Provenance: executor.StateProvenance{Mode: "simulated", PackageSHA256: in.PackageSHA256},
		Session:    "actual-session", BootID: "actual-boot", Sequence: 3, Position: [3]float64{-4.2, .24, 1.8},
		SimulationTimeS: 2, SampleUnixMS: now.UnixMilli(), Frame: in.Frame, Sensors: map[string]bool{"gas": true, "local-obstacles": true},
		ObstacleClear: true, Energy: 1, Synthetic: true}
}

func stateJSON(state executor.State) []byte {
	raw, _ := json.Marshal(state)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	body["commanded_velocity_mps"] = state.PlanarCommandedVelocity
	raw, _ = json.Marshal(body)
	return raw
}

func TestNavigationPreservesObservedHeightAndUsesCommissionedOffice(t *testing.T) {
	in, office, _ := inputFixture(t)
	now := time.Now()
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	planar := files["planar.json"].(executor.PlanarConfig)
	descriptor := files["inspection-destination.json"].(missioncontrol.InspectionDestination)
	policy := files["mission-context.json"].(missioncontrol.SignedContext).Context
	sha, err := executor.PlanarConfigSHA256(planar)
	if err != nil || sha != descriptor.PlanarConfigSHA256 || descriptor.MaxOffsetM != 2 {
		t.Fatal(descriptor, err)
	}
	for name, makeInput := range map[string]func(*testing.T, commissionInput, time.Time) missionInput{"person": personInput, "hazard": hazardInput} {
		t.Run(name, func(t *testing.T) {
			input := makeInput(t, in, now)
			before, _ := json.Marshal(input)
			plan, err := resolveNavigation(input, sourceState(in, now), planar, descriptor, policy, now)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Target[1] != .24 || plan.Descriptor != descriptor || plan.Review.StartPosition != sourceState(in, now).Position || plan.Review.StateSequence != 3 {
				t.Fatal(plan)
			}
			if name == "hazard" && (input.Target[1] != .65 || plan.Target == input.Target) {
				t.Fatal("lost original 3D observation")
			}
			if plan.LengthM <= 0 || plan.MinimumDurationMS != plan.NominalTravelMS+3000 || plan.MinimumDurationMS > policy.MaxDurationMS {
				t.Fatal(plan)
			}
			if _, err := collabdemo.PlanRoute(planar.Grid, collabdemo.Vec{-4.2, -1.8}, collabdemo.Vec{plan.Target[0], -plan.Target[2]}, planar.Reservations); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(input)
			if !bytes.Equal(before, after) {
				t.Fatal("source observation changed")
			}
		})
	}
}

func TestNavigationRefusesStaleWrongSourceOrImpossibleDuration(t *testing.T) {
	in, office, _ := inputFixture(t)
	now := time.Now()
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	planar := files["planar.json"].(executor.PlanarConfig)
	descriptor := files["inspection-destination.json"].(missioncontrol.InspectionDestination)
	policy := files["mission-context.json"].(missioncontrol.SignedContext).Context
	input := hazardInput(t, in, now)
	for _, mode := range []string{"stale", "frame", "package", "run", "duration", "blocked-start"} {
		t.Run(mode, func(t *testing.T) {
			state, p := sourceState(in, now), policy
			switch mode {
			case "stale":
				state.SampleUnixMS -= 251
			case "frame":
				state.Frame.Revision = "different"
			case "package":
				state.Provenance.PackageSHA256 = strings.Repeat("e", 64)
			case "run":
				state.RunID = "different"
			case "duration":
				p.MaxDurationMS = 1000
			case "blocked-start":
				state.Position = [3]float64{-8, .24, 6}
			}
			if _, err := resolveNavigation(input, state, planar, descriptor, p, now); !errors.Is(err, errInspectionUnavailable) {
				t.Fatal(err)
			}
		})
	}
	altered := descriptor
	altered.PlanarConfigSHA256 = strings.Repeat("0", 64)
	if _, err := resolveNavigation(input, sourceState(in, now), planar, altered, policy, now); err == nil || errors.Is(err, errInspectionUnavailable) {
		t.Fatal("bad commissioned binding treated as ordinary unavailability", err)
	}
}

func TestInspectionCommandSubprocess(t *testing.T) {
	encoded := os.Getenv("WORLD_INSPECTION_TEST_PROPOSE_ARGS")
	if encoded == "" {
		return
	}
	var args []string
	if json.Unmarshal([]byte(encoded), &args) != nil {
		t.Fatal("invalid subprocess arguments")
	}
	os.Args = append([]string{"world-sharedinspection"}, args...)
	main()
}

func TestProposeNavigationReadsOnlyStateAndRetainsSeparateHandoff(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "unavailable"}[unavailable], func(t *testing.T) {
			in, office, _ := inputFixture(t)
			if os.Getenv("WENDY_INSPECTION_HANDOFF_INPUT") != "" {
				// Production proof references require canonical native UUID scopes.
				in.Tenant = "11111111-1111-4111-8111-111111111111"
				in.Device = "spiffe://wendy.sh/tenant/" + in.Tenant + "/device/22222222-2222-4222-8222-222222222222"
			}
			now := time.Now()
			var gets, posts atomic.Int32
			token := strings.Repeat("r", 32)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				if r.Method != "GET" || r.URL.Path != "/v1/inspection/state" || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("planning used a control endpoint/token")
				}
				if unavailable {
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write(stateJSON(sourceState(in, time.Now())))
			}))
			defer source.Close()
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/proposals" || r.Header.Get("X-World-Authorization") == "" {
					t.Error("unexpected submission")
				}
				var request proposals.Request
				_ = json.NewDecoder(r.Body).Decode(&request)
				parameters, e := missioncontrol.DecodeInspectParameters(request.Parameters)
				if e != nil || parameters.Target[1] != .24 || parameters.InspectionDestination == nil {
					t.Error(parameters, e)
				}
				body, _ := json.Marshal(request)
				_ = json.NewEncoder(w).Encode(proposals.View{Record: proposals.Record{Proposal: request, Body: body, BodySHA256: hash(body)}})
			}))
			defer gateway.Close()
			in.SceneURL, in.GatewayURL = source.URL, gateway.URL
			files, err := commission(in, office, now)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for name, value := range files {
				if err := writeNew(filepath.Join(dir, name), value); err != nil {
					t.Fatal(err)
				}
			}
			input := hazardInput(t, in, now)
			// Cross-module qualification may supply a newly generated fixture from
			// production/resultrecord.Encode without changing the original bytes.
			if path := os.Getenv("WENDY_INSPECTION_HANDOFF_INPUT"); path != "" {
				if err := readJSON(path, &input, 768<<10, false); err != nil {
					t.Fatal(err)
				}
			}
			inputPath, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "handoff.json")
			if err := writeNew(inputPath, input); err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(dir, "scene-read-token")
			if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			if unavailable {
				args, _ := json.Marshal([]string{"propose", "--credentials", filepath.Join(dir, "proposer.json"), "--mission-input", inputPath, "--handoff", output, "--id", "selected-observation", "--navigation-directory", dir, "--source-read-token-file", tokenPath})
				child := exec.Command(os.Args[0], "-test.run=^TestInspectionCommandSubprocess$")
				child.Env = append(os.Environ(), "WORLD_INSPECTION_TEST_PROPOSE_ARGS="+string(args))
				message, err := child.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(string(message), "inspection_unavailable") || posts.Load() != 0 || gets.Load() != 1 {
					t.Fatal(err, string(message), gets.Load(), posts.Load())
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("handoff written after planning refusal")
				}
				return
			}
			err = propose(context.Background(), filepath.Join(dir, "proposer.json"), inputPath, output, "selected-observation", dir, tokenPath)
			if err != nil || gets.Load() != 1 || posts.Load() != 1 {
				t.Fatal(err, gets.Load(), posts.Load())
			}
			var hand handoff
			if err := readJSON(output, &hand, 1<<20, true); err != nil {
				t.Fatal(err)
			}
			original, _ := json.Marshal(input)
			returned, _ := json.Marshal(hand.MissionInput)
			if !bytes.Equal(original, returned) || hand.NavigationTarget == nil || (*hand.NavigationTarget)[1] != .24 || hand.InspectionDestination == nil || hand.Navigation == nil || hand.OriginalEnvelopeSHA256 != input.EnvelopeSHA256 {
				t.Fatal("handoff changed original input or omitted navigation context")
			}
			if output := os.Getenv("WENDY_INSPECTION_HANDOFF_OUTPUT"); output != "" {
				if err := writeNew(output, hand); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
