package grpcclient

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// agentRPCs is every method of every agent service this build links in.
func agentRPCs() []string {
	var methods []string
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(file.Package()), "wendy.agent.services.") {
			return true
		}
		for i := range file.Services().Len() {
			service := file.Services().Get(i)
			for j := range service.Methods().Len() {
				methods = append(methods, "/"+string(service.FullName())+"/"+string(service.Methods().Get(j).Name()))
			}
		}
		return true
	})
	slices.Sort(methods)
	return methods
}

func TestEveryAgentRPCIsClassified(t *testing.T) {
	methods := agentRPCs()
	if len(methods) < 100 {
		t.Fatalf("found %d agent RPCs; the agent service descriptors are not linked in", len(methods))
	}
	for _, method := range methods {
		read, write := readRPCs[method], writeRPCs[method]
		switch {
		case read && write:
			t.Errorf("%s is classified both read and write", method)
		case !read && !write:
			t.Errorf("%s is not classified: add it to readRPCs only if it changes nothing on the device, otherwise to writeRPCs", method)
		}
	}
	for _, list := range []map[string]bool{readRPCs, writeRPCs} {
		for method := range list {
			if !slices.Contains(methods, method) {
				t.Errorf("%s is classified but is not an agent RPC", method)
			}
		}
	}
}

// recordingAgent answers every method with UNIMPLEMENTED and records which
// methods reached it.
type recordingAgent struct {
	mu      sync.Mutex
	reached []string
}

func (a *recordingAgent) handle(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	a.mu.Lock()
	a.reached = append(a.reached, method)
	a.mu.Unlock()
	return status.Error(codes.Unimplemented, "recording agent")
}

func (a *recordingAgent) methods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reached)
}

func newRecordingConnection(t *testing.T) (*AgentConnection, *recordingAgent) {
	t.Helper()
	agent := &recordingAgent{}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnknownServiceHandler(agent.handle))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewFromConn(conn), agent
}

func TestReadOnlyContextRefusesWritesBeforeSending(t *testing.T) {
	conn, agent := newRecordingConnection(t)
	ctx := WithReadOnly(context.Background())
	if !IsReadOnly(ctx) || IsReadOnly(context.Background()) {
		t.Fatal("IsReadOnly does not report WithReadOnly")
	}

	_, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "StopContainer") {
		t.Fatalf("unary write: %v; want PermissionDenied naming the method", err)
	}
	stream, err := conn.ShellService.HostShell(ctx)
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("streaming write: %v; want PermissionDenied", err)
	}
	_, err = agentpbv2.NewROS2ServiceClient(conn.Client()).SetParam(ctx, &agentpbv2.SetROS2ParamRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("write through Client(): %v; want PermissionDenied", err)
	}
	if reached := agent.methods(); len(reached) != 0 {
		t.Fatalf("refused writes reached the agent: %v", reached)
	}

	// Reads pass in a read-only context, and writes pass outside one.
	if _, err := conn.ContainerService.ListVolumes(ctx, &agentpb.ListVolumesRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("read: %v; want the agent's UNIMPLEMENTED", err)
	}
	if _, err := agentpbv2.NewROS2ServiceClient(conn.Client()).ListTopics(ctx, &agentpbv2.ListROS2TopicsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("read through Client(): %v; want the agent's UNIMPLEMENTED", err)
	}
	if _, err := conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("write outside a read-only context: %v; want the agent's UNIMPLEMENTED", err)
	}
	want := []string{
		agentpb.WendyContainerService_ListVolumes_FullMethodName,
		agentpbv2.ROS2Service_ListTopics_FullMethodName,
		agentpb.WendyContainerService_StopContainer_FullMethodName,
	}
	if reached := agent.methods(); !slices.Equal(reached, want) {
		t.Fatalf("agent received %v; want %v", reached, want)
	}
}

func TestReadOnlyContextRefusesAnUnclassifiedMethod(t *testing.T) {
	conn, agent := newRecordingConnection(t)
	err := conn.Client().Invoke(WithReadOnly(context.Background()), "/wendy.agent.services.v9.Future/Read", &agentpb.ListVolumesRequest{}, &agentpb.ListVolumesResponse{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unclassified method: %v; want PermissionDenied", err)
	}
	if reached := agent.methods(); len(reached) != 0 {
		t.Fatalf("an unclassified method reached the agent: %v", reached)
	}
}
