package commands

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"path/filepath"
	"testing"
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
func TestLegacyBuildCapabilityAndServiceTarget(t *testing.T) {
	target := &pb.PushTarget{AssetId: 674, LegacyOrganizationId: 64, AgentPort: 50052, RegistryPort: 5000}
	caps := &pb.GetBuildCapabilitiesResponse{}
	if err := checkLegacyDeliverySupported("builder", caps, []*pb.PushTarget{target}); err == nil {
		t.Fatal("old builder silently accepted numeric target")
	}
	caps.LegacyDelivery = true
	if err := checkLegacyDeliverySupported("builder", caps, []*pb.PushTarget{target}); err != nil {
		t.Fatal(err)
	}
	spec := serviceBuildSpec(target, "app-web:latest", "linux/arm64", "Dockerfile", nil, "auto", nil)
	if spec.PushTarget.AssetId != 674 || spec.PushTarget.LegacyOrganizationId != 64 || spec.PushTarget.AssetUuid != "" {
		t.Fatalf("service target lost identity: %v", spec.PushTarget)
	}
}
