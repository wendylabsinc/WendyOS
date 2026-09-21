package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func robotUpdatePromptTestSetup(t *testing.T, interactive bool, confirm func(string) bool) {
	t.Helper()
	stubTerminal(t, interactive)
	previousConfirm, previousJSON := confirmFn, jsonOutput
	confirmFn, jsonOutput = confirm, false
	t.Cleanup(func() { confirmFn, jsonOutput = previousConfirm, previousJSON })
}

func robotUpdatePromptOldProfile(t *testing.T, store *vm.Store, kind string) vm.RobotProfile {
	t.Helper()
	robotTestProfileForKind(t, store, "robot", kind)
	if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error {
		p.SourceDigest, p.PolicyBundle = "sha256:"+strings.Repeat("a", 64), "old-policy"
		p.RuntimeDigest = "sha256:" + strings.Repeat("b", 64)
		p.Seed, p.VisualDetail, p.CPUs, p.MemoryMiB, p.SandboxHostPort = 42, "full", 6, 8192, 18890
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	profile, exists, err := store.ReadRobotProfile("robot")
	if err != nil || !exists {
		t.Fatalf("reading old profile: exists=%t, err=%v", exists, err)
	}
	return profile
}

func robotUpdatePromptProfileBytes(t *testing.T, store *vm.Store) []byte {
	t.Helper()
	data, err := os.ReadFile(store.RobotProfilePath("robot"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRobotRuntimeUpdatePromptAcceptanceUpdatesOnlyRuntimePins(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1} {
		t.Run(kind, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotUpdatePromptOldProfile(t, store, kind)
			conn := robotTestRunningVM(t, "robot", profile)
			prompts := 0
			robotUpdatePromptTestSetup(t, true, func(question string) bool {
				prompts++
				if !strings.Contains(question, "robot") || !strings.Contains(strings.ToLower(question), "update") {
					t.Fatalf("prompt did not identify the update and target: %q", question)
				}
				return true
			})
			reachedDeployment := errors.New("accepted update reached verified sandbox mapping")
			robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) { return 0, reachedDeployment }
			if err := reconcileSimulatorRobot(context.Background(), conn); !errors.Is(err, reachedDeployment) {
				t.Fatalf("accepted update did not continue provisioning: %v", err)
			}
			if prompts != 1 {
				t.Fatalf("expected one update prompt, got %d", prompts)
			}
			runtime, err := robotRuntimeForKind(kind)
			if err != nil {
				t.Fatal(err)
			}
			want := profile
			want.SourceDigest, want.PolicyBundle, want.RuntimeDigest = runtime.sourceDigest(), runtime.policyBundle, ""
			got, exists, err := store.ReadRobotProfile("robot")
			if err != nil || !exists || !reflect.DeepEqual(got, want) {
				t.Fatalf("update changed configuration beyond runtime pins: got %+v, want %+v, err=%v", got, want, err)
			}
		})
	}
}

func TestRobotRuntimeUpdatePromptDeclinePreservesProfile(t *testing.T) {
	store := robotTestStore(t)
	robotUpdatePromptOldProfile(t, store, vm.RobotKindGo2)
	before := robotUpdatePromptProfileBytes(t, store)
	prompts := 0
	robotUpdatePromptTestSetup(t, true, func(string) bool { prompts++; return false })
	// Deliberately omit agent and container services: refusal must never reach
	// runtime operations or attempt a deployment.
	err := reconcileSimulatorRobot(context.Background(), &grpcclient.AgentConnection{SimulatorName: "robot"})
	if !errors.Is(err, ErrUserCancelled) || prompts != 1 {
		t.Fatalf("decline did not cancel after one prompt: prompts=%d, err=%v", prompts, err)
	}
	if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
		t.Fatal("declining updated the persisted runtime")
	}
}

func TestRobotRuntimeUpdatePromptSuppressedWithoutInteractiveApproval(t *testing.T) {
	for _, scenario := range []string{"no terminal", "JSON", "noninteractive context", "noninteractive inherited context"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			robotUpdatePromptOldProfile(t, store, vm.RobotKindGo2)
			before := robotUpdatePromptProfileBytes(t, store)
			robotUpdatePromptTestSetup(t, scenario != "no terminal", func(string) bool {
				t.Fatal("opened a confirmation without an interactive caller")
				return true
			})
			ctx := context.Background()
			switch scenario {
			case "JSON":
				jsonOutput = true
			case "noninteractive context":
				ctx = robotRuntimePromptContext(ctx, true)
			case "noninteractive inherited context":
				ctx = robotRuntimePromptContext(robotRuntimePromptContext(ctx, true), false)
			}
			err := reconcileSimulatorRobot(ctx, &grpcclient.AgentConnection{SimulatorName: "robot"})
			var mismatch *robotSourceMismatchError
			if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "wendy vm robot update robot") {
				t.Fatalf("missing actionable runtime mismatch: %v", err)
			}
			if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
				t.Fatal("noninteractive reconciliation updated the persisted runtime")
			}
		})
	}
}

func TestRobotRuntimeUpdatePromptCancellationPreservesProfile(t *testing.T) {
	for _, cancelAt := range []string{"before reconciliation", "during confirmation"} {
		t.Run(cancelAt, func(t *testing.T) {
			store := robotTestStore(t)
			robotUpdatePromptOldProfile(t, store, vm.RobotKindGo2)
			before := robotUpdatePromptProfileBytes(t, store)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			robotUpdatePromptTestSetup(t, true, func(string) bool {
				if cancelAt == "before reconciliation" {
					t.Fatal("prompted after cancellation")
				}
				cancel()
				return true
			})
			if cancelAt == "before reconciliation" {
				cancel()
			}
			if err := reconcileSimulatorRobot(ctx, &grpcclient.AgentConnection{SimulatorName: "robot"}); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled reconciliation did not preserve cancellation: %v", err)
			}
			if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
				t.Fatal("canceled confirmation updated the persisted runtime")
			}
		})
	}
}

func TestRobotRuntimeUpdatePromptDoesNotOfferUpdatesForRuntimeStatus(t *testing.T) {
	for _, scenario := range []string{"matching runtime", "unhealthy runtime", "foreign runtime"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfile(t, store, "robot")
			conn := robotTestRunningVM(t, "robot", profile)
			robotUpdatePromptTestSetup(t, true, func(string) bool {
				t.Fatal("offered to update a runtime with matching source pins")
				return true
			})
			state := robotTestStatus("robot", profile)
			switch scenario {
			case "unhealthy runtime":
				state.Healthy, state.Error = false, "worker failed"
			case "foreign runtime":
				state.VMName = "other"
			}
			body, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			err = reconcileSimulatorRobot(context.Background(), conn)
			if (err == nil) != (scenario == "matching runtime") {
				t.Fatalf("unexpected reconciliation outcome for %s: %v", scenario, err)
			}
			got, _, err := store.ReadRobotProfile("robot")
			if err != nil || !reflect.DeepEqual(got, profile) {
				t.Fatalf("runtime status changed pinned configuration: %+v, %v", got, err)
			}
		})
	}
}

func TestRobotRuntimeUpdatePromptDoesNotOfferToRepairInvalidProfiles(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfile(t, store, "robot")
	if err := os.WriteFile(store.RobotProfilePath("robot"), []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := robotUpdatePromptProfileBytes(t, store)
	robotUpdatePromptTestSetup(t, true, func(string) bool {
		t.Fatal("offered a runtime update for an invalid profile")
		return true
	})
	err := reconcileSimulatorRobot(context.Background(), &grpcclient.AgentConnection{SimulatorName: "robot"})
	var mismatch *robotSourceMismatchError
	if err == nil || errors.As(err, &mismatch) {
		t.Fatalf("invalid profile became a source mismatch: %v", err)
	}
	if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
		t.Fatal("invalid profile was rewritten")
	}
}

func TestRobotRuntimeUpdatePromptSkipsConnectionsWithoutRobot(t *testing.T) {
	robotTestStore(t)
	robotUpdatePromptTestSetup(t, true, func(string) bool {
		t.Fatal("offered a robot update for a connection without a robot profile")
		return true
	})
	for _, conn := range []*grpcclient.AgentConnection{nil, {}, {SimulatorName: "generic"}} {
		if err := reconcileSimulatorRobot(context.Background(), conn); err != nil {
			t.Fatalf("connection without a robot was rejected: %v", err)
		}
	}
}

func TestRobotRuntimeUpdatePromptRespectsResolverNonInteractiveOptions(t *testing.T) {
	for _, scenario := range []string{"resolve target", "connect agent", "run --yes"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			robotUpdatePromptOldProfile(t, store, vm.RobotKindGo2)
			before := robotUpdatePromptProfileBytes(t, store)
			robotUpdatePromptTestSetup(t, true, func(string) bool {
				t.Fatal("noninteractive device resolution opened a runtime update prompt")
				return true
			})
			t.Setenv("WENDY_AGENT_SOCKET", "")
			previousChoice, previousDevice := connectSimulatorChoiceFn, deviceFlag
			t.Cleanup(func() { connectSimulatorChoiceFn, deviceFlag = previousChoice, previousDevice })
			deviceFlag = "vm:robot"
			connections := 0
			connectSimulatorChoiceFn = func(ctx context.Context, choice *simulatorChoice, _ bool) (*SelectedDevice, error) {
				connections++
				if choice.Name != "robot" || choice.Create {
					t.Fatalf("noninteractive resolution changed the selected VM: %+v", choice)
				}
				return nil, reconcileSimulatorRobot(ctx, &grpcclient.AgentConnection{SimulatorName: choice.Name})
			}
			var err error
			switch scenario {
			case "resolve target":
				_, err = resolveTargetInner(context.Background(), NonInteractive())
			case "connect agent":
				_, err = connectToAgent(context.Background(), NonInteractive())
			case "run --yes":
				_, err = resolveTargetInner(context.Background(), runResolveOptions(runOptions{yes: true})...)
			}
			var mismatch *robotSourceMismatchError
			if connections != 1 || !errors.As(err, &mismatch) {
				t.Fatalf("noninteractive resolution lost the source mismatch: connections=%d, err=%v", connections, err)
			}
			if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
				t.Fatal("noninteractive device resolution updated the pinned runtime")
			}
		})
	}
}
