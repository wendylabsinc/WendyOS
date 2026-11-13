package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestReadOnlyMonitoringPreservesSelectedVMIdentity(t *testing.T) {
	cfg := &config.Config{DefaultDevice: "vm:robot"}
	cfg.SetDevicePin("127.0.0.1", 1, "cloud", "other-device", "")
	cfg.SetDevicePin("vm:robot", 1, "", "robot-device", "")
	setTempConfig(t, cfg)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	stubVMStatuses(t, runningVM("robot", vm.NetUser, 50055))
	oldFlag, oldDial, oldChoice, oldUpdate := deviceFlag, dialAgentLadderFn, connectSimulatorChoiceFn, checkAndOfferUpdateFn
	t.Cleanup(func() {
		deviceFlag, dialAgentLadderFn, connectSimulatorChoiceFn, checkAndOfferUpdateFn = oldFlag, oldDial, oldChoice, oldUpdate
	})
	connectSimulatorChoiceFn = func(context.Context, *simulatorChoice, bool) (*SelectedDevice, error) {
		t.Fatal("read-only monitoring entered the VM lifecycle/runtime connection")
		return nil, nil
	}
	checkAndOfferUpdateFn = func(context.Context, *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		t.Fatal("read-only monitoring checked automatic updates")
		return nil, nil
	}
	dials := 0
	dialAgentLadderFn = func(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dials++
		if target.PinKey != "vm:robot" || target.Addr != "127.0.0.1:50055" || target.Expected == nil || target.Expected.EntityID != "robot-device" {
			t.Fatalf("lost selected VM identity: %+v", target)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("VM probe has no deadline")
		}
		return &grpcclient.AgentConnection{IsMTLS: true, CertInfo: &config.CertificateInfo{OrganizationID: 1}, AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	for _, selected := range []string{"vm:robot", ""} {
		deviceFlag = selected
		conn, err := connectToAgent(context.Background(), ReadOnlyMonitoring())
		if err != nil {
			t.Fatal(err)
		}
		if conn.SimulatorName != "robot" || deviceFlag != selected {
			t.Fatalf("selection changed: %+v / %q", conn, deviceFlag)
		}
		conn.Close()
	}
	if dials != 2 {
		t.Fatalf("got %d dials", dials)
	}
}

func TestReadOnlyMonitoringRefusesUnavailableVMWithoutLifecycle(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	setTempConfig(t, &config.Config{})
	oldFlag, oldChoice, oldDial := deviceFlag, connectSimulatorChoiceFn, dialAgentLadderFn
	t.Cleanup(func() { deviceFlag, connectSimulatorChoiceFn, dialAgentLadderFn = oldFlag, oldChoice, oldDial })
	connectSimulatorChoiceFn = func(context.Context, *simulatorChoice, bool) (*SelectedDevice, error) {
		t.Fatal("monitoring attempted to start or reconcile VM")
		return nil, nil
	}
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		t.Fatal("monitoring dialed an unavailable VM")
		return nil, nil, nil
	}
	for _, tc := range []struct {
		name     string
		statuses []vm.Status
		want     string
	}{
		{"missing", nil, "does not exist"},
		{"stopped", []vm.Status{{Name: "robot", Exists: true}}, "not running"},
		{"starting", []vm.Status{{Name: "robot", Exists: true, Running: true}}, "not running"},
		{"no forward", []vm.Status{runningVM("robot", vm.NetShared, 0)}, "no forwarded agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVMStatuses(t, tc.statuses...)
			deviceFlag = "vm:robot"
			_, err := connectToAgent(context.Background(), ReadOnlyMonitoring())
			if !errors.Is(err, errSimulatorUnavailable) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestReadOnlyMonitoringNeverRelaxesIdentityRefusal(t *testing.T) {
	setTempConfig(t, &config.Config{})
	stubVMStatuses(t, runningVM("robot", vm.NetUser, 50055))
	oldDial := dialAgentLadderFn
	t.Cleanup(func() { dialAgentLadderFn = oldDial })
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return nil, nil, &deviceIdentityRefusalError{msg: "wrong VM identity"}
	}
	_, matched, err := connectRunningSimulator(context.Background(), "vm:robot")
	if !matched || !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("identity failure changed: %v, %v", matched, err)
	}
}

func TestReadOnlyMonitoringSelectorValidationAndOptIn(t *testing.T) {
	for _, device := range []string{"192.168.0.2:50051", "device.local", "cloud:robot", ""} {
		conn, matched, err := connectRunningSimulator(context.Background(), device)
		if conn != nil || matched || err != nil {
			t.Fatalf("non-VM selector changed: %q, %v", device, err)
		}
	}
	if _, matched, err := connectRunningSimulator(context.Background(), "vm:../escape"); !matched || err == nil {
		t.Fatal("invalid VM selector accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, matched, err := connectRunningSimulator(ctx, "vm:robot"); !matched || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if len(monitoringOptions(false)) != 0 {
		t.Fatal("ordinary connections became read-only")
	}
	for _, makeCommand := range []func() *cobra.Command{newTopCmd, newDeviceLogsCmd, newDeviceInfoCmd} {
		command := makeCommand()
		flag := command.Flags().Lookup("read-only")
		if flag == nil || flag.DefValue != "false" {
			t.Fatalf("%s did not opt in explicitly", command.Name())
		}
	}
}

func TestReadOnlyMonitoringReportsTLSFailureWithoutClockRecovery(t *testing.T) {
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag, oldDial, oldDiscover, oldBroadcast, oldAttempted := deviceFlag, dialAgentLadderFn, discoverLANDevices, broadcastTimeFn, clockSkewSyncAttempted
	t.Cleanup(func() {
		deviceFlag, dialAgentLadderFn, discoverLANDevices, broadcastTimeFn, clockSkewSyncAttempted = oldFlag, oldDial, oldDiscover, oldBroadcast, oldAttempted
	})
	deviceFlag = "192.0.2.1:50051"
	clockSkewSyncAttempted = false
	cause := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	dials := 0
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		dials++
		return nil, nil, cause
	}
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	broadcastTimeFn = func(context.Context) error {
		t.Error("monitoring tried to change the device clock")
		return errors.New("unexpected clock recovery")
	}
	if _, err := connectToAgent(context.Background(), ReadOnlyMonitoring()); !errors.Is(err, cause) {
		t.Fatalf("connection failure changed: %v", err)
	}
	if dials != 1 || clockSkewSyncAttempted {
		t.Fatalf("monitoring attempted recovery: dials=%d, clock=%v", dials, clockSkewSyncAttempted)
	}
}
