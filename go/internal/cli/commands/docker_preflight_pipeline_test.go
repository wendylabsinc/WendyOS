package commands

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type preflightAgentServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	versions atomic.Int32
}

func (s *preflightAgentServer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	s.versions.Add(1)
	return &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}, nil
}

func TestDockerPreflightStopsRunBeforeBuild(t *testing.T) {
	for _, export := range []string{"dir", "tar"} {
		for _, chunking := range []string{chunkingAuto, chunkingForce} {
			for _, failure := range []string{"stopped", "hung"} {
				t.Run(fmt.Sprintf("%s-%s-%s", export, chunking, failure), func(t *testing.T) {
					stubDockerDaemonSeams(t, time.Second)
					stubNonInteractive(t)
					stubHumanPresent(t, false)
					fakeAllBuildDisks(t, 40<<30)
					captureDeployEvents(t)
					t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
					t.Setenv("WENDY_CHUNK_EXPORT", export)
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
						t.Fatal(err)
					}
					runtimePath := filepath.Join(t.TempDir(), "Docker.app")
					if err := os.Mkdir(runtimePath, 0700); err != nil {
						t.Fatal(err)
					}
					darwinDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: runtimePath}}
					windowsDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: runtimePath}}
					dockerLookPathFn = func(string) (string, error) { return os.Args[0], nil }
					dockerOpenRuntimeFn = func(context.Context, string) error { t.Error("preflight opened a runtime"); return nil }
					dockerInstallRuntimeFn = func(context.Context) error { t.Error("preflight installed a runtime"); return nil }
					probeLog := filepath.Join(t.TempDir(), "probe.log")
					var probes atomic.Int32
					dockerVersionOKFn = func(ctx context.Context) bool {
						probes.Add(1)
						// Retain actual subprocess exit/deadline behavior without
						// requiring or contacting the host's Docker daemon.
						command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDockerPreflightProbeProcess$")
						command.Env = append(os.Environ(), "WENDY_PREFLIGHT_FIXTURE="+failure, "WENDY_PREFLIGHT_LOG="+probeLog, "GORACE=atexit_sleep_ms=0")
						return command.Run() == nil
					}
					oldCommand, oldProgress := imageBuilderCommandContext, buildProgressInteractive
					t.Cleanup(func() { imageBuilderCommandContext, buildProgressInteractive = oldCommand, oldProgress })
					var buildCommands, progressCalls atomic.Int32
					imageBuilderCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
						buildCommands.Add(1)
						command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDockerPreflightProbeProcess$")
						command.Env = append(os.Environ(), "WENDY_PREFLIGHT_FIXTURE=unexpected-build")
						return command
					}
					buildProgressInteractive = func() bool { progressCalls.Add(1); return false }
					var progress bytes.Buffer
					t.Cleanup(setBuildProgressOut(&progress))

					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					var unexpectedRPCs atomic.Int32
					server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
						if info.FullMethod != agentpb.WendyAgentService_GetAgentVersion_FullMethodName {
							unexpectedRPCs.Add(1)
							return nil, status.Error(codes.FailedPrecondition, "deployment must not start before Docker preflight")
						}
						return handler(ctx, req)
					}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
						unexpectedRPCs.Add(1)
						return status.Error(codes.FailedPrecondition, "deployment must not start before Docker preflight")
					}))
					agent := &preflightAgentServer{}
					agentpb.RegisterWendyAgentServiceServer(server, agent)
					go server.Serve(listener)
					t.Cleanup(server.Stop)
					client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { client.Close() })
					connection := grpcclient.NewFromConn(client)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					cfg := &appconfig.AppConfig{AppID: "preflight-pipeline", Platform: "linux", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
					err = runWithAgent(ctx, connection, dir, cfg, runOptions{builder: imageBuilderDocker, chunking: chunking, detach: true, skipCloudRegistration: true})
					want := "docker daemon is not running"
					if failure == "hung" {
						want = "docker daemon is not responding"
					}
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("preflight failure was hidden or bypassed: %v", err)
					}
					if probes.Load() != 1 || agent.versions.Load() != 1 {
						t.Fatalf("wrong pipeline boundary: daemon probes=%d, agent versions=%d", probes.Load(), agent.versions.Load())
					}
					if got, err := os.ReadFile(probeLog); err != nil || string(got) != failure {
						t.Fatalf("daemon probe subprocess did not execute: %q %v", got, err)
					}
					if progressCalls.Load() != 0 || progress.Len() != 0 || buildCommands.Load() != 0 || unexpectedRPCs.Load() != 0 {
						t.Fatalf("failed preflight entered progress/build/deployment: progress=%d bytes=%q commands=%d RPCs=%d", progressCalls.Load(), progress.String(), buildCommands.Load(), unexpectedRPCs.Load())
					}
				})
			}
		}
	}
}

func TestDockerPreflightProbeProcess(t *testing.T) {
	mode := os.Getenv("WENDY_PREFLIGHT_FIXTURE")
	if mode == "" {
		return
	}
	if mode == "unexpected-build" {
		os.Exit(91)
	}
	if err := os.WriteFile(os.Getenv("WENDY_PREFLIGHT_LOG"), []byte(mode), 0600); err != nil {
		os.Exit(92)
	}
	if mode == "hung" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(1)
}
