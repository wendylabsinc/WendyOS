//go:build ignore

// Local plaintext positive control for the device-cache trust regression.
package main

import (
	"context"
	"fmt"
	"net"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type agent struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (agent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "test-plaintext", Os: "linux"}, nil
}

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, agent{})
	fmt.Println(listener.Addr().String())
	if err := server.Serve(listener); err != nil {
		panic(err)
	}
}
