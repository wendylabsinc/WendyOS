import Foundation
import GRPCCore
import Logging
import Noora
import WendyAgentGRPC
import WendySDK
import _NIOFileSystem

struct Agent {
    let client: GRPCClient<GRPCTransport>

    func provision(
        enrollmentToken: String,
        assetID: Int32,
        organizationID: Int32,
        cloudHost: String
    ) async throws {
        let service = Wendy_Agent_Services_V1_WendyProvisioningService.Client(wrapping: client)
        _ = try await service.startProvisioning(
            .with {
                $0.enrollmentToken = enrollmentToken
                $0.cloudHost = cloudHost
                $0.assetID = assetID
                $0.organizationID = organizationID
            }
        )
    }

    func discoverSSID() async throws -> String {
        let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)

        let networks = try await Noora().progressStep(
            message: "Listing available WiFi networks",
            successMessage: nil,
            errorMessage: nil,
            showSpinner: true
        ) { progress in
            try await agent.listWiFiNetworks(.init())
        }.networks

        // Group networks by SSID and keep the one with highest signal strength
        let uniqueNetworks = Dictionary(grouping: networks.filter { !$0.ssid.isEmpty }) { $0.ssid }
            .compactMapValues {
                networksWithSameSsid -> Wendy_Agent_Services_V1_ListWiFiNetworksResponse
                    .WiFiNetwork? in
                networksWithSameSsid.max(by: { $0.signalStrength < $1.signalStrength })
            }
            .values
            .sorted(by: {
                $0.signalStrength > $1.signalStrength
            })

        let ssids = uniqueNetworks.map { $0.ssid }

        let index = try await Noora().selectableTable(
            headers: ["SSID", "Strength"],
            rows: uniqueNetworks.map { network in
                let signalDisplay =
                    network.hasSignalStrength ? "\(network.signalStrength)" : "Unknown"
                return [network.ssid, signalDisplay]
            },
            pageSize: uniqueNetworks.count
        )

        return ssids[index]
    }

    func connectToWiFi(
        ssid: String,
        password: String
    ) async throws -> Wendy_Agent_Services_V1_ConnectToWiFiResponse {
        let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
        return try await agent.connectToWiFi(
            .with {
                $0.ssid = ssid
                $0.password = password
            }
        )
    }

    func update(
        fromBinary path: String,
        onProgress: (Double) -> Void = { _ in }
    ) async throws -> Bool {
        let logger = Logger(label: "sh.wendyengineer.agent.update")
        let agent = Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
        let (progress, continuation) = AsyncStream<Double>.makeStream()
        
        return try await withThrowingTaskGroup(of: Bool.self) { group in
            group.addTask {
                defer { continuation.finish() }
                return try await agent.updateAgent { writer in
                    logger.debug("Opening file...")
                    do {
                        try await FileSystem.shared.withFileHandle(forReadingAt: FilePath(path)) { handle in
                            let fileSize = try await handle.info().size
                            var writtenBytes: Int64 = 0
                            logger.debug("Uploading binary...")
                            for try await chunk in handle.readChunks() {
                                try await writer.write(
                                    .with {
                                        $0.chunk = .with {
                                            $0.data = Data(buffer: chunk)
                                        }
                                    }
                                )
                                writtenBytes += Int64(chunk.readableBytes)
                                continuation.yield(Double(writtenBytes) / Double(fileSize))
                            }

                            logger.debug("Finalizing update")
                            try await writer.write(
                                .with {
                                    $0.control = .with {
                                        $0.command = .update(.init())
                                    }
                                }
                            )
                        }
                    } catch {
                        logger.error("Failed to upload binary: \(error)")
                        throw error
                    }
                } onResponse: { response in
                    do {
                        for try await event in response.messages {
                            switch event.responseType {
                            case .updated:
                                return true
                            case .none:
                                ()
                            }
                        }
                        return false
                    } catch {
                        logger.error(
                            "Failed to update agent",
                            metadata: [
                                "error": "\(error)"
                            ]
                        )
                        throw error
                    }
                }
            }
            
            for await value in progress {
                onProgress(value)
            }

            guard let result = try await group.next() else {
                throw CancellationError()
            }

            return result
        }
    }
}
