import Foundation
import HTTPTypes
import Hummingbird
import NIOCore
import NIOSSL
import SwiftASN1
import X509

/// Retain the real accepted channel. HTTP headers never supply caller identity.
struct RegistryRequestContext: RequestContext {
    var coreContext: CoreRequestContextStorage
    let channel: any Channel
    init(source: ApplicationRequestContextSource) {
        coreContext = .init(source: source)
        channel = source.channel
    }
}

struct RegistryAccess: Sendable {
    var leaf: Certificate?
    var session: String?
    var owner: String
    var deadline: Int64?
    func checkTime() throws {
        if let deadline, Date().timeIntervalSince1970 >= Double(deadline) {
            throw HTTPError(.forbidden, message: "delivery credential or job expired")
        }
    }
}

enum RegistryAuthorization {
    enum Operation: Sendable { case ping, metadata, read, stage, publish }
    static let header = HTTPField.Name("Wendy-Build-Delivery")!
    static func access(
        request: Request,
        context: RegistryRequestContext,
        tls: RegistryTLS.Configuration?,
        authority: BuildDeliveryAuthority?,
        repository: String,
        operation: Operation
    ) async throws -> RegistryAccess {
        guard let tls else { return RegistryAccess(owner: "bootstrap") }
        guard let peer = try await context.channel.nioSSL_peerCertificate().get(),
            let leaf = try? Certificate(derEncoded: peer.toDERBytes()),
            Date() >= leaf.notValidBefore, Date() < leaf.notValidAfter
        else { throw HTTPError(.forbidden, message: "verified current mTLS identity required") }
        let identity = try OrgIdentity.identity(fromLeaf: leaf)
        if operation == .ping {
            if identity?.entityType == "asset" {
                guard let own = tls.deviceScope, identity?.scope.matches(own) == true else {
                    throw HTTPError(.forbidden)
                }
            } else {
                try RPCIdentityInterceptor.checkOperator(
                    leaf: leaf,
                    expectedScope: tls.deviceScope,
                    orgMode: tls.orgMode
                )
            }
            return RegistryAccess(
                leaf: leaf,
                owner: try BuildDeliveryContract.fingerprint(leaf),
                deadline: Int64(leaf.notValidAfter.timeIntervalSince1970)
            )
        }
        if identity?.entityType == "asset" {
            guard operation != .read, let authority else {
                throw HTTPError(
                    .forbidden,
                    message: "device credentials do not authorize registry reads"
                )
            }
            let values = request.headers[values: header]
            guard values.count == 1, let id = values.first,
                try BuildDeliveryContract.decodeURL(id).count == 32
            else {
                throw HTTPError(.forbidden, message: "an admitted delivery session is required")
            }
            let claims = try await authority.policy(id, leaf: leaf, repository: repository)
            if operation == .stage {
                try await authority.stage(id, leaf: leaf, repository: repository)
            }
            return RegistryAccess(
                leaf: leaf,
                session: id,
                owner: "delivery-" + id,
                deadline: min(claims.exp, Int64(leaf.notValidAfter.timeIntervalSince1970))
            )
        }
        try RPCIdentityInterceptor.checkOperator(
            leaf: leaf,
            expectedScope: tls.deviceScope,
            orgMode: tls.orgMode
        )
        return RegistryAccess(
            leaf: leaf,
            owner: "operator-" + (try BuildDeliveryContract.fingerprint(leaf)),
            deadline: Int64(leaf.notValidAfter.timeIntervalSince1970)
        )
    }

    /// Absolute expiry also closes idle HTTP bodies. A detached body reader
    /// cannot keep writing after its credential/job has expired.
    static func beforeDeadline<T: Sendable>(
        access: RegistryAccess,
        channel: any Channel,
        operation: @escaping @Sendable () async throws -> T
    ) async throws -> T {
        try access.checkTime()
        guard let deadline = access.deadline else { return try await operation() }
        return try await withThrowingTaskGroup(of: T.self) { group in
            group.addTask { try await operation() }
            group.addTask {
                let remaining = max(0, Double(deadline) - Date().timeIntervalSince1970)
                try await Task.sleep(for: .seconds(remaining))
                try? await channel.close()
                throw HTTPError(.forbidden, message: "delivery credential or job expired")
            }
            defer { group.cancelAll() }
            guard let value = try await group.next() else { throw CancellationError() }
            return value
        }
    }
}
