import Foundation
import GRPCCore
import WendyAgentGRPC

/// The v2 provisioning surface the `wendy` CLI's direct PKI enrollment speaks
/// (`IsProvisioned` preflight, then `StartACMEProvisioning`). Delegates to the
/// v1 `ProvisioningService`, which owns the state, exactly as the Go agent's
/// `ProvisioningServiceV2` does; v1 stays registered for existing callers.
struct ProvisioningServiceV2: Wendy_Agent_Services_V2_WendyProvisioningService.SimpleServiceProtocol
{
    let v1: ProvisioningService

    func isProvisioned(
        request: Wendy_Agent_Services_V2_IsProvisionedRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_IsProvisionedResponse {
        let info = await self.v1.provisioningInfo()
        var response = Wendy_Agent_Services_V2_IsProvisionedResponse()
        if info.enrolled {
            var provisioned = Wendy_Agent_Services_V2_ProvisionedResponse()
            provisioned.cloudHost = info.cloudHost
            provisioned.organizationID = info.orgID
            provisioned.assetID = info.assetID
            provisioned.principalUri = info.principalURI
            provisioned.assetUuid = Self.deviceUUID(from: info.principalURI)
            // The Swift agent does not yet implement Cloud-authenticated reset
            // and durable device preparation. Both capability flags stay false.
            provisioned.cloudUnenrollmentSupported = false
            provisioned.cloudAuthenticatedResetSupported = false
            response.provisioned = provisioned
        } else {
            var notProvisioned = Wendy_Agent_Services_V2_NotProvisionedResponse()
            // Refusing here stops the CLI before Cloud mints a single-use EAB.
            let unavailable = self.v1.acmeEnrollmentUnavailableReason()
            notProvisioned.acmeEnrollmentSupported = unavailable == nil
            notProvisioned.acmeEnrollmentUnavailableReason = unavailable ?? ""
            response.notProvisioned = notProvisioned
        }
        return response
    }

    /// The Cloud adapter field and certificate principal name the same device.
    /// Legacy or path-shaped device names do not acquire an invented UUID.
    static func deviceUUID(from principal: String) -> String {
        guard let identity = try? OrgIdentity.parsePrincipal(principal),
            let tenant = identity.tenantUUID,
            principal == "spiffe://wendy.sh/tenant/\(tenant)/device/\(identity.entityID)",
            let id = UUID(uuidString: identity.entityID),
            id.uuidString.lowercased() == identity.entityID
        else { return "" }
        return identity.entityID
    }

    func startProvisioning(
        request: Wendy_Agent_Services_V2_StartProvisioningRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_StartProvisioningResponse {
        var legacy = Wendy_Agent_Services_V1_StartProvisioningRequest()
        legacy.organizationID = request.organizationID
        legacy.enrollmentToken = request.enrollmentToken
        legacy.cloudHost = request.cloudHost
        legacy.assetID = request.assetID
        _ = try await self.v1.startProvisioning(request: legacy, context: context)
        return Wendy_Agent_Services_V2_StartProvisioningResponse()
    }

    func startACMEProvisioning(
        request: Wendy_Agent_Services_V2_StartACMEProvisioningRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_StartACMEProvisioningResponse {
        let config = ACMEEnrollmentConfig(
            directoryURL: request.directoryURL,
            deviceID: request.deviceID,
            eabKeyID: request.eabKeyID,
            eabHMACKey: request.eabHmacKey
        )
        var response = Wendy_Agent_Services_V2_StartACMEProvisioningResponse()
        response.principalUri = try await self.v1.startACMEProvisioning(
            cloudHost: request.cloudHost,
            config: config
        )
        return response
    }

    func unprovision(
        request: Wendy_Agent_Services_V2_UnprovisionRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_UnprovisionResponse {
        // Caller-supplied principals, revocation proofs and Cloud deletion
        // records are not Cloud-authenticated reset authorization, so a direct
        // PKI identity is never erased here, whatever principal is expected.
        guard await self.v1.provisioningInfo().principalURI.isEmpty else {
            throw RPCError(
                code: .failedPrecondition,
                message:
                    "direct PKI reset is disabled pending Cloud-authenticated authorization and durable device preparation; keys retained"
            )
        }
        _ = try await self.v1.unprovision(
            request: Wendy_Agent_Services_V1_UnprovisionRequest(),
            context: context
        )
        return Wendy_Agent_Services_V2_UnprovisionResponse()
    }

    func revokeACMECertificate(
        request: Wendy_Agent_Services_V2_RevokeACMECertificateRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_RevokeACMECertificateResponse {
        throw RPCError(
            code: .unimplemented,
            message: "Cloud owns device unenrollment and certificate revocation"
        )
    }

    func checkACMERevocation(
        request: Wendy_Agent_Services_V2_RevokeACMECertificateRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_RevokeACMECertificateResponse {
        throw RPCError(
            code: .unimplemented,
            message: "use Cloud binding preflight; no Agent ACME revocation path"
        )
    }
}
