package commands

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestDesktopDownloadedModelsUsesSelectedRuntimeWithoutTTY(t *testing.T) {
	client := newFoxgloveExecTestClient(t, func(stream pb.WendyContainerService_ExecContainerServer) error {
		request, err := stream.Recv()
		if err != nil {
			return err
		}
		start := request.GetStart()
		if start.GetAppName() != "sh.wendy.models.ollama.11437" || start.GetTty() || !reflect.DeepEqual(start.GetCommand(), []string{"ollama", "list"}) {
			return fmt.Errorf("unexpected inventory request: %v", start)
		}
		if err := stream.Send(&pb.ExecContainerResponse{ResponseType: &pb.ExecContainerResponse_StdoutData{StdoutData: []byte("NAME ID SIZE MODIFIED\nhf.co/Qwen/model:Q4_K_M abc 2.5 GB now\n")}}); err != nil {
			return err
		}
		return stream.Send(&pb.ExecContainerResponse{ResponseType: &pb.ExecContainerResponse_ExitCode{ExitCode: 0}})
	})
	names, err := DesktopDownloadedModelNames(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, 11437)
	if err != nil || !reflect.DeepEqual(names, []string{"hf.co/Qwen/model:Q4_K_M"}) {
		t.Fatalf("inventory %v, %v", names, err)
	}
}

func TestDesktopDownloadedModelsRejectsIncompleteInventory(t *testing.T) {
	if _, err := parseDownloadedModelNames("Error: server unavailable"); err == nil {
		t.Fatal("accepted error output as inventory")
	}
	names, err := parseDownloadedModelNames("NAME ID SIZE MODIFIED\n")
	if err != nil || len(names) != 0 {
		t.Fatalf("empty inventory %v, %v", names, err)
	}
}
