package commands

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Cloud reserves the name before the device proves it can reach PKI. When the
// device then fails, a retry under the same name is refused as taken, so the
// error has to name the command that releases the reservation.
func TestOIDCEnrollmentFailureAfterReservationNamesCleanup(t *testing.T) {
	const asset = "7b0e6c0a-3c55-4f0e-9d7e-0f2d5a1c9b11"
	for _, tc := range []struct {
		name     string
		agentErr error
	}{
		{"device cannot reach PKI", status.Error(codes.Internal, "enrolling with PKI: dial udp 9.9.9.9:53: connect: network is unreachable")},
		{"agent without direct PKI enrollment", status.Error(codes.Unimplemented, "missing")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: asset, CredentialKind: "eab", EabKeyId: "id", EabHmacKey: "abcd"}}
			agent := &acmeProvisioningServer{startErr: tc.agentErr}
			conn, host := enrollmentServers(t, cloud, agent)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := runEnrollDevice(ctx, conn, auth, "sim", 0)
			if err == nil || cloud.req == nil || agent.req == nil {
				t.Fatalf("expected a device-side failure after the reservation, got %v", err)
			}
			want := []string{"Remove the reserved asset before enrolling again: wendy cloud device unenroll --device cloud://" + host + "/tenant/" + testOperatorTenant + "/asset/" + asset}
			if got := NextSteps(err); !reflect.DeepEqual(got, want) {
				t.Fatalf("next steps = %q, want %q", got, want)
			}
		})
	}
}

// offlineDeviceAgent is what a board tethered over USB alone reports: no
// internet access, and no interface with a routable address.
type offlineDeviceAgent struct {
	agentpbv2.UnimplementedWendyBuildServiceServer
	agentpb.UnimplementedWendyAgentServiceServer
}

func (offlineDeviceAgent) GetBuildCapabilities(context.Context, *agentpbv2.GetBuildCapabilitiesRequest) (*agentpbv2.GetBuildCapabilitiesResponse, error) {
	return &agentpbv2.GetBuildCapabilitiesResponse{InternetCheckSupported: true, InternetDetail: "cannot resolve registry-1.docker.io"}, nil
}

func (offlineDeviceAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "2026.11.15-074641"}, nil
}

// A device tethered over USB alone cannot reach PKI. It is refused before
// Cloud reserves its name, so nothing is left behind to clean up.
func TestOIDCEnrollmentOfflineDeviceDoesNotMint(t *testing.T) {
	cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{CredentialKind: "eab", EabKeyId: "id", EabHmacKey: "abcd"}}
	agent := &acmeProvisioningServer{}
	conn, host := enrollmentServers(t, cloud, agent, func(srv *grpc.Server) {
		agentpbv2.RegisterWendyBuildServiceServer(srv, offlineDeviceAgent{})
		agentpb.RegisterWendyAgentServiceServer(srv, offlineDeviceAgent{})
	})
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// An address literal keeps the PKI lookup off the network.
	err := runEnrollDevice(ctx, conn, auth, "sim", 0, "https://203.0.113.10/"+testOperatorTenant+"/acme/directory")
	if err == nil || !strings.Contains(err.Error(), "no route to the internet") {
		t.Fatalf("error = %v, want the offline refusal", err)
	}
	if cloud.req != nil || agent.req != nil {
		t.Fatal("an offline device caused a reservation or credential handoff")
	}
}
