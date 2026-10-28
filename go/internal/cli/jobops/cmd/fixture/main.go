// workflow-fixture runs the real Wendy MCP handlers against a local test host.
// It is an agent-evaluation fixture, never a device service or deployment tool.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type host struct {
	agentpb.UnimplementedWendyShellServiceServer
	agentpb.UnimplementedWendyAgentServiceServer
}

func (h *host) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "evaluation-fixture", Os: runtime.GOOS, CpuArchitecture: runtime.GOARCH}, nil
}
func (h *host) HostShell(stream grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	argv := start.GetStart().GetCommand()
	if len(argv) != 4 || argv[0] != "python3" || argv[1] != "-c" {
		return fmt.Errorf("fixture accepts only fixed Wendy probe calls")
	}
	if root := os.Getenv("WENDY_EVAL_ROOT"); root != "" {
		var payload map[string]any
		if err = json.Unmarshal([]byte(argv[3]), &payload); err != nil {
			return err
		}
		if value, ok := payload["path"].(string); ok {
			relative, err := filepath.Rel(root, value)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("fixture refuses paths outside its workspace")
			}
		}
		if spec, ok := payload["spec"]; ok {
			var allowed any
			if err = json.Unmarshal([]byte(os.Getenv("WENDY_EVAL_SPEC_JSON")), &allowed); err != nil || !reflect.DeepEqual(spec, allowed) {
				return fmt.Errorf("fixture accepts only its sealed CPU specification")
			}
		}
	}
	command := exec.CommandContext(stream.Context(), argv[0], argv[1:]...)
	data, err := command.CombinedOutput()
	code := int32(0)
	if err != nil {
		code = 1
	}
	if err = stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: data}}); err != nil {
		return err
	}
	return stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: code}})
}
func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer()
	h := &host{}
	agentpb.RegisterWendyAgentServiceServer(server, h)
	agentpb.RegisterWendyShellServiceServer(server, h)
	go server.Serve(ln)
	defer server.Stop()
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	defer cc.Close()
	conn := grpcclient.NewFromConn(cc)
	// Test-only adapter: real transport authentication is independently tested
	// against Spark. This loopback fixture has no credentials or real devices.
	conn.IsMTLS = true
	conn.Host = "evaluation-fixture"
	conn.Addr = "evaluation-fixture"
	srv := mcp.New(&config.Config{}, nil)
	srv.SetConn(conn)
	srv.SetLANDiscoverer(func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil })
	if err = srv.SetToolGroups([]string{"observability"}); err != nil {
		panic(err)
	}
	if profile := os.Getenv("WENDY_EVAL_PROFILE"); profile != "" {
		if err = srv.SetToolProfile(profile); err != nil {
			panic(err)
		}
	}
	if err = srv.Start(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
