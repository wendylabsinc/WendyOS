package buildtarget

import (
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestLegacyAndUUIDResultIdentityRoundTrip(t *testing.T) {
	for _, target := range []*pb.PushTarget{{AssetId: 674, LegacyOrganizationId: 64}, {AssetId: 674}, {AssetUuid: "11111111-1111-4111-8111-111111111111"}} {
		result := Result(Key(target), true, "")
		if Key(result) != Key(target) || result.AssetUuid != target.AssetUuid || result.AssetId != target.AssetId || result.LegacyOrganizationId != target.LegacyOrganizationId {
			t.Fatalf("identity changed: %v -> %v", target, result)
		}
	}
}
func TestOriginalLegacyBuildTargetWireNumberStillWorks(t *testing.T) {
	target := new(pb.PushTarget)
	// The pre-v2 contract encoded asset_id=674 as varint field 1.
	if err := proto.Unmarshal([]byte{0x08, 0xa2, 0x05}, target); err != nil {
		t.Fatal(err)
	}
	if target.AssetId != 674 || target.AssetUuid != "" {
		t.Fatalf("legacy wire identity lost: %v", target)
	}
}
