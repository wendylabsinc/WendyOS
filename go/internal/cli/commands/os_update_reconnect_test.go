package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type osReconnectAgentClient struct {
	agentpb.WendyAgentServiceClient
	version      string
	result       *agentpb.GetOSUpdateStatusResponse
	versionCalls int
	statusCalls  int
}

func (c *osReconnectAgentClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.versionCalls++
	return &agentpb.GetAgentVersionResponse{OsVersion: &c.version}, nil
}

func (c *osReconnectAgentClient) GetOSUpdateStatus(context.Context, *agentpb.GetOSUpdateStatusRequest, ...grpc.CallOption) (*agentpb.GetOSUpdateStatusResponse, error) {
	c.statusCalls++
	return c.result, nil
}

type osReconnectDriverClient struct {
	agentpbv2.WendyDriverServiceClient
	calls int
}

func (c *osReconnectDriverClient) ListDrivers(context.Context, *agentpbv2.ListDriversRequest, ...grpc.CallOption) (*agentpbv2.ListDriversResponse, error) {
	c.calls++
	return &agentpbv2.ListDriversResponse{}, nil
}

func TestOSUpdateReconnectPreservesDirectPort(t *testing.T) {
	setTempConfig(t, &config.Config{})
	addr := startPlaintextVersionAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialOSUpdateAgent(ctx, &grpcclient.AgentConnection{
		Host: "display-name.invalid",
		Addr: addr,
	})
	if err != nil {
		t.Fatalf("reconnect to original endpoint: %v", err)
	}
	defer conn.Close()
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatalf("query original endpoint: %v", err)
	}
}

func TestOSUpdatePollRetriesPinnedCloudConnection(t *testing.T) {
	client := &osReconnectAgentClient{version: "WendyOS-0.20.0"}
	attempts := 0
	prior := &grpcclient.AgentConnection{
		Host: "cloud-display-name.invalid",
		Addr: "wrong-endpoint.invalid:50051",
		Reconnect: func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			attempts++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 15*time.Second {
				t.Error("Cloud reconnect attempt must have a bounded deadline")
			}
			if attempts == 1 {
				return nil, context.DeadlineExceeded
			}
			return &grpcclient.AgentConnection{AgentService: client}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	if err := pollDeviceOnline(ctx, prior); err != nil {
		t.Fatalf("poll pinned Cloud asset: %v", err)
	}
	if time.Since(started) < 5*time.Second {
		t.Error("poll did not allow the device to begin rebooting")
	}
	if attempts != 2 || client.versionCalls != 1 {
		t.Fatalf("reconnect attempts=%d, version probes=%d; want 2 and 1", attempts, client.versionCalls)
	}
}

func TestOSUpdateReconnectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prior := &grpcclient.AgentConnection{
		Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) {
			t.Fatal("a cancelled operation must not reconnect")
			return nil, nil
		},
	}
	if err := pollDeviceOnline(ctx, prior); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll cancellation = %v", err)
	}
	if err := reportOSUpdateOutcome(ctx, prior, "WendyOS-0.19.5", osUpdateBaseline{preVersion: "WendyOS-0.19.5", recordCleared: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("status cancellation = %v", err)
	}
}

func TestOSUpdateOutcomeUsesPinnedCloudConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome agentpb.GetOSUpdateStatusResponse_Outcome
		version string
		wantErr bool
		message string
	}{
		{"committed", agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED, "WendyOS-0.20.0", false, "Update verified"},
		{"rolled back", agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK, "WendyOS-0.19.5", true, "rolled back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &osReconnectAgentClient{
				version: tc.version,
				result: &agentpb.GetOSUpdateStatusResponse{
					HasResult:     true,
					Outcome:       tc.outcome,
					OldOsVersion:  "WendyOS-0.19.5",
					NewOsVersion:  "WendyOS-0.20.0",
					CreatedAtUnix: time.Now().Unix(),
				},
			}
			drivers := &osReconnectDriverClient{}
			reconnects := 0
			prior := &grpcclient.AgentConnection{
				Host: "cloud-display-name.invalid",
				Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) {
					reconnects++
					return &grpcclient.AgentConnection{AgentService: client, DriverService: drivers}, nil
				},
			}
			var err error
			out := captureStdout(t, func() {
				err = reportOSUpdateOutcome(context.Background(), prior, "WendyOS-0.19.5", osUpdateBaseline{preVersion: "WendyOS-0.19.5", recordCleared: true})
			})
			if (err != nil) != tc.wantErr || !strings.Contains(out, tc.message) {
				t.Fatalf("outcome err=%v, output=%q", err, out)
			}
			if reconnects != 2 || client.statusCalls != 1 || client.versionCalls != 1 || drivers.calls != 1 {
				t.Fatalf("pinned reconnects=%d, status=%d, version=%d, drivers=%d; want 2, 1, 1, 1", reconnects, client.statusCalls, client.versionCalls, drivers.calls)
			}
		})
	}
}
