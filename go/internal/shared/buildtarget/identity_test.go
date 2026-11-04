package buildtarget

import (
	"bytes"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestLegacyAndUUIDResultIdentityRoundTrip(t *testing.T) {
	for _, target := range []*pb.PushTarget{{AssetId: 674}, {AssetUuid: "11111111-1111-4111-8111-111111111111"}} {
		result := Result(Key(target), true, "")
		if Key(result) != Key(target) || result.AssetUuid != target.AssetUuid || result.AssetId != target.AssetId {
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

func TestOriginalLegacyDeliveryResultWireContract(t *testing.T) {
	// The original response uses field 1 for the numeric asset and field 2 for success.
	wire := []byte{0x08, 0xa2, 0x05, 0x10, 0x01}
	result := new(pb.DeliveryResult)
	if err := proto.Unmarshal(wire, result); err != nil {
		t.Fatal(err)
	}
	if result.GetAssetId() != 674 || !result.GetDelivered() || result.GetAssetUuid() != "" {
		t.Fatalf("lost original result identity: %v", result)
	}
	encoded, err := proto.Marshal(Result(Key(result), true, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, wire) {
		t.Fatalf("delivery result requires new wire fields: %x", encoded)
	}
}
