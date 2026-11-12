package commands

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Exercise the actual Cobra command and Docker subprocess boundary. No daemon
// is contacted: the executable records the build arguments and exits normally.
func TestBuildWithoutAgentPassesConfiguredPlatformToDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the subprocess fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	chdirTo(t, dir)
	for name, content := range map[string]string{
		"wendy.json": `{"appId":"platform-regression","platform":"wendyos/amd64","entitlements":[]}`,
		"Dockerfile": "FROM scratch\n",
		"docker":     "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WENDY_BUILD_ARGUMENTS\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	arguments := filepath.Join(dir, "arguments")
	t.Setenv("WENDY_BUILD_ARGUMENTS", arguments)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_AGENT_SOCKET", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Close the endpoint to reproduce an ordinary unavailable target without
	// discovery, a real device, or a user's saved default-device recovery flow.
	address := listener.Addr().String()
	_ = listener.Close()
	oldDevice := deviceFlag
	deviceFlag = address
	t.Cleanup(func() { deviceFlag = oldDevice })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := newBuildCmd()
	cmd.SetArgs([]string{"--builder", "docker", "--dockerfile", "Dockerfile"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"buildx", "build", "--platform", "linux/amd64", "-f", "Dockerfile", "-t", "platform-regression:latest", "--load", "."}
	if !slices.Equal(got, want) {
		t.Fatalf("Docker arguments = %q, want %q", got, want)
	}
}

type platformRegressionAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	calls atomic.Int32
	arch  string
	err   error
}

func (s *platformRegressionAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	s.calls.Add(1)
	return &agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: s.arch}, s.err
}

func TestBuildPlatformConfigurationAndConnectedAgent(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{"", "linux/arm64"}, {"linux/amd64", "linux/amd64"},
		{"wendyos/amd64", "linux/amd64"}, {"darwin", "darwin/arm64"},
	} {
		for _, target := range []*SelectedDevice{nil, {}} {
			if got := resolveBuildPlatform(context.Background(), target, tc.config); got != tc.want {
				t.Errorf("no Agent config %q: got %s, want %s", tc.config, got, tc.want)
			}
		}
	}
	for _, tc := range []struct {
		name, arch, config, want string
		err                      error
	}{
		{"connected architecture", "amd64", "", "linux/amd64", nil},
		{"connected explicit architecture", "arm64", "linux/amd64", "linux/amd64", nil},
		{"connected OS override", "amd64", "darwin", "darwin/amd64", nil},
		{"connected alias", "amd64", "wendyos", "linux/amd64", nil},
		{"missing reported architecture", "", "", "linux/arm64", nil},
		{"failed version retains explicit platform", "", "linux/amd64", "linux/amd64", status.Error(codes.Unavailable, "fixture unavailable")},
		{"failed version retains default", "", "", "linux/arm64", status.Error(codes.Unavailable, "fixture unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			service := &platformRegressionAgent{arch: tc.arch, err: tc.err}
			server := grpc.NewServer()
			agentpb.RegisterWendyAgentServiceServer(server, service)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			target := &SelectedDevice{Agent: &grpcclient.AgentConnection{AgentService: agentpb.NewWendyAgentServiceClient(conn)}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if got := resolveBuildPlatform(ctx, target, tc.config); got != tc.want {
				t.Fatalf("connected platform = %s, want %s", got, tc.want)
			}
			if service.calls.Load() != 1 {
				t.Fatal("platform was not obtained across the real Agent RPC")
			}
		})
	}
}
