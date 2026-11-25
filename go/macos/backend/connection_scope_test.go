package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type scopeVersionClient struct{ pb.WendyAgentServiceClient }

func (scopeVersionClient) GetAgentVersion(context.Context, *pb.GetAgentVersionRequest, ...grpc.CallOption) (*pb.GetAgentVersionResponse, error) {
	return &pb.GetAgentVersionResponse{}, nil
}

func TestChildSecureLANUsesInheritedConnectionScope(t *testing.T) {
	parent, child := New(), New()
	defer parent.tunnelSession.SetActive(false)
	defer child.tunnelSession.SetActive(false)
	defer child.Disconnect()
	var targetContext context.Context
	parent.connectTarget = func(ctx context.Context, selector string) (*grpcclient.AgentConnection, liteConnection, error) {
		if secure, _ := ctx.Value(secureLANConnectionKey{}).(bool); !secure || selector != "camera.local:50052" {
			return nil, nil, errors.New("inherited callback lost secure LAN policy or target")
		}
		targetContext = ctx
		return &grpcclient.AgentConnection{AgentService: scopeVersionClient{}}, nil, nil
	}
	// Preview backends inherit their parent's connector so all consumers use
	// its account generation, even for explicitly secure LAN selections.
	child.connectTarget = parent.connectTarget
	child.connectDevice = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, errors.New("secure LAN bypassed the inherited connector")
	}
	_, err := child.ConnectTargetRequest("camera.local:50052", false, child.ReserveConnection(), true)
	if err != nil {
		t.Fatal(err)
	}
	if targetContext == nil || targetContext.Err() != nil {
		t.Fatal("connection did not retain the inherited transport lifetime")
	}
	if parent.conn != nil || child.conn == nil {
		t.Fatal("child connection replaced the parent's interactive session")
	}
	if err := child.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(targetContext.Err(), context.Canceled) {
		t.Fatal("disconnect did not cancel the inherited connector's context")
	}
}

func TestChildSecureLANCancellationReachesInheritedConnector(t *testing.T) {
	parent, child := New(), New()
	defer parent.tunnelSession.SetActive(false)
	defer child.tunnelSession.SetActive(false)
	entered := make(chan context.Context, 1)
	parent.connectTarget = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, liteConnection, error) {
		entered <- ctx
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	child.connectTarget = parent.connectTarget
	request := child.ReserveConnection()
	result := make(chan error, 1)
	go func() {
		_, err := child.ConnectTargetRequest("camera.local:50052", false, request, true)
		result <- err
	}()
	select {
	case ctx := <-entered:
		if secure, _ := ctx.Value(secureLANConnectionKey{}).(bool); !secure {
			t.Error("inherited callback did not receive secure LAN policy")
		}
	case <-time.After(time.Second):
		t.Fatal("secure LAN did not use the inherited callback")
	}
	child.ReserveConnection() // Closing or replacing a preview retires its dial.
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled preview left its inherited dial running")
	}
	if child.conn != nil {
		t.Fatal("cancelled dial committed a connection")
	}
}
