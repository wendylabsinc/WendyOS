package services

import (
	"context"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestProvisioningReportsCertificateUUIDWithoutRoster(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/" + id
	svc := NewProvisioningServiceV2(&ProvisioningService{enrolled: true, principalURI: principal, cloudHost: "cloud.wendy.dev"})
	response, err := svc.IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.GetProvisioned().GetAssetUuid(); got != id {
		t.Fatalf("own certificate UUID depended on a Cloud roster refresh: got %q, want %q", got, id)
	}
	legacy := NewProvisioningServiceV2(&ProvisioningService{enrolled: true, orgID: 7, assetID: 42})
	response, err = legacy.IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.GetProvisioned(); got.GetAssetUuid() != "" || got.GetAssetId() != 42 {
		t.Fatalf("legacy identity invented a UUID: %+v", got)
	}
}
