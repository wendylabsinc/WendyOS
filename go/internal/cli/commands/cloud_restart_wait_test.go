package commands

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// slowUplinkTunnelSetup is how long a fresh Cloud tunnel took to answer its
// first GetAgentVersion on 2026-10-25: 6-10 s, to a Jetson on a phone hotspot
// (mesh-2, dev Cloud). A 3 s probe failed every time on the same idle agent.
const slowUplinkTunnelSetup = 10 * time.Second

// probeBudgetAgent answers GetAgentVersion at once and records how much time
// the caller's deadline left for it.
type probeBudgetAgent struct {
	agentpb.WendyAgentServiceClient
	budget time.Duration
}

func (a *probeBudgetAgent) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		a.budget = time.Until(deadline)
	}
	return &agentpb.GetAgentVersionResponse{Version: "dev"}, nil
}

// The connect step only builds a lazy client, so the whole tunnel is set up
// inside the readiness probe. With a 3 s probe every attempt was cancelled on a
// slow uplink, and `wendy cloud device update` reported a timeout for an agent
// that had restarted and was answering.
func TestWaitForCloudDeviceRestartGivesTheProbeTimeToBuildATunnel(t *testing.T) {
	agent := &probeBudgetAgent{}
	conn, err := waitForCloudDeviceRestart(t.Context(), "mesh-2", "asset-id", func(context.Context) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{AgentService: agent}, nil
	})
	if err != nil {
		t.Fatalf("waitForCloudDeviceRestart() error = %v", err)
	}
	defer conn.Close()
	if agent.budget < slowUplinkTunnelSetup {
		t.Fatalf("readiness probe got %v to build a Cloud tunnel and answer; a slow uplink needs %v", agent.budget.Round(time.Second), slowUplinkTunnelSetup)
	}
	if _, ok := conn.CachedAgentVersion(); !ok {
		t.Fatal("the probe's version response was not cached on the returned connection")
	}
}

// `device update` must not wrap a Cloud reconnect in the LAN-sized restart
// window: waitForCloudDeviceRestart is built for cloudAgentRestartTimeout, and
// the restarted agent needs part of that just to register with the relay again.
func TestAgentRestartTimeoutForConn(t *testing.T) {
	cloud := &grpcclient.AgentConnection{Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) {
		t.Fatal("choosing a timeout must not reconnect")
		return nil, nil
	}}
	lan := &grpcclient.AgentConnection{Host: "wendyos-mesh-2.local"}
	tests := []struct {
		name   string
		conn   *grpcclient.AgentConnection
		osName string
		want   time.Duration
	}{
		{"LAN Linux keeps the default", lan, "wendyos", defaultAgentRestartTimeout},
		{"no connection keeps the default", nil, "", defaultAgentRestartTimeout},
		{"LAN Mac keeps the Mac window", lan, "darwin", darwinAgentRestartTimeout},
		{"Cloud Linux gets the Cloud restart window", cloud, "wendyos", cloudAgentRestartTimeout},
		{"Cloud Mac gets the longer of the two", cloud, "darwin", max(darwinAgentRestartTimeout, cloudAgentRestartTimeout)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentRestartTimeoutForConn(tc.conn, tc.osName); got != tc.want {
				t.Fatalf("agentRestartTimeoutForConn(%q) = %v, want %v", tc.osName, got, tc.want)
			}
		})
	}
}
