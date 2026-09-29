package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func robotAgentUpdateTestConnection(t *testing.T, kind string) (*vm.Store, *grpcclient.AgentConnection) {
	t.Helper()
	store := robotTestStore(t)
	profile := robotTestProfileForKind(t, store, "robot", kind)
	conn := robotTestRunningVM(t, "robot", profile)
	info := conn.AgentService.(*fakeAgentVersionClient).resp
	info.Version, info.CpuArchitecture, info.Featureset = "2026.09.16-025644", "arm64", nil
	previousUpdate := robotAgentUpdateFn
	t.Cleanup(func() { robotAgentUpdateFn = previousUpdate })
	robotAgentUpdateFn = func(context.Context, *grpcclient.AgentConnection, string, string, bool) error {
		t.Fatal("unexpected agent update")
		return nil
	}
	return store, conn
}

func TestRobotAgentUpdatePromptAcceptanceReconnectsAndContinues(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1, vm.RobotKindRosmasterR2} {
		t.Run(kind, func(t *testing.T) {
			store, conn := robotAgentUpdateTestConnection(t, kind)
			before := robotUpdatePromptProfileBytes(t, store)
			profile, _, _ := store.ReadRobotProfile("robot")
			next := robotTestRunningVM(t, "robot", profile)
			closed := &recordingCloser{}
			conn.ExtraClosers = []io.Closer{closed}
			prompts, updates, reconnects := 0, 0, 0
			robotUpdatePromptTestSetup(t, true, func(question string) bool {
				prompts++
				if !strings.Contains(question, `VM "robot"`) || !strings.Contains(question, "2026.09.16-025644") || !strings.Contains(question, "Update the agent now?") {
					t.Fatalf("prompt did not identify the agent update: %q", question)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				unlock, err := lockRobotProvision(ctx, store, "robot")
				if err != nil {
					t.Fatalf("provisioning lock held during prompt: %v", err)
				}
				unlock()
				return true
			})
			robotAgentUpdateFn = func(ctx context.Context, got *grpcclient.AgentConnection, osName, arch string, nightly bool) error {
				updates++
				if got != conn || osName != "wendyos" || arch != "arm64" || nightly || ctx.Err() != nil {
					t.Fatalf("wrong update target or platform: %p, %s/%s, nightly=%t", got, osName, arch, nightly)
				}
				if kind == vm.RobotKindRosmasterR2 {
					// A restart may drop the upload stream before its ack arrives.
					return errAgentUpdateUnconfirmed
				}
				return nil
			}
			conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
				reconnects++
				if !closed.closed {
					t.Fatal("old connection was not closed before reconnecting")
				}
				return next, nil
			}
			reachedDeployment := errors.New("updated agent reached sandbox mapping")
			robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) { return 0, reachedDeployment }
			got, err := reconcileSimulatorRobotWithAgentUpdate(context.Background(), conn)
			if got != next || !errors.Is(err, reachedDeployment) || prompts != 1 || updates != 1 || reconnects != 1 {
				t.Fatalf("did not continue on updated connection: got=%p, want=%p, prompts=%d, updates=%d, reconnects=%d, err=%v", got, next, prompts, updates, reconnects, err)
			}
			if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
				t.Fatal("agent update changed the robot profile")
			}
		})
	}
}

func TestRobotAgentUpdatePromptRequiresInteractiveApproval(t *testing.T) {
	for _, scenario := range []string{"declined", "no terminal", "JSON", "noninteractive", "inherited noninteractive", "maintenance", "physical host", "other OS", "probe failure", "canceled before prompt", "canceled during prompt"} {
		t.Run(scenario, func(t *testing.T) {
			store, conn := robotAgentUpdateTestConnection(t, vm.RobotKindRosmasterR2)
			before := robotUpdatePromptProfileBytes(t, store)
			info := conn.AgentService.(*fakeAgentVersionClient).resp
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prompts := 0
			robotUpdatePromptTestSetup(t, scenario != "no terminal", func(string) bool {
				prompts++
				if scenario == "canceled during prompt" {
					cancel()
					return true
				}
				if scenario != "declined" {
					t.Fatal("unexpected prompt")
				}
				return false
			})
			var wantErr error
			switch scenario {
			case "declined":
				wantErr = ErrUserCancelled
			case "JSON":
				jsonOutput = true
			case "noninteractive":
				ctx = robotRuntimePromptContext(ctx, true)
			case "inherited noninteractive":
				ctx = robotRuntimePromptContext(robotRuntimePromptContext(ctx, true), false)
			case "maintenance":
				ctx = robotAgentMaintenanceContext(ctx)
			case "physical host":
				deviceType := "jetson-orin-nano"
				info.DeviceType = &deviceType
			case "other OS":
				info.Os = "darwin"
			case "probe failure":
				wantErr = errors.New("agent unavailable")
				conn.AgentService.(*fakeAgentVersionClient).err = wantErr
			case "canceled before prompt":
				cancel()
				wantErr = context.Canceled
			case "canceled during prompt":
				wantErr = context.Canceled
			}
			got, err := reconcileSimulatorRobotWithAgentUpdate(ctx, conn)
			if got != conn {
				t.Fatal("connection changed without an update")
			}
			if wantErr != nil {
				if !errors.Is(err, wantErr) {
					t.Fatalf("got %v, want %v", err, wantErr)
				}
			} else if scenario == "maintenance" {
				if err != nil {
					t.Fatalf("agent maintenance blocked: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "rosmaster-r2-virtual-robot") || !strings.Contains(err.Error(), "wendy --device vm:robot device update") {
				t.Fatalf("missing actionable capability error: %v", err)
			}
			wantPrompts := 0
			if scenario == "declined" || scenario == "canceled during prompt" {
				wantPrompts = 1
			}
			if prompts != wantPrompts {
				t.Fatalf("got %d prompts, want %d", prompts, wantPrompts)
			}
			if string(robotUpdatePromptProfileBytes(t, store)) != string(before) {
				t.Fatal("robot profile changed without approval")
			}
		})
	}
}

func TestRobotAgentUpdatePromptStopsOnFailureWithoutRetryingUpdate(t *testing.T) {
	for _, scenario := range []string{"upload failure", "reconnect failure", "release still unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			_, conn := robotAgentUpdateTestConnection(t, vm.RobotKindRosmasterR2)
			prompts, updates, reconnects := 0, 0, 0
			robotUpdatePromptTestSetup(t, true, func(string) bool { prompts++; return true })
			failure := errors.New(scenario)
			closed := &recordingCloser{}
			conn.ExtraClosers = []io.Closer{closed}
			robotAgentUpdateFn = func(context.Context, *grpcclient.AgentConnection, string, string, bool) error {
				updates++
				if scenario == "upload failure" {
					return failure
				}
				return nil
			}
			next := &grpcclient.AgentConnection{SimulatorName: conn.SimulatorName, Addr: conn.Addr, AgentService: conn.AgentService}
			conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
				reconnects++
				if scenario == "reconnect failure" {
					return nil, failure
				}
				return next, nil
			}
			robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) {
				t.Fatal("unsupported agent reached deployment")
				return 0, nil
			}
			got, err := reconcileSimulatorRobotWithAgentUpdate(context.Background(), conn)
			if scenario == "release still unsupported" {
				if got != next || err == nil || !strings.Contains(err.Error(), "device update --binary") {
					t.Fatalf("missing manual update guidance after unsupported release: %v", err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("lost update failure: %v", err)
			}
			wantReconnects := 1
			if scenario == "upload failure" {
				wantReconnects = 0
				if got != conn {
					t.Fatal("upload failure replaced the connection")
				}
			} else if scenario == "reconnect failure" && got != nil {
				t.Fatal("reconnect failure returned a stale connection")
			}
			if prompts != 1 || updates != 1 || reconnects != wantReconnects || closed.closed != (wantReconnects == 1) {
				t.Fatalf("unexpected retry or cleanup: prompts=%d, updates=%d, reconnects=%d, closed=%t", prompts, updates, reconnects, closed.closed)
			}
		})
	}
}
