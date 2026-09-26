package grpcclient

import (
	"slices"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestAgentConnectionHasModelService(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if NewFromConn(conn).ModelService == nil {
		t.Fatal("AgentConnection.ModelService is not wired")
	}
	var rpcs []string
	for _, m := range agentpbv2.WendyModelService_ServiceDesc.Methods {
		rpcs = append(rpcs, m.MethodName)
	}
	for _, s := range agentpbv2.WendyModelService_ServiceDesc.Streams {
		rpcs = append(rpcs, s.StreamName)
	}
	slices.Sort(rpcs)
	if want := []string{"ListCatalog", "ListModels", "StartModel", "StopModel", "WatchModel"}; !slices.Equal(rpcs, want) {
		t.Fatalf("rpcs = %v, want %v", rpcs, want)
	}
}
