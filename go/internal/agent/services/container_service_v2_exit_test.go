package services

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// When the containerd runtime cannot read a task's exit result, the final
// ContainerOutput carries Err and a zero ExitCode. The v2 service must surface
// that as an Internal error rather than reporting a clean Exited{exit_code: 0}.
func TestContainerServiceV2_StartContainer_ResultErrorIsNotReportedAsCleanExit(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true, ExitCode: 0, Err: errors.New("task wait failed")}
	close(outputCh)

	mc := &mockContainerdClient{startOutputCh: outputCh}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	stream, err := client.StartContainer(context.Background(), &agentpbv2.StartContainerRequest{AppName: "myapp"})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	var lastErr error
	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			lastErr = recvErr
			break
		}
		if exited := resp.GetExited(); exited != nil {
			t.Fatalf("got Exited{exit_code: %d}; want an Internal error instead", exited.GetExitCode())
		}
	}

	if lastErr == io.EOF {
		t.Fatal("stream ended cleanly; want an Internal error for an unreadable exit result")
	}
	if status.Code(lastErr) != codes.Internal {
		t.Fatalf("error code = %v (%v); want Internal", status.Code(lastErr), lastErr)
	}
}
