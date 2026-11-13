import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("AgentService power mode")
struct AgentServicePowerModeTests {
    @Test("getPowerMode is unavailable off Jetson")
    func getIsUnavailable() async {
        let service = AgentService()
        do {
            _ = try await service.getPowerMode(
                request: ServerRequest(
                    metadata: [:],
                    message: Wendy_Agent_Services_V1_GetPowerModeRequest()
                ),
                context: makePowerModeContext(method: "GetPowerMode")
            )
            Issue.record("expected getPowerMode to throw")
        } catch let error as RPCError {
            #expect(error.code == .unavailable)
        } catch {
            Issue.record("expected RPCError, got \(error)")
        }
    }

    @Test("setPowerMode is unavailable off Jetson")
    func setIsUnavailable() async {
        let service = AgentService()
        var request = Wendy_Agent_Services_V1_SetPowerModeRequest()
        request.mode = "MAXN"
        do {
            _ = try await service.setPowerMode(
                request: ServerRequest(metadata: [:], message: request),
                context: makePowerModeContext(method: "SetPowerMode")
            )
            Issue.record("expected setPowerMode to throw")
        } catch let error as RPCError {
            #expect(error.code == .unavailable)
        } catch {
            Issue.record("expected RPCError, got \(error)")
        }
    }
}

private func makePowerModeContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyAgentService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}
