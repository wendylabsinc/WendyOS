package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

func TestAuthModesA28PublicCLIRefusals(t *testing.T) {
	c := authComputerFor(t)
	c.refused(t, "", "--device-name", "install", "--close")
	c.refused(t, "", "--pre-enroll", "install", "--close", "--pre-enroll", "--device-name", "auth-modes-inert")
	c.refused(t, "", "cannot be combined", "install", "--close", "inert.img", "/dev/auth-modes-nonexistent")
}

type authModesWrongOwnerBoot struct {
	pb.UnimplementedWendyAccessServiceServer
}

func (authModesWrongOwnerBoot) GetAccess(context.Context, *pb.GetAccessRequest) (*pb.GetAccessResponse, error) {
	return &pb.GetAccessResponse{Shape: pb.AccessShape_ACCESS_SHAPE_CLOSED, CallerRole: pb.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER}, nil
}

func TestAuthModesA28FailedFirstBoot(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	useLocalKey(t)
	origDial, origUSB, origWait := dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait
	t.Cleanup(func() { dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait = origDial, origUSB, origWait })
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	closeBootWait = 100 * time.Millisecond
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, localVersionAgent{})
	pb.RegisterWendyAccessServiceServer(srv, authModesWrongOwnerBoot{})
	addr := serveTest(t, srv)
	for _, c := range []struct {
		name        string
		unreachable bool
		diagnostic  string
	}{{"wrong owner", false, "not its owner"}, {"unreachable", true, "was not reached"}} {
		t.Run(c.name, func(t *testing.T) {
			dialInstallDriverDevice = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
				if c.unreachable {
					return nil, errors.New("inert unreachable fixture")
				}
				return connectWithAutoTLS(ctx, addr)
			}
			err := verifyClosedAfterBoot(context.Background(), "auth-modes-inert")
			if err == nil || !strings.Contains(err.Error(), "OS written") || !strings.Contains(err.Error(), c.diagnostic) || !strings.Contains(err.Error(), "wendy device access close") {
				t.Fatalf("failed first boot must retain recovery diagnostic: %v", err)
			}
		})
	}
}

func TestAuthModesA28SeedWriteFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ownerKeysSeedFile), 0700); err != nil {
		t.Fatal(err)
	}
	err := writeConfigFiles(dir, configAgent{data: []byte("inert agent bytes"), ownerKeys: []byte("inert seed bytes")}, nil, "", nil)
	if err == nil {
		t.Fatal("failed seed write reported installation success")
	}
}

func TestAuthModesA28DesktopRefusalBeforeReadiness(t *testing.T) {
	oldLoad, oldToken := linuxDesktopConfigLoad, linuxDesktopTokenFn
	t.Cleanup(func() { linuxDesktopConfigLoad, linuxDesktopTokenFn = oldLoad, oldToken })
	loaded, minted := false, false
	linuxDesktopConfigLoad = func() (*config.Config, error) { loaded = true; return &config.Config{}, nil }
	linuxDesktopTokenFn = func(context.Context, *config.AuthConfig, string, int32) (string, time.Time, error) {
		minted = true
		return "", time.Time{}, errors.New("unexpected token request")
	}
	for _, route := range []string{linuxDesktopMachineLabel, headlessMacMachineLabel} {
		t.Run(route, func(t *testing.T) {
			out := captureStdout(t, func() {
				err := installDesktop(context.Background(), preEnrollOptions{ownerKeys: []byte("inert close seed")}, "auth-modes-inert", route)
				if err == nil || !strings.Contains(err.Error(), "wendy device access close") {
					t.Fatalf("desktop --close route did not refuse: %v", err)
				}
			})
			if loaded || minted || out != "" {
				t.Fatalf("unsupported desktop close reached readiness/config/token/instructions: loaded=%t minted=%t out=%q", loaded, minted, out)
			}
		})
	}
}
