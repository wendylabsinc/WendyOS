package commands

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	legacyagentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestLegacyBuildDeliveryRetainsAuthenticatedNumericIdentity(t *testing.T) {
	actual := certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "674"}
	conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "target", "target:50052", nil, actual)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reported := &pb.ProvisionedResponse{OrganizationId: 64, AssetId: 674}
	if err := verifiedLegacyBuildDelivery(conn, nil, reported); err != nil {
		t.Fatal(err)
	}
	for _, other := range []*pb.ProvisionedResponse{{OrganizationId: 65, AssetId: 674}, {OrganizationId: 64, AssetId: 675}, {OrganizationId: 64, AssetId: 674, AssetUuid: "11111111-1111-4111-8111-111111111111"}} {
		if err := verifiedLegacyBuildDelivery(conn, nil, other); err == nil {
			t.Fatalf("accepted %v", other)
		}
	}
	conn.CertInfo = &config.CertificateInfo{OrganizationID: 65, UserID: "operator"}
	if err := verifiedLegacyBuildDelivery(conn, nil, reported); err == nil {
		t.Fatal("cross-org operator accepted")
	}
}
func TestLegacyServiceTargetRetainsOriginalNumericField(t *testing.T) {
	target := &pb.PushTarget{AssetId: 674, AgentPort: 50052, RegistryPort: 5000}
	spec := serviceBuildSpec(target, "app-web:latest", "linux/arm64", "Dockerfile", nil, "auto", nil)
	if spec.PushTarget.AssetId != 674 || spec.PushTarget.AssetUuid != "" {
		t.Fatalf("service target lost identity: %v", spec.PushTarget)
	}
}

func TestLegacyBuildHostOrganizationUsesAuthenticatedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		identity  certs.WendyIdentity
		wantError bool
	}{
		{name: "same organization", identity: certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "675"}},
		{name: "foreign organization", identity: certs.WendyIdentity{OrgID: 65, EntityType: certs.EntityAsset, EntityID: "675"}, wantError: true},
		{name: "operator is not a build host", identity: certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityUser, EntityID: "operator"}, wantError: true},
		{name: "PKI identity", identity: certs.WendyIdentity{Principal: deploymentV2Device().GetPrincipalUri()}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "builder", "builder:50052", nil, tc.identity)
			if err != nil {
				t.Fatal(err)
			}
			defer builder.Close()
			if err := verifiedLegacyBuildHostOrganization(builder, 64); (err != nil) != tc.wantError {
				t.Fatalf("organization check: %v wantError=%v", err, tc.wantError)
			}
		})
	}
	if err := verifiedLegacyBuildHostOrganization(nil, 64); err == nil {
		t.Fatal("unverified host accepted")
	}
}

func TestLegacyBuildTargetNeedsOnlyOriginalNumericIdentity(t *testing.T) {
	server := &deploymentV2Server{device: &pb.ProvisionedResponse{OrganizationId: 64, AssetId: 674}}
	target := deploymentTLSConnection(t, server, nil, "urn:wendy:org:64:asset:674")
	target.Addr = "127.0.0.1:50052"
	target.AgentService = &fakeAgentVersionClient{resp: &legacyagentpb.GetAgentVersionResponse{Os: "linux"}}
	for _, org := range []int32{64, 65} {
		builder, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "builder", "builder:50052", nil, certs.WendyIdentity{OrgID: org, EntityType: certs.EntityAsset, EntityID: "675"})
		if err != nil {
			t.Fatal(err)
		}
		defer builder.Close()
		wireTarget, err := targetPushTargetBase(context.Background(), target, builder)
		if org == 65 {
			if err == nil {
				t.Fatal("foreign build host accepted")
			}
		} else if err != nil || wireTarget.GetAssetId() != 674 || wireTarget.GetAssetUuid() != "" {
			t.Fatalf("original numeric target refused or changed: %+v %v", wireTarget, err)
		}
	}
}
