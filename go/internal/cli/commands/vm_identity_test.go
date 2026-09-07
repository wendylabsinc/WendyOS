package commands

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestVMIdentityUsesNameNotLoopbackOrPort(t *testing.T) {
	for alias, want := range map[string]string{"vm:one": "vm:one", "vm:two": "vm:two", "sim": "vm:sim", "simulator": "vm:sim", "127.0.0.1:50100": "127.0.0.1"} {
		if got := pinKeyForAddr(alias); got != want {
			t.Fatalf("pin key for %s = %s, want %s", alias, got, want)
		}
	}
	cfg := &config.Config{}
	cfg.SetDevicePin("127.0.0.1", 1, "cloud", "old-container")
	cfg.SetDevicePin("vm:one", 1, "cloud", "one")
	cfg.SetDevicePin("vm:two", 1, "cloud", "two")
	oldLoad, oldDial := loadConfigForPinFn, dialAgentLadderFn
	t.Cleanup(func() { loadConfigForPinFn, dialAgentLadderFn = oldLoad, oldDial })
	loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
	for _, tc := range []struct{ name, addr, asset string }{
		{"one", "127.0.0.1:50053", "one"},
		{"one", "127.0.0.1:50100", "one"},
		{"two", "127.0.0.1:50053", "two"},
		{"fresh", "127.0.0.1:50051", ""},
	} {
		dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
			if target.PinKey != "vm:"+tc.name || target.Addr != tc.addr {
				t.Fatalf("wrong target: %+v", target)
			}
			if tc.asset == "" {
				if target.pinned() {
					t.Fatal("fresh VM inherited localhost pin")
				}
			} else if target.Expected == nil || target.Expected.EntityID != tc.asset {
				t.Fatalf("wrong identity constraint: %+v", target)
			}
			return &grpcclient.AgentConnection{AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
		}
		conn, _, err := connectSimulatorAgent(context.Background(), tc.name, tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
}

func TestVMReconnectPreservesAliasAndFullEndpoint(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDevicePin("127.0.0.1", 1, "cloud", "unrelated-container")
	oldLoad, oldDial := loadConfigForPinFn, dialAgentLadderFn
	t.Cleanup(func() { loadConfigForPinFn, dialAgentLadderFn = oldLoad, oldDial })
	loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
	calls := 0
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		if target.PinKey != "vm:second" || target.Addr != "127.0.0.1:50103" || target.pinned() {
			t.Fatalf("reconnect lost VM identity: %+v", target)
		}
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	conn, _, err := connectSimulatorAgent(context.Background(), "second", "127.0.0.1:50103")
	if err != nil {
		t.Fatal(err)
	}
	for _, reconnect := range []func(context.Context) (*grpcclient.AgentConnection, error){
		func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			return reconnectAgentAfterRestart(ctx, conn)
		},
		updatedAgentReconnectFunc(context.Background(), conn),
	} {
		next, err := reconnect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if next.SimulatorName != "second" || next.Addr != conn.Addr {
			t.Fatalf("identity not retained: %+v", next)
		}
		next.Close()
	}
	conn.Close()
	if calls != 3 {
		t.Fatalf("got %d named dials, want 3", calls)
	}
	// An identity change after an update must fail immediately, not retry via
	// generic localhost discovery or an unauthenticated connection.
	cfg.SetDevicePin("vm:second", 1, "cloud", "second")
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		if target.Expected == nil || target.Expected.EntityID != "second" {
			t.Fatalf("lost expected identity: %+v", target)
		}
		return nil, nil, &deviceIdentityRefusalError{msg: "changed identity"}
	}
	if _, err := reconnectAgentAfterRestart(context.Background(), conn); !blocksUnauthenticatedFallback(err) {
		t.Fatalf("did not retain identity refusal: %v", err)
	}
}

func TestVMSelectionKeepsAliasAcrossEveryFrontDoor(t *testing.T) {
	original, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.Save(original) })
	cfg := *original
	cfg.DefaultDevice = "vm:second"
	if err := config.Save(&cfg); err != nil {
		t.Fatal(err)
	}
	oldFlag, oldConnect := deviceFlag, connectSimulatorChoiceFn
	t.Cleanup(func() { deviceFlag, connectSimulatorChoiceFn = oldFlag, oldConnect })
	var names []string
	connectSimulatorChoiceFn = func(_ context.Context, choice *simulatorChoice, _ bool) (*SelectedDevice, error) {
		names = append(names, choice.Name)
		return &SelectedDevice{PinKey: "vm:" + choice.Name, Agent: &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:50053"}}, nil
	}
	for _, flag := range []string{"vm:second", ""} {
		deviceFlag = flag
		if _, err := connectToAgent(context.Background()); err != nil {
			t.Fatal(err)
		}
		if deviceFlag != flag {
			t.Fatal("connect mutated global device selection")
		}
		selected, err := resolveTargetInner(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		name, err := defaultDeviceNameFor(selected)
		if err != nil || name != "vm:second" {
			t.Fatalf("default lost alias/port: %q, %v", name, err)
		}
	}
	_, err = connectPickedLANDevice(context.Background(), &models.DiscoveredDevice{LAN: &models.LANDevice{ID: "vm:second"}}, "127.0.0.1:50051", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 5 {
		t.Fatalf("got %d VM selections, want 5", len(names))
	}
	for _, name := range names {
		if name != "second" {
			t.Fatal(name)
		}
	}
}
