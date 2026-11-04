package proto

import (
	"bytes"
	"encoding/hex"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// Captured with the committed Go messages at 52dd3be8dcbe3f6e96fa8a9b8b1d362d6c1ddedb,
// before Organizations and Notifications moved to Shared gRPC API contracts.
// Keep these bytes fixed: signed request bodies must retain their wire shape,
// and old replies must leave the new device defaults/count unavailable.
func TestOrganizationsAndNotificationsOldWire(t *testing.T) {
	for _, tt := range []struct {
		name string
		wire string
		want proto.Message
	}{
		{"get-organization", "0a036f7267", &pb.GetOrganizationRequest{Id: "org"}},
		{"list-organizations", "080010641a036c6162", &pb.ListOrganizationsRequest{Offset: proto.Int32(0), Limit: proto.Int32(100), Query: proto.String("lab")}},
		{"member-default-role", "0a036f726712067669657765722001", &pb.SetMemberDefaultsRequest{OrganizationId: "org", DefaultRoleId: proto.String("viewer"), SkipDefaultWhenGroupMapped: true}},
		{"member-default-clear", "0a036f72671801", &pb.SetMemberDefaultsRequest{OrganizationId: "org", ClearDefault: true}},
		{"member-default-empty-role", "0a036f72671200", &pb.SetMemberDefaultsRequest{OrganizationId: "org", DefaultRoleId: proto.String("")}},
		{"organization", "0a036f726712034c61624a067669657765725001", &pb.Organization{Id: "org", Name: "Lab", DefaultRoleId: "viewer", SkipDefaultWhenGroupMapped: true}},
		{"member", "0a066d656d62657212036f72671a047573657220053a0676696577657242077375626a6563744a0764656661756c74", &pb.OrganizationMember{Id: "member", OrganizationId: "org", UserId: "user", Role: pb.OrganizationRole_ORGANIZATION_ROLE_VIEWER, RoleId: "viewer", Subject: "subject", RoleSource: "default"}},
		{"create-notification", "120a0a04757365721a0202051a05416c6572742203486f7428023a2431323334353637382d313233342d343233342d383233342d3132333435363738396162634a03617070", &pb.CreateNotificationV2Request{
			Audience: &pb.NotificationAudience{UserIds: []string{"user"}, Roles: []pb.OrganizationRole{pb.OrganizationRole_ORGANIZATION_ROLE_ADMIN, pb.OrganizationRole_ORGANIZATION_ROLE_VIEWER}},
			Title:    "Alert", Body: "Hot", Severity: pb.NotificationSeverity_NOTIFICATION_SEVERITY_WARNING,
			NotificationId: "12345678-1234-4234-8234-123456789abc", AppId: proto.String("app"),
		}},
		{"list-notifications", "0a036f72671a2431323334353637382d313233342d343233342d383233342d313233343536373839616263203228023002", &pb.ListNotificationsV2Request{
			OrganizationId: "org", AfterNotificationId: proto.String("12345678-1234-4234-8234-123456789abc"), Limit: 50,
			SeverityFilter: pb.NotificationSeverity_NOTIFICATION_SEVERITY_WARNING.Enum(), ReadStatus: pb.NotificationReadStatus_NOTIFICATION_READ_STATUS_UNREAD.Enum(),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := hex.DecodeString(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			got := tt.want.ProtoReflect().Type().New().Interface()
			if err := proto.Unmarshal(wire, got); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Fatalf("decoded old wire = %v, want %v", got, tt.want)
			}
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, wire) {
				t.Fatalf("current wire = %x, want old wire %x", encoded, wire)
			}
		})
	}
}

func TestOrganizationDeviceAccessCountPresence(t *testing.T) {
	// Field 10 explicitly set to zero means no devices. An old server's absent
	// count means unavailable, so GetDeviceAccessCount alone cannot distinguish it.
	var member pb.OrganizationMember
	if err := proto.Unmarshal([]byte{0x50, 0x00}, &member); err != nil {
		t.Fatal(err)
	}
	if member.DeviceAccessCount == nil || *member.DeviceAccessCount != 0 {
		t.Fatalf("explicit zero count lost: %v", &member)
	}
}
