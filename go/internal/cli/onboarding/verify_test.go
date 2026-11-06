package onboarding

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type bootAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyProvisioningServiceServer
	enrollmentCalls atomic.Int32
	version         *agentpb.GetAgentVersionResponse
	provisioning    *agentpb.IsProvisionedResponse
}

func (s *bootAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return s.version, nil
}
func (s *bootAgent) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	s.enrollmentCalls.Add(1)
	return s.provisioning, nil
}

// serveBootAgent serves fake over an in-memory listener and returns a
// connector that fails the test if Verify dials anything but want.
func serveBootAgent(t *testing.T, fake *bootAgent, want string) func(context.Context, string) (*grpcclient.AgentConnection, error) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, fake)
	agentpb.RegisterWendyProvisioningServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return func(ctx context.Context, address string) (*grpcclient.AgentConnection, error) {
		if address != want {
			t.Fatalf("verified wrong target %s", address)
		}
		conn, err := grpc.NewClient("passthrough:///boot-test", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return grpcclient.NewFromConn(conn), nil
	}
}

func TestVerifyFirstBootIdentityAndEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		opts                      VerifyOptions
		agentOnly, enrolled, want bool
		enrollment                string
	}{
		{"matching installation", VerifyOptions{OSVersion: "release", DeviceType: "raspberry-pi-5", PublicKey: "key"}, false, true, true, "enrolled"},
		{"wrong OS", VerifyOptions{OSVersion: "different"}, false, true, false, "unknown"},
		{"wrong board", VerifyOptions{DeviceType: "jetson-orin-nano"}, false, true, false, "unknown"},
		{"wrong identity", VerifyOptions{PublicKey: "different-key"}, false, true, false, "unknown"},
		{"enrollment required", VerifyOptions{RequireEnrollment: true}, false, false, false, "not_enrolled"},
		{"enrollment optional", VerifyOptions{}, false, false, true, "not_enrolled"},
		{"agent-only accepted", VerifyOptions{}, true, false, true, "not_enrolled"},
		{"agent-only is not WendyOS", VerifyOptions{OSVersion: "release"}, true, false, false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &bootAgent{version: &agentpb.GetAgentVersionResponse{Version: "agent", OsVersion: proto.String("release"), DeviceType: proto.String("raspberry-pi-5"), PublicKey: proto.String("key")}}
			if tc.agentOnly {
				fake.version.OsVersion = nil
				fake.version.DeviceType = nil
			}
			fake.provisioning = &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_NotProvisioned{}}
			if tc.enrolled {
				fake.provisioning.Response = &agentpb.IsProvisionedResponse_Provisioned{}
			}
			tc.opts.Address = "expected.local:50051"
			tc.opts.Timeout = time.Second
			v, err := Verify(context.Background(), tc.opts, serveBootAgent(t, fake, "expected.local:50051"))
			if err != nil {
				t.Fatal(err)
			}
			if v.Verified != tc.want || !v.Reachable || v.Enrollment != tc.enrollment || v.Application != "not_checked" {
				t.Fatalf("unexpected verification: %+v", v)
			}
			if tc.enrollment == "unknown" && fake.enrollmentCalls.Load() != 0 {
				t.Fatal("queried enrollment after identity mismatch")
			}
		})
	}
}

// WDY-3974: the installer prints --expected-os-version "0.19.5" while the agent
// reports os_version "WendyOS-0.19.5"; both name the same release.
func TestVerifyOSVersionIgnoresWendyOSPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, want, reported string
		verified             bool
	}{
		{"installer version", "0.19.5", "WendyOS-0.19.5", true},
		{"reported form", "WendyOS-0.19.5", "WendyOS-0.19.5", true},
		{"prefix case", "wendyos-0.20.0", "WendyOS-0.20.0", true},
		{"unprefixed report", "WendyOS-0.19.5", "0.19.5", true},
		{"different release", "0.19.4", "WendyOS-0.19.5", false},
		{"release prefix only", "0.19", "WendyOS-0.19.5", false},
		{"suffixed build", "0.19.5", "WendyOS-0.19.5-dev", false},
		{"other prefix", "0.19.5", "Ubuntu-0.19.5", false},
		{"bare prefix on agent-only", "WendyOS-", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &bootAgent{
				version:      &agentpb.GetAgentVersionResponse{Version: "agent", OsVersion: proto.String(tc.reported)},
				provisioning: &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_NotProvisioned{}},
			}
			if tc.reported == "" {
				fake.version.OsVersion = nil
			}
			opts := VerifyOptions{Address: "expected.local", OSVersion: tc.want, Timeout: time.Second}
			v, err := Verify(context.Background(), opts, serveBootAgent(t, fake, "expected.local:50051"))
			if err != nil {
				t.Fatal(err)
			}
			if v.Verified != tc.verified || v.OSVersion != tc.reported {
				t.Fatalf("expected %q, reported %q: %+v", tc.want, tc.reported, v)
			}
		})
	}
}

func TestVerifyNeverFallsBackToDefault(t *testing.T) {
	called := false
	connect := func(context.Context, string) (*grpcclient.AgentConnection, error) {
		called = true
		return nil, errors.New("offline")
	}
	if _, err := Verify(context.Background(), VerifyOptions{Timeout: time.Second}, connect); err == nil || called {
		t.Fatal("missing address must fail before connecting")
	}
	v, err := Verify(context.Background(), VerifyOptions{Address: "expected.local", Timeout: time.Second}, connect)
	if err != nil || v.Verified || v.Reachable || len(v.Problems) != 1 {
		t.Fatalf("offline device reported success: %+v %v", v, err)
	}
}

func TestVerificationAddress(t *testing.T) {
	for input, want := range map[string]string{
		"robot.local": "robot.local:50051", "192.0.2.1": "192.0.2.1:50051", "::1": "[::1]:50051", "robot:50052": "robot:50052", "cloud://host/org/1/asset/2": "cloud://host/org/1/asset/2",
		// WDY-3974: a USB device as wendy discover lists it, bare or bracketed.
		"fe80::b0be:2b85:9f8:a22%en31": "[fe80::b0be:2b85:9f8:a22%en31]:50051",
		"[fe80::1%en31]:50052":         "[fe80::1%en31]:50052",
		"[fe80::1%en31]":               "[fe80::1%en31]:50051",
		"[::1]":                        "[::1]:50051",
	} {
		got, err := verificationAddress(input)
		if err != nil || got != want {
			t.Fatalf("%q => %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", " ", ":50051", "robot:bad", "robot:0", "robot/other",
		"fe80::1%", "fe80::1%en31:50051", "fe80::1%en31]", "[fe80::1%en31:1]:50051", "[robot]", "[192.0.2.1]"} {
		if _, err := verificationAddress(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
