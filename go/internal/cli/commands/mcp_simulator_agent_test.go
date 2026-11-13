package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

func TestMCPSimulatorAgentSetupSkipsCompatibleAndRejectsOtherTargets(t *testing.T) {
	for _, scenario := range []string{"compatible", "physical", "wrong VM", "stopped", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfile(t, store, "go2-test")
			conn := robotTestRunningVM(t, "go2-test", profile)
			info := conn.AgentService.(*fakeAgentVersionClient).resp
			info.CpuArchitecture = "arm64"
			previousConnect, previousResolve := simulatorUpdateConnect, simulatorUpdateResolve
			t.Cleanup(func() { simulatorUpdateConnect, simulatorUpdateResolve = previousConnect, previousResolve })
			simulatorUpdateResolve = func(string, string, bool) ([]byte, string, string, error) {
				t.Fatal("unexpected update for an incompatible target or already capable agent")
				return nil, "", "", nil
			}
			calls := 0
			simulatorUpdateConnect = func(_ context.Context, name, addr string) (*grpcclient.AgentConnection, *agentpb.GetAgentVersionResponse, error) {
				calls++
				if name != "go2-test" || addr != "127.0.0.1:50053" {
					t.Fatal("update escaped the named VM")
				}
				return conn, info, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "physical":
				deviceType := "jetson-orin-nano"
				info.DeviceType = &deviceType
			case "wrong VM":
				conn.SimulatorName = "another-vm"
			case "stopped":
				vmStatusesFn = func() ([]vm.Status, error) { return nil, nil }
			case "canceled":
				cancel()
			}
			result, err := updateMCPSimulatorAgent(ctx, "go2-test")
			if scenario == "compatible" {
				if err != nil || result.Updated || !result.CapabilityVerified {
					t.Fatalf("compatible agent result: %+v, %v", result, err)
				}
			} else if err == nil || result != nil {
				t.Fatalf("accepted %s target: %+v, %v", scenario, result, err)
			}
			if (scenario == "stopped" || scenario == "canceled") && calls != 0 {
				t.Fatal("dialed a stopped or canceled target")
			}
		})
	}
}

func TestMCPSimulatorAgentSetupUploadsOfficialArtifactAndVerifiesCapability(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(map[bool]string{true: "supported release", false: "unsupported release"}[supported], func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfile(t, store, "go2-test")
			conn := robotTestRunningVM(t, "go2-test", profile)
			old := conn.AgentService.(*fakeAgentVersionClient).resp
			old.CpuArchitecture, old.Version, old.Featureset = "arm64", "old", nil
			artifact := []byte("official test artifact")
			hash := sha256.Sum256(artifact)
			digest := hex.EncodeToString(hash[:])
			fresh := proto.Clone(old).(*agentpb.GetAgentVersionResponse)
			fresh.Version, fresh.BinarySha256 = "2026.09.28-142251", digest
			if supported {
				fresh.Featureset = []string{"go2-virtual-robot"}
			}
			next := &grpcclient.AgentConnection{SimulatorName: "go2-test", AgentService: &fakeAgentVersionClient{resp: fresh}}
			previousConnect, previousResolve, previousUpload := simulatorUpdateConnect, simulatorUpdateResolve, simulatorUpdateUpload
			t.Cleanup(func() {
				simulatorUpdateConnect, simulatorUpdateResolve, simulatorUpdateUpload = previousConnect, previousResolve, previousUpload
			})
			uploads := 0
			simulatorUpdateConnect = func(_ context.Context, name, addr string) (*grpcclient.AgentConnection, *agentpb.GetAgentVersionResponse, error) {
				if name != "go2-test" || addr != "127.0.0.1:50053" {
					t.Fatal("reconnect escaped the named VM")
				}
				if uploads == 0 {
					return conn, old, nil
				}
				return next, fresh, nil
			}
			simulatorUpdateResolve = func(osName, arch string, nightly bool) ([]byte, string, string, error) {
				if osName != "wendyos" || arch != "arm64" || nightly {
					t.Fatal("unexpected artifact platform or channel")
				}
				return artifact, fresh.Version, "gcs", nil
			}
			simulatorUpdateUpload = func(_ context.Context, service agentpb.WendyAgentServiceClient, data []byte, checksum string) error {
				uploads++
				if service != conn.AgentService || string(data) != string(artifact) || checksum != digest {
					t.Fatal("wrong update target or artifact")
				}
				return errAgentUpdateUnconfirmed // A restarted agent can drop its upload acknowledgement.
			}
			result, err := updateMCPSimulatorAgent(context.Background(), "go2-test")
			if supported {
				if err != nil || !result.Updated || !result.CapabilityVerified || result.Version != fresh.Version {
					t.Fatalf("updated agent not verified: %+v, %v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "robot support is not available") || result != nil {
				t.Fatalf("unsupported release reported ready: %+v, %v", result, err)
			}
			if uploads != 1 {
				t.Fatalf("expected one upload, got %d", uploads)
			}
		})
	}
}
