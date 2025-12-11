import Foundation
import GRPCCore
import Logging
import Noora
import WendyCloudGRPC
import WendySDK

struct CloudGRPCClient {
    let grpc: GRPCClient<GRPCTransport>
    let cloudHost: String
    let metadata: Metadata

    func listOrganizations() async throws -> [Wendycloud_V1_Organization] {
        let orgsAPI = Wendycloud_V1_OrganizationService.Client(wrapping: grpc)
        return try await orgsAPI.listOrganizations(
            .with {
                $0.limit = 25
            },
            metadata: metadata
        ) { response in
            var orgs = [Wendycloud_V1_Organization]()
            for try await org in response.messages {
                orgs.append(org.organization)
            }
            return orgs
        }
    }
}
