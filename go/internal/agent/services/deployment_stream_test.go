package services

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type attachedDeploymentRuntime struct {
	*fakeDeploymentRuntime
	input  chan string
	starts atomic.Int32
}

func (f *attachedDeploymentRuntime) StartContainerWithStdin(_ context.Context, _ string, stdin io.Reader, _ string, _ *agentpb.RestartPolicy) (<-chan ContainerOutput, error) {
	f.starts.Add(1)
	go func() {
		data, _ := io.ReadAll(stdin)
		f.input <- string(data)
	}()
	return f.startOutputCh, nil
}

func TestAttachedDeployVerifiesOriginalProcessWithStdin(t *testing.T) {
	f := &attachedDeploymentRuntime{fakeDeploymentRuntime: newFakeDeploymentRuntime(), input: make(chan string, 1)}
	client, cleanup := startContainerServer(t, f)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.AttachContainer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpb.AttachContainerRequest{RequestType: &agentpb.AttachContainerRequest_Deploy{Deploy: deploymentTestRequest(true)}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentpb.AttachContainerRequest{RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: []byte("interactive input\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if result := event.GetDeployment(); result != nil {
			if result.State != agentpb.DeploymentState_READY || f.starts.Load() != 1 {
				t.Fatalf("outcome=%v, starts=%d", result, f.starts.Load())
			}
			break
		}
	}
	select {
	case got := <-f.input:
		if got != "interactive input\n" {
			t.Fatalf("stdin=%q", got)
		}
	case <-ctx.Done():
		t.Fatal("stdin was not forwarded")
	}
	close(f.startOutputCh)
}

func TestDeploymentOutcomeSurvivesSaturatedLogs(t *testing.T) {
	stream := &deploymentEventStream{events: make(chan *agentpb.RunContainerLayersResponse, 8)}
	log := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("log")}}}
	for i := 0; i < 16; i++ {
		_ = stream.Send(log)
	}
	result := &agentpb.DeploymentResult{State: agentpb.DeploymentState_ROLLED_BACK}
	_ = stream.Send(&agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Deployment{Deployment: result}})
	for i := 0; i < 16; i++ {
		_ = stream.Send(log)
	}
	for len(stream.events) > 0 {
		if event := <-stream.events; event.GetDeployment() == result {
			return
		}
	}
	t.Fatal("slow log reader lost deployment outcome")
}

// The caller resolves implicit readiness into app_config; the agent never
// infers a probe from an HTTP entitlement.
func TestVerifiedDeployRunsOnlyExplicitReadiness(t *testing.T) {
	f := newFakeDeploymentRuntime()
	f.probe = func(_ context.Context, probe *appconfig.ReadinessConfig) error {
		if probe != nil {
			t.Error("inherited HTTP entitlement became a service readiness probe")
		}
		return nil
	}
	client, cleanup := startContainerServer(t, f)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := deploymentTestRequest(false)
	req.AppConfig = []byte(`{"appId":"test-app","entitlements":[{"type":"http","port":8080}]}`)
	stream, err := client.RunContainer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if result := event.GetDeployment(); result != nil {
			if result.State != agentpb.DeploymentState_RUNNING {
				t.Fatalf("result=%v", result)
			}
			break
		}
	}
	close(f.startOutputCh)
}

func TestAttachedDeployRequiresVerified(t *testing.T) {
	f := newFakeDeploymentRuntime()
	client, cleanup := startContainerServer(t, f)
	defer cleanup()
	stream, err := client.AttachContainer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req := deploymentTestRequest(true)
	req.Verified = false
	if err := stream.Send(&agentpb.AttachContainerRequest{RequestType: &agentpb.AttachContainerRequest_Deploy{Deploy: req}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error=%v", err)
	}
	if f.prepared.Load() != 0 {
		t.Fatal("unverified attached deploy prepared a candidate")
	}
}
