import GRPCCore
import GRPCNIOTransportHTTP2
import WendyAgentGRPC
import X509

struct BuildDeliveryService: Wendy_Agent_Services_V2_WendyBuildDeliveryService.SimpleServiceProtocol
{
    let authority: BuildDeliveryAuthority
    let expectedScope: OrgIdentity.Scope?
    let orgMode: ClientCertAuthorizer.OrgEnforcementMode
    private func leaf(_ context: ServerContext) throws -> Certificate {
        guard
            let leaf = (context.transportSpecific as? HTTP2ServerTransport.Posix.Context)?
                .peerCertificate
        else { throw RPCError(code: .unauthenticated, message: "verified mTLS identity required") }
        return leaf
    }
    func getDeliveryCapabilities(
        request: Wendy_Agent_Services_V2_GetDeliveryCapabilitiesRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_GetDeliveryCapabilitiesResponse {
        do {
            let (target, certificates) = try await authority.capabilities()
            var response = Wendy_Agent_Services_V2_GetDeliveryCapabilitiesResponse()
            response.contractVersion = 1
            response.targetPrincipal = target
            response.certificateChain = certificates
            return response
        } catch {
            throw RPCError(
                code: .failedPrecondition,
                message: "operator delivery unavailable: \(error)"
            )
        }
    }
    func getDeliveryReference(
        request: Wendy_Agent_Services_V2_GetDeliveryReferenceRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_GetDeliveryReferenceResponse {
        try RPCIdentityInterceptor.checkOperator(
            leaf: leaf(context),
            expectedScope: expectedScope,
            orgMode: orgMode
        )
        do { _ = try BuildDeliveryContract.reference(request.reference) } catch {
            throw RPCError(code: .invalidArgument, message: "unsupported local image reference")
        }
        var response = Wendy_Agent_Services_V2_GetDeliveryReferenceResponse()
        response.digest = try await authority.referenceDigest(request.reference)
        return response
    }
    func admitDelivery(
        request: Wendy_Agent_Services_V2_AdmitDeliveryRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_AdmitDeliveryResponse {
        let operatorLeaf = try leaf(context)
        try RPCIdentityInterceptor.checkOperator(
            leaf: operatorLeaf,
            expectedScope: expectedScope,
            orgMode: orgMode
        )
        do {
            var response = Wendy_Agent_Services_V2_AdmitDeliveryResponse()
            response.session = try await authority.admit(
                request.authorizationJws,
                operator: operatorLeaf
            )
            return response
        } catch {
            throw RPCError(code: .permissionDenied, message: "delivery admission refused: \(error)")
        }
    }
    func getDeliveryStatus(
        request: Wendy_Agent_Services_V2_GetDeliveryStatusRequest,
        context: ServerContext
    ) async throws -> Wendy_Agent_Services_V2_GetDeliveryStatusResponse {
        do {
            let status = try await authority.status(request.session, leaf: leaf(context))
            var response = Wendy_Agent_Services_V2_GetDeliveryStatusResponse()
            response.reference = status.reference
            response.outputDigest = status.output
            response.completed = status.completed
            return response
        } catch {
            throw RPCError(code: .permissionDenied, message: "delivery status refused: \(error)")
        }
    }
}
