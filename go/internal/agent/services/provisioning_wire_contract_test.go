package services

import (
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestProvisioningResetAndMeshWireFieldsStayDistinct(t *testing.T) {
	fields := (&agentpbv2.ProvisionedResponse{}).ProtoReflect().Descriptor().Fields()
	for _, want := range []struct {
		name   protoreflect.Name
		number protoreflect.FieldNumber
		kind   protoreflect.Kind
	}{
		{"cloud_unenrollment_supported", 5, protoreflect.BoolKind},
		{"cloud_authenticated_reset_supported", 6, protoreflect.BoolKind},
		{"asset_uuid", 7, protoreflect.StringKind},
	} {
		field := fields.ByName(want.name)
		if field == nil || field.Number() != want.number || field.Kind() != want.kind {
			t.Fatalf("%s must retain field %d with kind %s", want.name, want.number, want.kind)
		}
	}

	want := &agentpbv2.ProvisionedResponse{
		CloudAuthenticatedResetSupported: true,
		AssetUuid:                        "11111111-2222-4333-8444-555555555555",
	}
	wire, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got agentpbv2.ProvisionedResponse
	if err := proto.Unmarshal(wire, &got); err != nil || !proto.Equal(want, &got) {
		t.Fatalf("reset capability and mesh identity did not survive together: %v, %v", &got, err)
	}
}

func TestDraftMeshAssetFieldCannotAuthorizeCloudReset(t *testing.T) {
	// The early mesh draft used field 6 for a string. Main owns field 6 for
	// the authenticated-reset capability. Old mesh data must never advertise
	// reset support merely because its roster supplied an asset UUID.
	wire := protowire.AppendTag(nil, 6, protowire.BytesType)
	wire = protowire.AppendString(wire, "11111111-2222-4333-8444-555555555555")
	var got agentpbv2.ProvisionedResponse
	err := proto.Unmarshal(wire, &got)
	if err == nil && (got.GetCloudAuthenticatedResetSupported() || got.GetAssetUuid() != "") {
		t.Fatalf("draft field was interpreted as current reset/asset evidence: %v", &got)
	}
}
