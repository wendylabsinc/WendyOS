import Foundation

struct CloudOrganization: Identifiable, Hashable {
    let id: String
    let name: String
}
struct CloudMember: Identifiable, Hashable {
    let id: String
    let name: String
    let email: String
    let roleID: String
    let profileError: String
    var title: String { !name.isEmpty ? name : !email.isEmpty ? email : id }
}
struct CloudRole: Identifiable {
    let id: String
    let name: String
    let description: String
    let permissions: [String]
}
struct CloudAccessRule: Identifiable {
    let id: String
    let principalID: String
    let principalName: String
    let principalType: WDPrincipalType
    let targetID: String
    let targetName: String
    let targetType: WDAccessTarget
    let effect: WDAccessEffect
    var principal: String { principalName.isEmpty ? principalID : principalName }
    var principalKind: String {
        switch principalType {
        case WD_PRINCIPAL_USER: "Person"
        case WD_PRINCIPAL_GROUP: "Group"
        case WD_PRINCIPAL_DEVICE: "Device"
        default: "Unknown"
        }
    }
    var target: String {
        targetType == WD_TARGET_ALL ? "All devices" : targetName.isEmpty ? targetID : targetName
    }
    var scope: String {
        switch targetType {
        case WD_TARGET_DEVICE: "Device"
        case WD_TARGET_SITE: "Site"
        case WD_TARGET_TAG: "Tag"
        case WD_TARGET_ALL: "Organization"
        default: "Unknown"
        }
    }
    var effectName: String {
        effect == WD_ACCESS_ALLOW ? "Allow" : effect == WD_ACCESS_DENY ? "Deny" : "Unknown"
    }
}
struct CloudInvitation: Identifiable {
    let id: String
    let role: String
    let expires: Date
    let revoked: Bool
    var state: String { revoked ? "Revoked" : expires <= Date() ? "Expired" : "Active" }
}
struct CloudOrganizationSnapshot {
    let name: String
    let members: [CloudMember]
    let roles: [CloudRole]
    let devices: [CloudOrganization]
    let rules: [CloudAccessRule]
    let invitations: [CloudInvitation]
    let permissions: Set<String>
    let teamError: String
    let rolesError: String
    let devicesError: String
    let accessError: String
    let invitesError: String
    let permissionsError: String
}

// Separate from device calls so organization reads cannot delay camera or app controls.
final class CloudManagementClient: Sendable {
    private let queue = DispatchQueue(label: "dev.wendy.desktop.organization", qos: .userInitiated)
    private func perform<T: Sendable>(_ body: @escaping @Sendable () throws -> T) async throws -> T {
        try await BlockingWork.run(on: queue, body)
    }
    private static func text(_ p: UnsafeMutablePointer<CChar>?) -> String {
        p.map { String(cString: $0) } ?? ""
    }
    private static func array<T>(_ p: UnsafeMutablePointer<T>?, _ count: Int) -> [T] {
        Array(UnsafeBufferPointer(start: p, count: count))
    }
    private static func strings(
        _ p: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?,
        _ count: Int
    ) -> [String] { array(p, count).map(text) }
    private static func finish(_ p: UnsafeMutablePointer<WDStatus>?) throws {
        guard let p else { throw WendyError(message: "Missing organization response.") }
        defer { WDStatusFree(p) }
        try GoClient.check(p.pointee)
    }
    func organizations(account: String) async throws -> [CloudOrganization] {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            guard let p = WDOrganizations(s.add(account)) else {
                throw WendyError(message: "Missing organizations.")
            }
            defer { WDOrganizationsFree(p) }
            try GoClient.check(p.pointee.status)
            return Self.array(p.pointee.items, p.pointee.count).map {
                CloudOrganization(id: Self.text($0.id), name: Self.text($0.name))
            }
        }
    }
    func overview(account: String, org: String) async throws -> CloudOrganizationSnapshot {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            guard let p = WDOrganizationOverview(s.add(account), s.add(org)) else {
                throw WendyError(message: "Missing organization.")
            }
            defer { WDOrganizationFree(p) }
            try GoClient.check(p.pointee.status)
            return Self.snapshot(p.pointee)
        }
    }
    func rules(account: String, org: String, device: String) async throws -> [CloudAccessRule] {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            guard let p = WDDeviceAccess(s.add(account), s.add(org), s.add(device)) else {
                throw WendyError(message: "Missing access rules.")
            }
            defer { WDOrganizationFree(p) }
            try GoClient.check(p.pointee.status)
            return Self.snapshot(p.pointee).rules
        }
    }
    static func snapshot(_ r: WDOrganizationResult) -> CloudOrganizationSnapshot {
        CloudOrganizationSnapshot(
            name: text(r.name),
            members: array(r.members, r.member_count).map {
                CloudMember(
                    id: text($0.id),
                    name: text($0.name),
                    email: text($0.email),
                    roleID: text($0.role_id),
                    profileError: text($0.profile_error)
                )
            }.sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending },
            roles: array(r.roles, r.role_count).map {
                CloudRole(
                    id: text($0.id),
                    name: text($0.name),
                    description: text($0.description),
                    permissions: strings($0.permissions, $0.permission_count)
                )
            }.sorted { $0.name < $1.name },
            devices: array(r.devices, r.device_count).map {
                CloudOrganization(id: text($0.id), name: text($0.name))
            }.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending },
            rules: array(r.access, r.access_count).map {
                CloudAccessRule(
                    id: text($0.id),
                    principalID: text($0.principal_id),
                    principalName: text($0.principal_name),
                    principalType: $0.principal_type,
                    targetID: text($0.target_id),
                    targetName: text($0.target_name),
                    targetType: $0.target_type,
                    effect: $0.effect
                )
            },
            invitations: array(r.invites, r.invite_count).map {
                CloudInvitation(
                    id: text($0.id),
                    role: text($0.role),
                    expires: Date(timeIntervalSince1970: Double($0.expires)),
                    revoked: $0.revoked != 0
                )
            }.sorted { $0.expires > $1.expires },
            permissions: Set(strings(r.permissions, r.permission_count)),
            teamError: text(r.team_error),
            rolesError: text(r.roles_error),
            devicesError: text(r.devices_error),
            accessError: text(r.access_error),
            invitesError: text(r.invites_error),
            permissionsError: text(r.permissions_error)
        )
    }
    func addMember(account: String, org: String, email: String, role: String) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(WDAddMember(s.add(account), s.add(org), s.add(email), s.add(role)))
        }
    }
    func setRole(account: String, org: String, user: String, role: String) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(WDSetMemberRole(s.add(account), s.add(org), s.add(user), s.add(role)))
        }
    }
    func removeMember(account: String, org: String, user: String) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(WDRemoveMember(s.add(account), s.add(org), s.add(user)))
        }
    }
    func invite(account: String, org: String, role: String, expires: Date) async throws -> String {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            guard
                let p = WDCreateInvite(
                    s.add(account),
                    s.add(org),
                    s.add(role),
                    Int64(expires.timeIntervalSince1970)
                )
            else { throw WendyError(message: "Missing invitation.") }
            defer { WDInviteLinkFree(p) }
            try GoClient.check(p.pointee.status)
            return Self.text(p.pointee.link)
        }
    }
    func revokeInvite(account: String, org: String, id: String) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(WDRevokeInvite(s.add(account), s.add(org), s.add(id)))
        }
    }
    func addAccess(
        account: String,
        org: String,
        user: String,
        device: String,
        effect: WDAccessEffect
    ) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(
                WDCreateAccess(s.add(account), s.add(org), s.add(user), s.add(device), effect)
            )
        }
    }
    func removeAccess(account: String, org: String, id: String) async throws {
        try await perform {
            let s = CStringStorage()
            defer { withExtendedLifetime(s) {} }
            try Self.finish(WDDeleteAccess(s.add(account), s.add(org), s.add(id)))
        }
    }
}
