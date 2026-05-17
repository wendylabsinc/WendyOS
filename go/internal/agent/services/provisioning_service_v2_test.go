package services

import (
	"context"
	"math"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func TestProvisioningServiceV2_StartProvisioning_RejectsOutOfRangeIDs(t *testing.T) {
	tests := []struct {
		name  string
		orgID int64
		asset int64
	}{
		{"org id above int32 max", math.MaxInt32 + 1, 10},
		{"org id below int32 min", math.MinInt32 - 1, 10},
		{"asset id above int32 max", 10, math.MaxInt32 + 1},
		{"asset id below int32 min", 10, math.MinInt32 - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v1, tmpDir := newTestProvisioningService(t)
			defer os.RemoveAll(tmpDir)
			svc := NewProvisioningServiceV2(v1)

			_, err := svc.StartProvisioning(context.Background(), &agentpbv2.StartProvisioningRequest{
				OrganizationId: tt.orgID,
				CloudHost:      "test.wendy.io",
				AssetId:        tt.asset,
			})
			if err == nil {
				t.Fatal("expected error for out-of-range ID, got nil")
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("error code = %v; want InvalidArgument", status.Code(err))
			}

			// The truncating cast must never have reached the v1 path: the
			// agent must still be unprovisioned.
			resp, err := v1.IsProvisioned(context.Background(), nil)
			if err != nil {
				t.Fatalf("IsProvisioned: %v", err)
			}
			if resp.GetNotProvisioned() == nil {
				t.Fatal("agent was provisioned despite out-of-range ID being rejected")
			}
		})
	}
}

func TestProvisioningServiceV2_StartProvisioning_AcceptsInRangeIDs(t *testing.T) {
	v1, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)
	svc := NewProvisioningServiceV2(v1)

	_, err := svc.StartProvisioning(context.Background(), &agentpbv2.StartProvisioningRequest{
		OrganizationId: 42,
		CloudHost:      "test.wendy.io",
		AssetId:        100,
	})
	if err != nil {
		t.Fatalf("StartProvisioning with in-range IDs: %v", err)
	}
}
