package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	invite "github.com/wendylabsinc/wendy/go/proto/gen/cloudinvitepb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Every organization operation resolves a specific saved session. Merely selecting
// another organization never signs a request using the previous tenant's identity.
func desktopCloudAuth(account, organization string) (*config.AuthConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	selected, ok := cfg.ContextByName(account)
	if !ok {
		return nil, errors.New("This saved account is no longer available. Sign in again.")
	}
	if organization == "" {
		return selected, nil
	}
	if len(selected.Certificates) > 0 && selected.Certificates[0].TenantUUID() == organization {
		return selected, nil
	}

	return nil, errors.New("Sign in to this organization first, then refresh accounts.")
}
func DesktopOrganizations(ctx context.Context, account string) ([]*pb.Organization, error) {
	a, e := desktopCloudAuth(account, "")
	if e != nil {
		return nil, e
	}
	orgs, err := listCloudOrganizationsV2(ctx, a)
	if err == nil && len(a.Certificates) > 0 {
		for i, o := range orgs {
			if o.Id == a.Certificates[0].TenantUUID() {
				orgs[0], orgs[i] = orgs[i], orgs[0]
				break
			}
		}
	}
	return orgs, err
}

type DesktopCloudMember struct {
	Member                    *pb.OrganizationMember
	Name, Email, ProfileError string
}
type DesktopOrganization struct {
	Organization                                                                     *pb.Organization
	Members                                                                          []DesktopCloudMember
	Roles                                                                            []*pb.Role
	Devices                                                                          []*pb.Asset
	Access                                                                           []*pb.AccessEntry
	Invites                                                                          []*invite.OrganizationInvite
	Permissions                                                                      []string
	TeamError, RolesError, DevicesError, AccessError, InvitesError, PermissionsError string
}

func cloudError(e error) string {
	if e != nil {
		return e.Error()
	}
	return ""
}
func DesktopOrganizationOverview(ctx context.Context, account, org string) (*DesktopOrganization, error) {
	a, e := desktopCloudAuth(account, org)
	if e != nil {
		return nil, e
	}
	result := &DesktopOrganization{}
	e = withCloud(ctx, a, func(ctx context.Context, c grpc.ClientConnInterface) error {
		var err error
		result.Organization, err = pb.NewOrganizationServiceClient(c).GetOrganization(ctx, &pb.GetOrganizationRequest{Id: org})
		if err != nil {
			return err
		}
		// Independent reads retain their errors: lacking permission is never an empty team.
		permissions, err := pb.NewRoleServiceClient(c).ListEffectivePermissions(ctx, &pb.ListEffectivePermissionsRequest{OrganizationId: org})
		result.PermissionsError = cloudError(err)
		if err == nil {
			for _, p := range permissions.GetPermissions() {
				result.Permissions = append(result.Permissions, p.GetPermission())
			}
		}
		roles, err := pb.NewRoleServiceClient(c).ListRoles(ctx, &pb.ListRolesRequest{OrganizationId: org})
		result.RolesError = cloudError(err)
		if err == nil {
			result.Roles = roles.GetRoles()
		}
		members, err := pb.NewOrganizationServiceClient(c).ListMembers(ctx, &pb.ListMembersRequest{OrganizationId: org})
		result.TeamError = cloudError(err)
		if err == nil {
			for _, m := range members.GetMembers() {
				pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				u, err := pb.NewUserServiceClient(c).GetUser(pctx, &pb.GetUserRequest{Id: m.GetUserId()})
				cancel()
				result.Members = append(result.Members, DesktopCloudMember{Member: m, Name: u.GetName(), Email: u.GetEmail(), ProfileError: cloudError(err)})
			}
		}
		entries, err := pb.NewDeviceAccessServiceClient(c).ListAccessEntries(ctx, &pb.ListAccessEntriesRequest{OrganizationId: org})
		result.AccessError = cloudError(err)
		if err == nil {
			result.Access = entries.GetEntries()
		}
		stream, err := invite.NewOrganizationInviteServiceClient(c).ListOrganizationInvites(ctx, &invite.ListOrganizationInvitesRequest{OrganizationId: org})
		if err == nil {
			for {
				i, e := stream.Recv()
				if e == io.EOF {
					break
				}
				if e != nil {
					err = e
					break
				}
				result.Invites = append(result.Invites, i)
			}
		}
		result.InvitesError = cloudError(err)
		return nil
	})
	if e != nil {
		return nil, e
	}
	result.Devices, e = fetchCloudAssetsV2(ctx, a, false)
	result.DevicesError = cloudError(e)
	return result, nil
}
func desktopCloudWrite(ctx context.Context, account, org string, call func(context.Context, grpc.ClientConnInterface, *config.AuthConfig) error) error {
	a, e := desktopCloudAuth(account, org)
	if e != nil {
		return e
	}
	return withCloud(ctx, a, func(ctx context.Context, c grpc.ClientConnInterface) error { return call(ctx, c, a) })
}
func DesktopAddMember(ctx context.Context, account, org, email, role string) error {
	parsed, e := mail.ParseAddress(email)
	if e != nil || parsed.Address != email {
		return errors.New("Enter a valid email address.")
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		m := &pb.EmailWithRole{Email: email}
		if role != "" {
			m.RoleId = &role
		}
		out := &pb.AddMembersByEmailsResponse{}
		if e := cloudrequest.Invoke(ctx, c, a, pb.OrganizationService_AddMembersByEmails_FullMethodName, &pb.AddMembersByEmailsRequest{OrganizationId: org, Members: []*pb.EmailWithRole{m}}, out); e != nil {
			return e
		}
		if len(out.Results) != 1 || !out.Results[0].Success {
			if len(out.Results) > 0 {
				return errors.New(out.Results[0].Error)
			}
			return errors.New("Cloud did not confirm the new member.")
		}
		return nil
	})
}
func DesktopSetMemberRole(ctx context.Context, account, org, user, role string) error {
	if user == "" || role == "" {
		return errors.New("Choose a teammate and role.")
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		return cloudrequest.Invoke(ctx, c, a, pb.OrganizationService_UpdateMemberRole_FullMethodName, &pb.UpdateMemberRoleRequest{OrganizationId: org, UserId: user, RoleId: &role}, &pb.OrganizationMember{})
	})
}
func DesktopRemoveMember(ctx context.Context, account, org, user string) error {
	if user == "" {
		return errors.New("Choose a teammate.")
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		return cloudrequest.Invoke(ctx, c, a, pb.OrganizationService_RemoveMember_FullMethodName, &pb.RemoveMemberRequest{OrganizationId: org, UserId: user}, &pb.RemoveMemberResponse{})
	})
}
func DesktopCreateInvite(ctx context.Context, account, org, role string, expires int64) (string, error) {
	if role != "" && role != "admin" && role != "member" && role != "viewer" {
		return "", errors.New("Choose an invitation role.")
	}
	if expires <= time.Now().Unix() {
		return "", errors.New("Choose a future expiration date.")
	}
	link := ""
	e := desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		base, e := url.Parse(a.CloudDashboard)
		if e != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
			return errors.New("This account has no Cloud dashboard address. Sign in again.")
		}
		out := &invite.CreateOrganizationInviteResponse{}
		e = cloudrequest.Invoke(ctx, c, a, invite.OrganizationInviteService_CreateOrganizationInvite_FullMethodName, &invite.CreateOrganizationInviteRequest{OrganizationId: org, Role: role, ExpiresAt: timestamppb.New(time.Unix(expires, 0))}, out)
		if e != nil {
			return e
		}
		if out.Token == "" {
			return errors.New("Cloud returned no invitation token.")
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/invite/" + out.Token
		base.RawQuery = ""
		base.Fragment = ""
		link = base.String()
		return nil
	})
	return link, e
}
func DesktopRevokeInvite(ctx context.Context, account, org, id string) error {
	if id == "" {
		return errors.New("Choose an invitation.")
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		_, e := invite.NewOrganizationInviteServiceClient(c).RevokeOrganizationInvite(ctx, &invite.RevokeOrganizationInviteRequest{OrganizationId: org, Id: id})
		return e
	})
}
func DesktopCreateAccess(ctx context.Context, account, org, user, target string, effect pb.AccessEffect) error {
	if user == "" || (effect != pb.AccessEffect_ACCESS_EFFECT_ALLOW && effect != pb.AccessEffect_ACCESS_EFFECT_DENY) {
		return errors.New("Choose a teammate and access rule.")
	}
	kind := pb.AccessTargetType_ACCESS_TARGET_TYPE_DEVICE
	if target == "" {
		kind = pb.AccessTargetType_ACCESS_TARGET_TYPE_ALL
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		return cloudrequest.Invoke(ctx, c, a, pb.DeviceAccessService_CreateAccessEntry_FullMethodName, &pb.CreateAccessEntryRequest{OrganizationId: org, PrincipalType: pb.AccessPrincipalType_ACCESS_PRINCIPAL_TYPE_USER, PrincipalId: user, TargetType: kind, TargetRef: target, Effect: effect}, &pb.AccessEntry{})
	})
}
func DesktopDeleteAccess(ctx context.Context, account, org, id string) error {
	if id == "" {
		return fmt.Errorf("Choose an access rule.")
	}
	return desktopCloudWrite(ctx, account, org, func(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig) error {
		return cloudrequest.Invoke(ctx, c, a, pb.DeviceAccessService_DeleteAccessEntry_FullMethodName, &pb.DeleteAccessEntryRequest{OrganizationId: org, EntryId: id}, &pb.DeleteAccessEntryResponse{})
	})
}

// Cloud expands a device filter to its site, tags and organization-wide rules.
// Do not try to derive those relationships from display names in the app.
func DesktopDeviceAccess(ctx context.Context, account, org, device string) ([]*pb.AccessEntry, error) {
	if device == "" {
		return nil, errors.New("Choose a device.")
	}
	a, e := desktopCloudAuth(account, org)
	if e != nil {
		return nil, e
	}
	var entries []*pb.AccessEntry
	e = withCloud(ctx, a, func(ctx context.Context, c grpc.ClientConnInterface) error {
		result, e := pb.NewDeviceAccessServiceClient(c).ListAccessEntries(ctx, &pb.ListAccessEntriesRequest{OrganizationId: org, TargetType: pb.AccessTargetType_ACCESS_TARGET_TYPE_DEVICE, TargetRef: device})
		if e == nil {
			entries = result.GetEntries()
		}
		return e
	})
	return entries, e
}
