package commands

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func TestDesktopModelRuntimeConfig(t *testing.T) {
	for profile := uint32(0); profile < 4; profile++ {
		req, err := desktopModelRuntime(11435, profile)
		if err != nil {
			t.Fatal(err)
		}
		var cfg appconfig.AppConfig
		if err = json.Unmarshal(req.AppConfig, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.AppID != "sh.wendy.models.ollama.11435" || !strings.Contains(req.ImageName, "@sha256:") || cfg.HasEntitlement("gpu") != (profile != 0) {
			t.Fatalf("bad runtime: %+v", req)
		}
		if !strings.Contains(strings.Join(req.Env, "\n"), "OLLAMA_HOST=0.0.0.0:11435") {
			t.Fatal("port not set")
		}
		if cfg.Validate() != nil {
			t.Fatal("invalid app config")
		}
	}
	for _, port := range []uint32{0, 65536} {
		if _, err := desktopModelRuntime(port, 0); err == nil {
			t.Fatal("invalid port accepted")
		}
	}
}

type runtimeInventory struct {
	pb.WendyContainerServiceClient
	app *pb.AppContainer
}

func (f runtimeInventory) ListContainers(context.Context, *pb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.ListContainersResponse], error) {
	return &runtimeList{app: f.app}, nil
}

type runtimeList struct {
	grpc.ClientStream
	app  *pb.AppContainer
	sent bool
}

func (s *runtimeList) Recv() (*pb.ListContainersResponse, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return &pb.ListContainersResponse{Container: s.app}, nil
}
func TestDesktopModelRuntimeDoesNotReplaceExistingApp(t *testing.T) {
	conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{app: &pb.AppContainer{AppName: "sh.wendy.models.ollama.11434", AppVersion: "unrelated", HttpPort: 11434}}}
	err := DesktopEnsureModelRuntime(context.Background(), conn, 11434, 0, func() bool { t.Fatal("must refuse before probing or deploying"); return false }, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "different settings") {
		t.Fatal("existing app not protected", err)
	}
}
func TestDesktopModelRuntimeRefusesOccupiedPort(t *testing.T) {
	conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{app: &pb.AppContainer{AppName: "another-app"}}}
	err := DesktopEnsureModelRuntime(context.Background(), conn, 11434, 0, func() bool { return true }, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatal("port collision not rejected", err)
	}
}

func TestDesktopModelRuntimeRecognizesLinuxDistributionIDs(t *testing.T) {
	for _, name := range []string{"linux", "ubuntu", "debian", "wendyos", "qcom"} {
		if desktopModelOS(name) != "linux" {
			t.Fatal("Linux distribution refused", name)
		}
	}
	for _, name := range []string{"darwin", "windows", "", "freebsd"} {
		if desktopModelOS(name) != name {
			t.Fatal("non-Linux platform accepted", name)
		}
	}
}

func TestDesktopModelAutoPortSkipsOccupied(t *testing.T) {
	conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{app: &pb.AppContainer{AppName: "other", HttpPort: 11434}}}
	var probes []uint32
	port, err := DesktopSelectModelPort(context.Background(), conn, 0, func(_ context.Context, p uint32) (bool, error) { probes = append(probes, p); return p == 11435, nil })
	if err != nil || port != 11436 || len(probes) != 2 {
		t.Fatalf("port=%d probes=%v err=%v", port, probes, err)
	}
}
func TestDesktopModelAutoPortReusesMatchingRuntime(t *testing.T) {
	req, _ := desktopModelRuntime(11438, 1)
	var cfg appconfig.AppConfig
	_ = json.Unmarshal(req.AppConfig, &cfg)
	conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{app: &pb.AppContainer{AppName: req.AppName, AppVersion: cfg.Version, HttpPort: 11438, RunningState: pb.AppRunningState_RUNNING}}}
	port, err := DesktopSelectModelPort(context.Background(), conn, 1, func(context.Context, uint32) (bool, error) { t.Fatal("should reuse running runtime"); return true, nil })
	if err != nil || port != 11438 {
		t.Fatal(port, err)
	}
}
func TestDesktopModelAutoPortDoesNotTreatFailedProbeAsFree(t *testing.T) {
	conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{}}
	_, err := DesktopSelectModelPort(context.Background(), conn, 0, func(context.Context, uint32) (bool, error) { return false, context.DeadlineExceeded })
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
}

func TestDesktopDiscoversInstalledModelRuntimePorts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		port  uint32
		state pb.AppRunningState
		want  bool
	}{
		{"sh.wendy.models.ollama.11435", 11435, pb.AppRunningState_RUNNING, true},
		{"sh.wendy.models.ollama.11435", 11435, pb.AppRunningState_STOPPED, false},
		{"unrelated-app", 11435, pb.AppRunningState_RUNNING, false},
		{"sh.wendy.models.ollama.11435", 11436, pb.AppRunningState_RUNNING, false},
		{"sh.wendy.models.ollama.65536", 65536, pb.AppRunningState_RUNNING, false},
	} {
		t.Run(tt.name+tt.state.String(), func(t *testing.T) {
			conn := &grpcclient.AgentConnection{ContainerService: runtimeInventory{app: &pb.AppContainer{AppName: tt.name, HttpPort: tt.port, RunningState: tt.state, AppVersion: "older-runtime"}}}
			ports, err := DesktopModelPorts(context.Background(), conn)
			if err != nil || (len(ports) == 1) != tt.want {
				t.Fatalf("ports %v, %v", ports, err)
			}
		})
	}
}
