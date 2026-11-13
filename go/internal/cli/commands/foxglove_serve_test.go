package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type foxgloveSelectedClient struct {
	agentpb.WendyContainerServiceClient
	execs     int
	cancel    context.CancelFunc
	container *agentpb.AppContainer
	deleted   *agentpb.DeleteContainerRequest
}

type foxgloveListStream struct {
	grpc.ServerStreamingClient[agentpb.ListContainersResponse]
	container *agentpb.AppContainer
	sent      bool
}

func (s *foxgloveListStream) Recv() (*agentpb.ListContainersResponse, error) {
	if s.sent || s.container == nil {
		return nil, io.EOF
	}
	s.sent = true
	return &agentpb.ListContainersResponse{Container: s.container}, nil
}

func (c *foxgloveSelectedClient) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	return &foxgloveListStream{container: c.container}, nil
}

func (c *foxgloveSelectedClient) DeleteContainer(_ context.Context, req *agentpb.DeleteContainerRequest, _ ...grpc.CallOption) (*agentpb.DeleteContainerResponse, error) {
	c.deleted = req
	return &agentpb.DeleteContainerResponse{}, nil
}

func (c *foxgloveSelectedClient) ExecContainer(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.ExecContainerRequest, agentpb.ExecContainerResponse], error) {
	c.execs++
	c.cancel()
	return nil, status.Error(codes.Canceled, "test stopped after forwarding used selected agent")
}

func foxgloveTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func TestFoxgloveServeUsesOneAgentForDeployAndForward(t *testing.T) {
	for _, selector := range []string{"", "192.168.100.126", "vm:robot", "cloud://cloud.example:443/org/2/asset/42"} {
		t.Run(selector, func(t *testing.T) {
			oldResolve, oldDeploy := resolveFoxgloveTarget, deployFoxgloveApp
			t.Cleanup(func() { resolveFoxgloveTarget, deployFoxgloveApp = oldResolve, oldDeploy })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &foxgloveSelectedClient{cancel: cancel}
			conn := &grpcclient.AgentConnection{ContainerService: client}
			resolutions := 0
			resolveFoxgloveTarget = func(_ context.Context, device string) (*SelectedDevice, error) {
				resolutions++
				if device != selector {
					t.Fatalf("selector = %q, want %q", device, selector)
				}
				return &SelectedDevice{Agent: conn}, nil
			}
			deployed := false
			var appDir string
			deployFoxgloveApp = func(_ context.Context, got *grpcclient.AgentConnection, dir string, cfg *appconfig.AppConfig, builder string) error {
				deployed = true
				appDir = dir
				if got != conn {
					t.Fatal("deployment used a different agent")
				}
				if cfg.AppID != foxgloveAppID || len(cfg.Services) != 0 {
					t.Fatalf("bridge must use the single-container deploy path: %+v", cfg)
				}
				if builder != "apple-container" {
					t.Fatalf("builder = %q, want apple-container", builder)
				}
				data, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "      socat \\") {
					t.Fatal("generated bridge must install its forwarding dependency")
				}
				return nil
			}
			err := foxgloveServe(ctx, foxgloveServeOpts{device: selector, localPort: foxgloveTestPort(t), distro: "humble", rmw: "rmw_cyclonedds_cpp", builder: "apple-container"})
			if err != nil {
				t.Fatal(err)
			}
			if resolutions != 1 || !deployed || client.execs != 1 {
				t.Fatalf("resolve=%d deploy=%v exec=%d", resolutions, deployed, client.execs)
			}
			if client.deleted != nil {
				t.Fatalf("current single-container deployment was deleted before redeploy: %+v", client.deleted)
			}
			if _, err := os.Stat(appDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary app was not removed: %v", err)
			}
		})
	}
}

func TestRemoveLegacyFoxgloveAppDeletesOnlyNamedServiceDeployment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		container  *agentpb.AppContainer
		wantDelete bool
	}{
		{name: "none"},
		{name: "current single container", container: &agentpb.AppContainer{AppName: foxgloveAppID}},
		{name: "different app", container: &agentpb.AppContainer{AppName: "example", Services: []*agentpb.ServiceEntry{{Name: "foxglove"}}}},
		{name: "legacy named service", container: &agentpb.AppContainer{AppName: foxgloveAppID, Services: []*agentpb.ServiceEntry{{Name: "foxglove"}}}, wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &foxgloveSelectedClient{container: tc.container}
			if err := removeLegacyFoxgloveApp(context.Background(), client); err != nil {
				t.Fatal(err)
			}
			if tc.wantDelete {
				if client.deleted == nil || client.deleted.AppName != multiServiceContainerName(foxgloveAppID, "foxglove") || !client.deleted.DeleteImage {
					t.Fatalf("legacy delete = %+v", client.deleted)
				}
			} else if client.deleted != nil {
				t.Fatalf("unexpected delete = %+v", client.deleted)
			}
		})
	}
}

func TestFoxgloveServeRejectsInvalidBuilderBeforeDeviceChanges(t *testing.T) {
	oldResolve := resolveFoxgloveTarget
	t.Cleanup(func() { resolveFoxgloveTarget = oldResolve })
	resolveFoxgloveTarget = func(context.Context, string) (*SelectedDevice, error) {
		t.Fatal("must reject an invalid builder before contacting a device")
		return nil, nil
	}
	err := foxgloveServe(context.Background(), foxgloveServeOpts{
		localPort: foxgloveTestPort(t), distro: "humble", rmw: "rmw_cyclonedds_cpp", builder: "podman",
	})
	if err == nil || !strings.Contains(err.Error(), "--builder") {
		t.Fatalf("error = %v, want invalid --builder", err)
	}
}

func TestFoxgloveServeRejectsLocalPortBeforeDeviceChanges(t *testing.T) {
	oldResolve := resolveFoxgloveTarget
	t.Cleanup(func() { resolveFoxgloveTarget = oldResolve })
	resolveFoxgloveTarget = func(context.Context, string) (*SelectedDevice, error) {
		t.Fatal("must reject unusable local ports before contacting a device")
		return nil, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, port := range []int{-1, 0, 65536, listener.Addr().(*net.TCPAddr).Port} {
		err := foxgloveServe(context.Background(), foxgloveServeOpts{localPort: port, distro: "humble", rmw: "rmw_cyclonedds_cpp"})
		if err == nil {
			t.Fatalf("port %d: wanted error", port)
		}
	}
}
