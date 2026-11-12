import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCProtobuf
import Logging
import WendyAgentGRPC
import X509

/// Per-method authorization after the main listener's mTLS verification.
/// Keep device capabilities aligned with Go's deviceRPCAllowed; registration
/// alone must never make a new management method available to device callers.
struct RPCIdentityInterceptor: ServerInterceptor {
    let expectedScope: OrgIdentity.Scope?
    let logger: Logger
    let orgMode: ClientCertAuthorizer.OrgEnforcementMode

    static func deviceRPCAllowed(_ method: String) -> Bool {
        switch method {
        case "wendy.agent.services.v2.WendyBuildDeliveryService/GetDeliveryCapabilities",
            "wendy.agent.services.v2.WendyBuildDeliveryService/AdmitDelivery",
            "wendy.agent.services.v2.WendyBuildDeliveryService/GetDeliveryStatus":
            return true
        case Wendy_Agent_Services_V1_WendyContainerService.Method.QueryChunks.descriptor
            .fullyQualifiedMethod,
            Wendy_Agent_Services_V1_WendyContainerService.Method.QueryLayers.descriptor
            .fullyQualifiedMethod,
            Wendy_Agent_Services_V1_WendyContainerService.Method.WriteChunks.descriptor
            .fullyQualifiedMethod,
            // No Swift descriptors/implementations yet for these two methods.
            // Preserve the shared policy without granting entire services.
            "wendy.agent.services.v1.WendyContainerService/PrepareImage",
            "wendy.agent.services.v2.WendyMeshService/MeshDial",
            Wendy_Agent_Services_V2_WendySensorService.Method.GetSensorManifest.descriptor
            .fullyQualifiedMethod,
            Wendy_Agent_Services_V2_WendySensorService.Method.StreamSensors.descriptor
            .fullyQualifiedMethod:
            return true
        default:
            return false
        }
    }

    static func denied(_ message: String) -> RPCError {
        let status = GoogleRPCStatus(
            code: .permissionDenied,
            message: message,
            details: [.errorInfo(reason: "RPC_IDENTITY_DENIED", domain: "wendy.dev")]
        )
        return RPCError(code: status.code, message: message, metadata: status.rpcErrorMetadata)
    }

    /// Reused by the OCI listener after TLS path verification. A device never
    /// gains an ambient management capability through a different transport.
    static func checkOperator(
        leaf: Certificate?,
        expectedScope: OrgIdentity.Scope?,
        orgMode: ClientCertAuthorizer.OrgEnforcementMode
    ) throws {
        guard let leaf else { throw denied("management requires operator credentials") }
        let identity: OrgIdentity.WendyIdentity?
        do { identity = try OrgIdentity.identity(fromLeaf: leaf) } catch {
            throw denied("malformed operator identity")
        }
        if let identity {
            guard identity.entityType == "user", let expectedScope, expectedScope.isKnown,
                identity.scope.matches(expectedScope)
            else { throw denied("management requires a matching tenant operator") }
        } else {
            guard orgMode == .grace, legacyUser(leaf) else {
                throw denied("management requires operator credentials")
            }
        }
    }

    static func check(
        leaf: Certificate?,
        method: String,
        expectedScope: OrgIdentity.Scope?,
        orgMode: ClientCertAuthorizer.OrgEnforcementMode = .strict
    ) throws {
        guard let leaf else {
            throw RPCError(
                code: .unauthenticated,
                message: "verified mTLS caller identity required"
            )
        }
        let parsed: OrgIdentity.WendyIdentity?
        do {
            parsed = try OrgIdentity.identity(fromLeaf: leaf)
        } catch {
            throw denied("client certificate has no recognized RPC caller identity")
        }
        guard let identity = parsed else {
            // The TLS verifier already checked the chain and tenant policy.
            // Fall back only when no authoritative identity was present, never
            // when parsing one failed or identified a device/signer.
            if orgMode == .grace && legacyUser(leaf) { return }
            throw denied("client certificate has no recognized RPC caller identity")
        }
        switch identity.entityType {
        case "user":
            // Tenant enforcement has already run in the TLS verifier. Existing
            // operator and handler-specific policies continue to apply.
            return
        case "asset":
            guard let expectedScope, expectedScope.isKnown, identity.scope.matches(expectedScope)
            else {
                throw denied("device RPC requires a matching tenant identity")
            }
            guard deviceRPCAllowed(method) else {
                throw denied(
                    "RPC is not permitted for device credentials; sign in with operator credentials"
                )
            }
        default:
            throw denied("client certificate role is not permitted for agent RPCs")
        }
    }

    private static func legacyUser(_ leaf: Certificate) -> Bool {
        var commonNames: [String] = []
        for relativeName in leaf.subject {
            for attribute in relativeName where attribute.type == .RDNAttributeType.commonName {
                commonNames.append(attribute.value.description)
            }
        }
        guard commonNames.count == 1, let cn = commonNames.first, cn.hasPrefix("wendy/user/")
        else { return false }
        let uid = cn.dropFirst("wendy/user/".count)
        return !uid.isEmpty && !uid.contains("/") && !uid.contains(":")
            && uid.rangeOfCharacter(from: .whitespacesAndNewlines.union(.controlCharacters)) == nil
    }

    @concurrent
    func intercept<Input: Sendable, Output: Sendable>(
        request: StreamingServerRequest<Input>,
        context: ServerContext,
        next:
            @concurrent @Sendable (StreamingServerRequest<Input>, ServerContext) async throws ->
            StreamingServerResponse<Output>
    ) async throws -> StreamingServerResponse<Output> {
        // Only transport-provided identity is trusted; request metadata is not
        // evidence of the caller's certificate or role.
        let leaf = (context.transportSpecific as? HTTP2ServerTransport.Posix.Context)?
            .peerCertificate
        do {
            try Self.check(
                leaf: leaf,
                method: context.descriptor.fullyQualifiedMethod,
                expectedScope: expectedScope,
                orgMode: orgMode
            )
        } catch {
            var metadata: Logger.Metadata = [
                "method": "/\(context.descriptor.fullyQualifiedMethod)",
                "remote": "\(context.remotePeer)",
                "error": "\(error)",
            ]
            if let leaf { metadata["serial"] = "\(leaf.serialNumber)" }
            logger.warning("rejected gRPC caller by RPC identity policy", metadata: metadata)
            throw error
        }
        return try await next(request, context)
    }
}
