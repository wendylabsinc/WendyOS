package commands

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestMCPConnectionProvisioningKeepsStdoutForJSONRPC(t *testing.T) {
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	previous := connectCloudDeviceSelectorFn
	t.Cleanup(func() { connectCloudDeviceSelectorFn = previous })
	defer forceBuildProgressInteractive(true)()
	connectCloudDeviceSelectorFn = func(ctx context.Context, _ cloudDeviceSelector) (*SelectedDevice, error) {
		if !detachedJSONRun(ctx) {
			t.Fatal("MCP provisioning did not reserve stdout")
		}
		err := runBuildWithProgress(ctx, "Building simulator fixture", dumpRawAlways, func(_ context.Context, stream, logw io.Writer) error {
			io.WriteString(logw, "fixture setup\n")
			io.WriteString(stream, "#1 [1/1] RUN simulator\n#1 DONE 0.1s\n")
			return nil
		})
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{}}, err
	}
	stdout, stderr := captureBoth(t, func() {
		if _, err := connectMCPDevice(context.Background(), "cloud://cloud.example:443/org/7/asset/42"); err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "" || !strings.Contains(stderr, "Building simulator fixture") {
		t.Fatalf("provisioning corrupted MCP stdout: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestMCPChunkUploadHeartbeatKeepsStdoutForJSONRPC(t *testing.T) {
	defer forceBuildProgressInteractive(true)()
	previous := chunkPushPlainHeartbeatInterval
	t.Cleanup(func() { chunkPushPlainHeartbeatInterval = previous })
	chunkPushPlainHeartbeatInterval = time.Millisecond
	ctx := context.WithValue(context.Background(), detachedJSONRunKey{}, true)
	diffID := "sha256:" + strings.Repeat("ab", 32)
	client := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			return &agentpb.QueryLayersResponse{Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: 4096}}}
		},
	}
	stdout, stderr := captureBoth(t, func() {
		_, err := pushLayersWithProgress(ctx, client, []localLayer{{DiffID: diffID}}, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
			time.Sleep(30 * time.Millisecond)
			return nil
		}, chunkUploadConfig{}, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "" || !strings.Contains(stderr, "...") {
		t.Fatalf("upload heartbeat corrupted MCP stdout: stdout=%q stderr=%q", stdout, stderr)
	}
}
