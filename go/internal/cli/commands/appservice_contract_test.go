package commands

import (
	"bytes"
	"encoding/hex"
	"testing"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/protobuf/proto"
)

// These wire fixtures are shared with Cloud and both Companion SDKs.
func TestAppServiceWireContract(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		message    proto.Message
	}{
		{"update", "0a016110071a014e2201442801", &cloudpb.UpdateAppRequest{Id: "a", OrganizationId: 7, Name: proto.String("N"), Details: proto.String("D"), CanSendNotifications: proto.Bool(true)}},
		{"list", "08071014180a220178", &cloudpb.ListAppsRequest{OrganizationId: 7, Offset: proto.Int32(20), Limit: proto.Int32(10), Filter: proto.String("x")}},
		{"total", "101e", &cloudpb.ListAppsResponse{Total: 30}},
		{"upsert", "0a016110071a014e220144", &cloudpb.UpsertAppRequest{Id: "a", OrganizationId: 7, Name: proto.String("N"), Details: proto.String("D")}},
		{"get", "0a01611007", &cloudpb.GetAppRequest{Id: "a", OrganizationId: 7}},
		{"delete", "0a01611007", &cloudpb.DeleteAppRequest{Id: "a", OrganizationId: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := hex.DecodeString(tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			decoded := tc.message.ProtoReflect().New().Interface()
			if err := proto.Unmarshal(wire, decoded); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(decoded, tc.message) {
				t.Fatalf("decoded %v, want %v", decoded, tc.message)
			}
			got, err := proto.Marshal(tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wire) {
				t.Fatalf("wire %x, want %x", got, wire)
			}
		})
	}
}
