import Darwin
import Foundation
import Testing

@testable import WendyAgentCore

@Suite("WendyAgent startup", .serialized)
struct WendyAgentStartupTests {
    @Test("startup is local-only and does not advertise")
    func localOnlyStartup() async throws {
        let agent = WendyAgent(
            configuration: .init(port: 0, otelPort: 0),
            probesLinuxContainerRuntime: false,
            reportedVersion: { "test" }
        )
        try await agent.start()
        do {
            // No Bonjour task must not disable monitoring or look like failure.
            try await Task.sleep(for: .milliseconds(100))
            #expect(await agent.status == .running)
            let port = try #require(await agent.listeningPort)
            #expect(await agent.advertisedPort == nil)
            let listeners = try listenerNames(port: port)
            #expect(listeners.contains("n127.0.0.1:\(port)"))
            #expect(!listeners.contains("n*:\(port)"))
        } catch {
            await agent.stop()
            throw error
        }
        await agent.stop()
    }

    @Test("access changes rebind and advertise the running server's actual port")
    func networkAccessSwitching() async throws {
        let agent = WendyAgent(
            configuration: .init(port: 0, otelPort: 0),
            probesLinuxContainerRuntime: false,
            reportedVersion: { "test" }
        )
        try await agent.start()
        do {
            try await agent.setNetworkAccess(allowed: true)
            #expect(await agent.configuration.allowsNetworkAccess)
            let networkPort = try #require(await agent.listeningPort)
            #expect(await agent.advertisedPort == networkPort)
            #expect(try listenerNames(port: networkPort).contains("n*:\(networkPort)"))

            try await agent.setNetworkAccess(allowed: false)
            #expect(!(await agent.configuration.allowsNetworkAccess))
            #expect(await agent.advertisedPort == nil)
            let localPort = try #require(await agent.listeningPort)
            let listeners = try listenerNames(port: localPort)
            #expect(listeners.contains("n127.0.0.1:\(localPort)"))
            #expect(!listeners.contains("n*:\(localPort)"))
            #expect(await agent.status == .running)
        } catch {
            await agent.stop()
            throw error
        }
        await agent.stop()
    }

    @Test("network startup listens and advertises on an available preferred port")
    func usesPreferredPort() async throws {
        let reservation = try OccupiedTCPPort()
        let preferredPort = reservation.port
        reservation.close()
        let agent = WendyAgent(
            configuration: .init(host: "::", port: preferredPort, otelPort: 0),
            probesLinuxContainerRuntime: false,
            reportedVersion: { "test" }
        )
        try await agent.start()
        #expect(await agent.listeningPort == preferredPort)
        #expect(await agent.advertisedPort == preferredPort)
        await agent.stop()
    }

    @Test("network startup advertises a free port when the preferred port is occupied")
    func recoversFromPortConflict() async throws {
        let occupiedPort = try OccupiedTCPPort()
        defer { occupiedPort.close() }
        let agent = WendyAgent(
            configuration: .init(host: "::", port: occupiedPort.port, otelPort: 0),
            probesLinuxContainerRuntime: false,
            reportedVersion: { "test" }
        )
        try await agent.start()
        do {
            let port = try #require(await agent.listeningPort)
            #expect(port != occupiedPort.port)
            #expect(await agent.advertisedPort == port)
        } catch {
            await agent.stop()
            throw error
        }
        await agent.stop()
    }

    @Test("local startup recovers from a port conflict without opening network access")
    func localPortConflict() async throws {
        let occupiedPort = try OccupiedTCPPort(ipv4: true)
        defer { occupiedPort.close() }
        let agent = WendyAgent(
            configuration: .init(port: occupiedPort.port, otelPort: 0),
            probesLinuxContainerRuntime: false,
            reportedVersion: { "test" }
        )
        try await agent.start()
        do {
            let port = try #require(await agent.listeningPort)
            #expect(port != occupiedPort.port)
            #expect(await agent.advertisedPort == nil)
            let listeners = try listenerNames(port: port)
            #expect(listeners.contains("n127.0.0.1:\(port)"))
            #expect(!listeners.contains("n*:\(port)"))
        } catch {
            await agent.stop()
            throw error
        }
        await agent.stop()
    }
}

private func listenerNames(port: Int) throws -> String {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/sbin/lsof")
    process.arguments = [
        "-nP", "-a", "-p", String(ProcessInfo.processInfo.processIdentifier), "-iTCP:\(port)",
        "-sTCP:LISTEN", "-F", "n",
    ]
    let output = Pipe()
    process.standardOutput = output
    try process.run()
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    #expect(process.terminationStatus == 0)
    return String(decoding: data, as: UTF8.self)
}

private final class OccupiedTCPPort: @unchecked Sendable {
    let fileDescriptor: Int32
    let port: Int

    init(ipv4: Bool = false) throws {
        var hints = addrinfo()
        hints.ai_family = ipv4 ? AF_INET : AF_INET6
        hints.ai_socktype = SOCK_STREAM
        hints.ai_protocol = IPPROTO_TCP
        var result: UnsafeMutablePointer<addrinfo>?
        let lookupError = getaddrinfo(ipv4 ? "127.0.0.1" : "::", "0", &hints, &result)
        guard lookupError == 0, let address = result else {
            throw NSError(domain: "getaddrinfo", code: Int(lookupError))
        }
        defer { freeaddrinfo(address) }

        let fileDescriptor = socket(
            address.pointee.ai_family,
            address.pointee.ai_socktype,
            address.pointee.ai_protocol
        )
        guard fileDescriptor >= 0 else { throw currentPOSIXError() }
        do {
            guard bind(fileDescriptor, address.pointee.ai_addr, address.pointee.ai_addrlen) == 0,
                listen(fileDescriptor, 1) == 0
            else {
                throw currentPOSIXError()
            }
            var boundAddress = sockaddr_storage()
            var boundAddressLength = socklen_t(MemoryLayout<sockaddr_storage>.size)
            let boundPort = try withUnsafeMutablePointer(to: &boundAddress) { pointer in
                try pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                    guard getsockname(fileDescriptor, socketAddress, &boundAddressLength) == 0
                    else { throw currentPOSIXError() }
                    if ipv4 {
                        return socketAddress.withMemoryRebound(to: sockaddr_in.self, capacity: 1) {
                            Int(in_port_t(bigEndian: $0.pointee.sin_port))
                        }
                    }
                    return socketAddress.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) {
                        Int(in_port_t(bigEndian: $0.pointee.sin6_port))
                    }
                }
            }
            self.fileDescriptor = fileDescriptor
            self.port = boundPort
        } catch {
            Darwin.close(fileDescriptor)
            throw error
        }
    }

    func close() {
        Darwin.close(self.fileDescriptor)
    }
}

private func currentPOSIXError() -> POSIXError {
    POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
}
