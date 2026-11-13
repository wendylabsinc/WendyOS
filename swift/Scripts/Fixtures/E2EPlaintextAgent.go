//go:build ignore

// Local plaintext positive control for the device-cache trust regression.
package main

import (
	"context"
	"fmt"
	"net"
	"os"

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
	network, address := "tcp", "127.0.0.1:0"
	if len(os.Args) == 2 {
		network, address = "unix", os.Args[1]
	}
	listener, err := net.Listen(network, address)
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
