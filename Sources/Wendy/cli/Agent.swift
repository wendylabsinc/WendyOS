import Crypto
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
        struct WifiNetwork: Sendable {
            let ssid: String
            let signalStrength: Int32

            init(network: Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork) {
                self.ssid = network.ssid
                self.signalStrength = network.hasSignalStrength ? network.signalStrength : 0
            }
        }

        actor LiveData: nonisolated AsyncSequence {
            nonisolated let source: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>
            var networks: [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork] = []

            init(source: Wendy_Agent_Services_V1_WendyAgentService.Client<GRPCTransport>) {
                self.source = source
            }

            func setNetworks(
                _ networks: [Wendy_Agent_Services_V1_ListWiFiNetworksResponse.WiFiNetwork]
            ) {
                self.networks = networks
            }

            nonisolated func makeAsyncIterator() -> AsyncIterator {
                return AsyncIterator(actor: self)
            }

            struct AsyncIterator: AsyncIteratorProtocol {
                let actor: LiveData

                func next() async throws -> TableData? {
                    let networks = try await actor.source.listWiFiNetworks(.init()).networks
                    await actor.setNetworks(networks)
                    // Group networks by SSID and keep the one with highest signal strength
                    let uniqueNetworks = Dictionary(grouping: networks.filter { !$0.ssid.isEmpty })
                    { $0.ssid }
                    .compactMapValues {
                        networksWithSameSsid -> Wendy_Agent_Services_V1_ListWiFiNetworksResponse
                            .WiFiNetwork? in
                        networksWithSameSsid.max(by: { $0.signalStrength < $1.signalStrength })
                    }
                    .values
                    .sorted(by: {
                        $0.signalStrength > $1.signalStrength
                    })

                    let rows = uniqueNetworks.map { network -> TableRow in
                        return [
                            "\(network.ssid)",
                            "\(network.hasSignalStrength ? "\(network.signalStrength)" : "Unknown")",
                        ]
                    }

                    return TableData(
                        columns: [TableColumn(title: "SSID"), TableColumn(title: "Strength")],
                        rows: rows
                    )
                }
            }
        }

        let data = LiveData(
            source: Wendy_Agent_Services_V1_WendyAgentService.Client(wrapping: client)
        )
        guard let initial = try await data.makeAsyncIterator().next() else {
            throw CancellationError()
        }
        let index = try await Noora().selectableTable(initial, updates: data, pageSize: 20)
        let networks = await data.networks
        return networks[index].ssid
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
                        try await FileSystem.shared.withFileHandle(
                            forReadingAt: FilePath(path)
                        ) { handle in
                            var hash = SHA256()
                            let fileSize = try await handle.info().size
                            var writtenBytes: Int64 = 0
                            logger.debug("Uploading binary...")
                            for try await chunk in handle.readChunks() {
                                hash.update(data: chunk.readableBytesView)
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
                            let finalHash = hash.finalize().map { String(format: "%02x", $0) }
                                .joined()
                            try await writer.write(
                                .with {
                                    $0.control = .with {
                                        $0.command = .update(
                                            .with {
                                                $0.sha256 = finalHash
                                            }
                                        )
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
