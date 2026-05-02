package mcp

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// startMCPProxy starts a local TCP listener that proxies each incoming connection
// to the named container's MCP server via StreamMCP. Returns the listener address
// (e.g. "127.0.0.1:52341") and a close function.
func startMCPProxy(ctx context.Context, conn *grpcclient.AgentConnection, appName string) (addr string, closeFn func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("starting MCP proxy for %q: %w", appName, err)
	}

	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer func() { close(done) }()
		for {
			tcpConn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveMCPProxyConn(pctx, conn, appName, tcpConn)
		}
	}()

	return ln.Addr().String(), func() {
		cancel()
		ln.Close()
		<-done
	}, nil
}

func serveMCPProxyConn(ctx context.Context, conn *grpcclient.AgentConnection, appName string, tcpConn net.Conn) {
	defer tcpConn.Close()

	md := metadata.Pairs("app-name", appName)
	ctx = metadata.NewOutgoingContext(ctx, md)

	stream, err := conn.ContainerService.StreamMCP(ctx)
	if err != nil {
		return
	}

	errc := make(chan error, 2)

	// TCP → gRPC
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := tcpConn.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&agentpb.MCPChunk{Data: buf[:n]}); sendErr != nil {
					errc <- sendErr
					return
				}
			}
			if readErr != nil {
				errc <- readErr
				return
			}
		}
	}()

	// gRPC → TCP
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			if _, err := tcpConn.Write(chunk.Data); err != nil {
				errc <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-errc:
	}
}
