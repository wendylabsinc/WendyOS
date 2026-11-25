package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type osReleaseClient struct {
	agentpb.WendyAgentServiceClient
	info *agentpb.GetAgentVersionResponse
}

func (c osReleaseClient) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return c.info, ctx.Err()
}

// checkOSRelease runs the device and catalog steps of a release check in order.
func checkOSRelease(ctx context.Context, conn *grpcclient.AgentConnection) (*DesktopOSRelease, error) {
	info, err := DesktopOSReleaseIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	return DesktopResolveOSRelease(ctx, info)
}

func TestDesktopOSReleaseUsesCLISelection(t *testing.T) {
	serveManifests(t, map[string]string{
		"manifests/master.json": `{"devices":{"jetson-agx-thor":{"latest":"0.21.2","latest_nightly":"nightly-newer","manifest_path":"manifests/thor.json"}}}`,
		"manifests/thor.json":   `{"versions":{"0.21.2":{"ota_update_path":"thor/default.wendy","nvme_ota_update_path":"thor/nvme.wendy"}}}`,
	})
	for _, tc := range []struct {
		name, installed, storage, suffix string
		available, current, blocked      bool
	}{
		{"older OS with NVMe", "WendyOS-0.18.2", "nvme", "nvme.wendy", true, false, false},
		{"default storage", "WendyOS-0.18.2", "emmc", "default.wendy", true, false, false},
		{"equal OS", "WendyOS-0.21.2", "nvme", "nvme.wendy", false, true, false},
		{"newer OS", "WendyOS-0.22.0", "nvme", "nvme.wendy", false, true, false},
		{"unknown installed OS", "", "nvme", "nvme.wendy", false, false, true},
		{"old incompatible stack", "WendyOS-0.16.0", "nvme", "nvme.wendy", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &agentpb.GetAgentVersionResponse{OsVersion: proto.String(tc.installed), DeviceType: proto.String("jetson-agx-thor"), StorageMedium: proto.String(tc.storage), Featureset: []string{"wendyos-update"}}
			conn := &grpcclient.AgentConnection{AgentService: osReleaseClient{info: info}}
			got, err := checkOSRelease(context.Background(), conn)
			if err != nil {
				t.Fatal(err)
			}
			cliURL, cliVersion, err := getLatestOTAInfoForDeviceType("jetson-agx-thor", tc.storage, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.Latest != cliVersion || got.ArtifactURL != cliURL || got.ArtifactURL != gcsBaseURL+"/thor/"+tc.suffix || got.Available != tc.available || got.Current != tc.current || (got.BlockedReason != "") != tc.blocked {
				t.Fatalf("wrong release: %+v", got)
			}
		})
	}
}

func TestDesktopOSReleaseUnknownCatalogNeverMeansCurrent(t *testing.T) {
	serveManifests(t, map[string]string{
		"manifests/master.json": `{"devices":{"no-ota":{"latest":"0.21.2","manifest_path":"manifests/no-ota.json"}}}`,
		"manifests/no-ota.json": `{"versions":{"0.21.2":{}}}`,
	})
	for _, device := range []string{"", "not-in-catalog", "no-ota"} {
		conn := &grpcclient.AgentConnection{AgentService: osReleaseClient{info: &agentpb.GetAgentVersionResponse{OsVersion: proto.String("WendyOS-0.18.2"), DeviceType: proto.String(device)}}}
		if value, err := checkOSRelease(context.Background(), conn); err == nil || value != nil {
			t.Fatalf("unknown release for %q reported as %+v: %v", device, value, err)
		}
	}
}

func TestDesktopOSReleaseHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &grpcclient.AgentConnection{AgentService: osReleaseClient{info: &agentpb.GetAgentVersionResponse{}}}
	if _, err := DesktopOSReleaseIdentity(ctx, conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	info := &agentpb.GetAgentVersionResponse{OsVersion: proto.String("WendyOS-0.18.2"), DeviceType: proto.String("jetson-agx-thor")}
	if _, err := DesktopResolveOSRelease(ctx, info); !errors.Is(err, context.Canceled) {
		t.Fatalf("catalog lookup ignored cancellation: %v", err)
	}
}
