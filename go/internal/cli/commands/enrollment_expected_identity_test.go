package commands

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/resetcleanup"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type setupIdentityProbe struct {
	agentpb.UnimplementedWendyAgentServiceServer
	pb.UnimplementedWendyProvisioningServiceServer
	state string
}

func (*setupIdentityProbe) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "setup-test"}, nil
}

func (s *setupIdentityProbe) IsProvisioned(context.Context, *pb.IsProvisionedRequest) (*pb.IsProvisionedResponse, error) {
	switch s.state {
	case "unprovisioned":
		return &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &pb.NotProvisionedResponse{}}}, nil
	case "enrolled":
		return &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{}}}, nil
	case "reset-cleanup":
		// What an agent answers after a reset while it cannot finish removing
		// what the previous enrollment left on the host.
		return nil, resetcleanup.Refusal(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying", errors.New(setupProbeCleanupCause))
	case "probe-error":
		return nil, status.Error(codes.Unavailable, "reset recovery blocked; explicit local recovery required")
	default:
		return &pb.IsProvisionedResponse{}, nil
	}
}

const setupProbeCleanupCause = `interface "wendy0" is not owned by wendy-net`

func TestExplicitEnrollmentSetupIsSeparateFromManagement(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		explicit    bool
		want        bool
	}{
		{"explicit unprovisioned setup", "unprovisioned", true, true},
		{"ordinary management reaches an unprovisioned device", "unprovisioned", false, true},
		{"ordinary management refuses an enrolled plaintext answer", "enrolled", false, false},
		{"enrolled device is not a setup endpoint", "enrolled", true, false},
		{"unknown provisioning state is refused", "unknown", true, false},
		// A reset device that cannot finish its cleanup is unenrolled and
		// serves only the setup port. Refusing it there leaves no way to read
		// its logs, update its agent or reboot it, which is how it recovers.
		{"ordinary management reaches a device still cleaning up after a reset", "reset-cleanup", false, true},
		// It cannot be enrolled until the cleanup is done.
		{"enrollment setup waits for reset cleanup", "reset-cleanup", true, false},
		{"ordinary management refuses a status probe that fails for another reason", "probe-error", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}})
			setPinCache(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			service := &setupIdentityProbe{state: tc.state}
			agentpb.RegisterWendyAgentServiceServer(server, service)
			pb.RegisterWendyProvisioningServiceServer(server, service)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			target, err := resolveTargetInner(ctx, func(cfg *resolveConfig) {
				cfg.device = listener.Addr().String()
				cfg.enrollmentTrustRecovery = tc.explicit
				cfg.nonInteractive = true
				cfg.disableSessionBroker = true
				cfg.suppressUpdateCheck = true
			})
			if target != nil {
				defer target.Close()
			}
			if (err == nil) != tc.want {
				t.Fatalf("setup connection error=%v, want allowed=%v", err, tc.want)
			}
			if tc.state == "reset-cleanup" && err != nil && !strings.Contains(err.Error(), setupProbeCleanupCause) {
				t.Fatalf("refusal %q does not say what the cleanup is failing on", err)
			}
		})
	}
}

func TestExplicitSetupCannotOverrideSavedIdentity(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDevicePin("robot.local", 2, "cloud.test:443", "42", "")
	setTempConfig(t, cfg)
	setPinCache(t)
	target, handled, err := connectUnboundSetupTarget(context.Background(), "robot.local", "127.0.0.1:1")
	if target != nil || handled || err != nil {
		t.Fatalf("setup bypassed a saved device binding: target=%v handled=%v err=%v", target, handled, err)
	}
}

func TestEnrollmentBindingPreservesMatchingCloudAuthority(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDevicePinFrom("robot", 2, "authoritative-cloud.test:443", "42", "", config.PinSourceCloud)
	want, _ := cfg.DevicePinFor("robot")
	setTempConfig(t, cfg)
	identity := &certs.WendyIdentity{OrgID: 2, EntityType: certs.EntityAsset, EntityID: "42"}
	if err := saveEnrollmentBinding(&grpcclient.AgentConnection{Host: "robot"}, &config.AuthConfig{CloudGRPC: "other-cloud.test:443"}, identity, "robot"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := loaded.DevicePinFor("robot"); got != want {
		t.Fatalf("enrollment demoted or rewrote matching Cloud authority: got=%+v want=%+v", got, want)
	}
	identity.EntityID = "43"
	if err := saveEnrollmentBinding(&grpcclient.AgentConnection{Host: "robot"}, &config.AuthConfig{}, identity, "robot"); err == nil {
		t.Fatal("enrollment overwrote a different saved device identity")
	}
}
