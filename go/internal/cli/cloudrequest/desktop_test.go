package cloudrequest

import (
	invite "github.com/wendylabsinc/wendy/go/proto/gen/cloudinvitepb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestOrganizationMutationTargets(t *testing.T) {
	tests := []struct {
		method, path string
		request      proto.Message
	}{
		{pb.OrganizationService_AddMembersByEmails_FullMethodName, "members-by-emails", &pb.AddMembersByEmailsRequest{OrganizationId: testTenant, Members: []*pb.EmailWithRole{{Email: "person@example.com"}}}},
		{pb.OrganizationService_UpdateMemberRole_FullMethodName, "member/user-1", &pb.UpdateMemberRoleRequest{OrganizationId: testTenant, UserId: "user-1"}},
		{pb.OrganizationService_RemoveMember_FullMethodName, "member/user-1", &pb.RemoveMemberRequest{OrganizationId: testTenant, UserId: "user-1"}},
		{pb.DeviceAccessService_CreateAccessEntry_FullMethodName, "device-access", &pb.CreateAccessEntryRequest{OrganizationId: testTenant, PrincipalId: "user-1"}},
		{pb.DeviceAccessService_DeleteAccessEntry_FullMethodName, "device-access/entry-1", &pb.DeleteAccessEntryRequest{OrganizationId: testTenant, EntryId: "entry-1"}},
		{invite.OrganizationInviteService_CreateOrganizationInvite_FullMethodName, "invite", &invite.CreateOrganizationInviteRequest{OrganizationId: testTenant}},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			resolve := signedResources[tt.method]
			if path, ok := resolve(testTenant, tt.request); !ok || path != "org/"+testTenant+"/"+tt.path {
				t.Fatalf("target %q, valid %v", path, ok)
			}
			if _, ok := resolve("another-tenant", tt.request); ok {
				t.Fatal("signed across tenants")
			}
			if _, ok := resolve("", tt.request); ok {
				t.Fatal("signed with no tenant")
			}
			if _, ok := resolve(testTenant, &pb.GetAssetRequest{}); ok {
				t.Fatal("signed wrong payload type")
			}
		})
	}
}
