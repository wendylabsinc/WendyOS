package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"time"
	"unsafe"
)

func cloudContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}
func cloudStrings(items []string) (**C.char, C.size_t) {
	p := alloc[*C.char](len(items))
	for i, s := range items {
		unsafe.Slice(p, len(items))[i] = C.CString(s)
	}
	return p, C.size_t(len(items))
}
func freeCloudStrings(p **C.char, n C.size_t) {
	for _, s := range unsafe.Slice(p, int(n)) {
		free(s)
	}
	free(p)
}

//export WDOrganizations
func WDOrganizations(account *C.char) *C.WDOrganizationsResult {
	ctx, cancel := cloudContext()
	defer cancel()
	items, e := commands.DesktopOrganizations(ctx, C.GoString(account))
	p := alloc[C.WDOrganizationsResult](1)
	p.status = status(e)
	p.items, p.count = alloc[C.WDOrganization](len(items)), C.size_t(len(items))
	for i, o := range items {
		unsafe.Slice(p.items, len(items))[i] = C.WDOrganization{id: C.CString(o.Id), name: C.CString(o.Name)}
	}
	return p
}

//export WDOrganizationsFree
func WDOrganizationsFree(p *C.WDOrganizationsResult) {
	if p == nil {
		return
	}
	for _, o := range unsafe.Slice(p.items, int(p.count)) {
		free(o.id)
		free(o.name)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDOrganizationOverview
func WDOrganizationOverview(account, org *C.char) *C.WDOrganizationResult {
	ctx, cancel := cloudContext()
	defer cancel()
	result, e := commands.DesktopOrganizationOverview(ctx, C.GoString(account), C.GoString(org))
	return organizationResult(result, e)
}
func organizationResult(r *commands.DesktopOrganization, e error) *C.WDOrganizationResult {
	p := alloc[C.WDOrganizationResult](1)
	p.status = status(e)
	if e != nil || r == nil {
		return p
	}
	p.name = C.CString(r.Organization.GetName())
	p.team_error = C.CString(r.TeamError)
	p.roles_error = C.CString(r.RolesError)
	p.devices_error = C.CString(r.DevicesError)
	p.access_error = C.CString(r.AccessError)
	p.invites_error = C.CString(r.InvitesError)
	p.permissions_error = C.CString(r.PermissionsError)
	p.permissions, p.permission_count = cloudStrings(r.Permissions)
	p.members, p.member_count = alloc[C.WDCloudMember](len(r.Members)), C.size_t(len(r.Members))
	for i, m := range r.Members {
		unsafe.Slice(p.members, len(r.Members))[i] = C.WDCloudMember{id: C.CString(m.Member.GetUserId()), name: C.CString(m.Name), email: C.CString(m.Email), role_id: C.CString(m.Member.GetRoleId()), profile_error: C.CString(m.ProfileError)}
	}
	p.roles, p.role_count = alloc[C.WDCloudRole](len(r.Roles)), C.size_t(len(r.Roles))
	for i, r := range r.Roles {
		v := &unsafe.Slice(p.roles, int(p.role_count))[i]
		v.id = C.CString(r.Id)
		v.name = C.CString(r.Name)
		v.description = C.CString(r.GetDescription())
		v.permissions, v.permission_count = cloudStrings(r.Permissions)
	}
	p.devices, p.device_count = alloc[C.WDOrganization](len(r.Devices)), C.size_t(len(r.Devices))
	for i, d := range r.Devices {
		unsafe.Slice(p.devices, len(r.Devices))[i] = C.WDOrganization{id: C.CString(d.Id), name: C.CString(d.Name)}
	}
	p.access, p.access_count = alloc[C.WDAccessEntry](len(r.Access)), C.size_t(len(r.Access))
	for i, a := range r.Access {
		unsafe.Slice(p.access, len(r.Access))[i] = C.WDAccessEntry{id: C.CString(a.Id), principal_id: C.CString(a.PrincipalId), principal_name: C.CString(a.PrincipalDisplayName), target_id: C.CString(a.TargetRef), target_name: C.CString(a.TargetDisplayName), principal_type: C.WDPrincipalType(a.PrincipalType), target_type: C.WDAccessTarget(a.TargetType), effect: C.WDAccessEffect(a.Effect)}
	}
	p.invites, p.invite_count = alloc[C.WDCloudInvite](len(r.Invites)), C.size_t(len(r.Invites))
	for i, a := range r.Invites {
		v := &unsafe.Slice(p.invites, len(r.Invites))[i]
		v.id = C.CString(a.Id)
		v.role = C.CString(a.Role)
		v.expires = C.int64_t(a.GetExpiresAt().GetSeconds())
		if a.Revoked {
			v.revoked = 1
		}
	}
	return p
}

//export WDOrganizationFree
func WDOrganizationFree(p *C.WDOrganizationResult) {
	if p == nil {
		return
	}
	free(p.name)
	free(p.status.message)
	for _, m := range unsafe.Slice(p.members, int(p.member_count)) {
		free(m.id)
		free(m.name)
		free(m.email)
		free(m.role_id)
		free(m.profile_error)
	}
	for _, r := range unsafe.Slice(p.roles, int(p.role_count)) {
		free(r.id)
		free(r.name)
		free(r.description)
		freeCloudStrings(r.permissions, r.permission_count)
	}
	for _, d := range unsafe.Slice(p.devices, int(p.device_count)) {
		free(d.id)
		free(d.name)
	}
	for _, a := range unsafe.Slice(p.access, int(p.access_count)) {
		free(a.id)
		free(a.principal_id)
		free(a.principal_name)
		free(a.target_id)
		free(a.target_name)
	}
	for _, i := range unsafe.Slice(p.invites, int(p.invite_count)) {
		free(i.id)
		free(i.role)
	}
	freeCloudStrings(p.permissions, p.permission_count)
	free(p.members)
	free(p.roles)
	free(p.devices)
	free(p.access)
	free(p.invites)
	free(p.team_error)
	free(p.roles_error)
	free(p.devices_error)
	free(p.access_error)
	free(p.invites_error)
	free(p.permissions_error)
	free(p)
}

//export WDAddMember
func WDAddMember(account, org, email, role *C.char) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopAddMember(ctx, C.GoString(account), C.GoString(org), C.GoString(email), C.GoString(role)))
}

//export WDSetMemberRole
func WDSetMemberRole(account, org, user, role *C.char) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopSetMemberRole(ctx, C.GoString(account), C.GoString(org), C.GoString(user), C.GoString(role)))
}

//export WDRemoveMember
func WDRemoveMember(account, org, user *C.char) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopRemoveMember(ctx, C.GoString(account), C.GoString(org), C.GoString(user)))
}

//export WDCreateInvite
func WDCreateInvite(account, org, role *C.char, expires C.int64_t) *C.WDInviteLinkResult {
	ctx, cancel := cloudContext()
	defer cancel()
	link, e := commands.DesktopCreateInvite(ctx, C.GoString(account), C.GoString(org), C.GoString(role), int64(expires))
	p := alloc[C.WDInviteLinkResult](1)
	p.status = status(e)
	p.link = C.CString(link)
	return p
}

//export WDInviteLinkFree
func WDInviteLinkFree(p *C.WDInviteLinkResult) {
	if p == nil {
		return
	}
	free(p.link)
	free(p.status.message)
	free(p)
}

//export WDRevokeInvite
func WDRevokeInvite(account, org, id *C.char) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopRevokeInvite(ctx, C.GoString(account), C.GoString(org), C.GoString(id)))
}

//export WDCreateAccess
func WDCreateAccess(account, org, user, device *C.char, effect C.WDAccessEffect) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopCreateAccess(ctx, C.GoString(account), C.GoString(org), C.GoString(user), C.GoString(device), pb.AccessEffect(effect)))
}

//export WDDeleteAccess
func WDDeleteAccess(account, org, id *C.char) *C.WDStatus {
	ctx, cancel := cloudContext()
	defer cancel()
	return statusResult(commands.DesktopDeleteAccess(ctx, C.GoString(account), C.GoString(org), C.GoString(id)))
}

//export WDDeviceAccess
func WDDeviceAccess(account, org, device *C.char) *C.WDOrganizationResult {
	ctx, cancel := cloudContext()
	defer cancel()
	entries, e := commands.DesktopDeviceAccess(ctx, C.GoString(account), C.GoString(org), C.GoString(device))
	return organizationResult(&commands.DesktopOrganization{Access: entries}, e)
}
