package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// DesktopDownloadedModelNames reads Ollama's inventory through the existing
// agent connection when the model's HTTP port cannot be reached. The caller
// must have discovered this port in DesktopModelPorts first.
func DesktopDownloadedModelNames(ctx context.Context, conn *grpcclient.AgentConnection, port uint32) ([]string, error) {
	if port == 0 || port > 65535 {
		return nil, errors.New("invalid model runtime port")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := conn.ContainerService.ExecContainer(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&pb.ExecContainerRequest{RequestType: &pb.ExecContainerRequest_Start{Start: &pb.ExecContainerRequest_ExecStart{
		AppName: fmt.Sprintf("sh.wendy.models.ollama.%d", port), Command: []string{"ollama", "list"}, Tty: false,
	}}})
	if err != nil {
		return nil, err
	}
	// Keep stdin open until the process exits; some agents cancel exec on EOF.
	var output strings.Builder
	for {
		response, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		switch value := response.GetResponseType().(type) {
		case *pb.ExecContainerResponse_StdoutData:
			if output.Len()+len(value.StdoutData) > 1<<20 {
				return nil, errors.New("model inventory is too large")
			}
			output.Write(value.StdoutData)
		case *pb.ExecContainerResponse_ExitCode:
			if value.ExitCode != 0 {
				return nil, fmt.Errorf("model inventory exited with code %d", value.ExitCode)
			}
			return parseDownloadedModelNames(output.String())
		}
	}
}

func parseDownloadedModelNames(output string) ([]string, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "NAME") {
		return nil, errors.New("model runtime did not return its downloaded model list")
	}
	var names []string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			names = append(names, fields[0])
		}
	}
	return names, nil
}
