import ArgumentParser
import Foundation
import Testing

@testable import wendy

@Suite("AgentConnectionOptions")
struct AgentConnectionOptionsTests {
    @Test("Endpoint init with host only")
    func testEndpointInitWithHostOnly() {
        let endpoint = TargetOptions.Endpoint(argument: "example.com")
        #expect(endpoint?.remote == .grpc(host: "example.com", port: 50051))
    }

    @Test("Endpoint init with host and port")
    func testEndpointInitWithHostAndPort() {
        let endpoint = TargetOptions.Endpoint(argument: "example.com:8080")
        #expect(endpoint?.remote == .grpc(host: "example.com", port: 8080))
    }

    @Test("Endpoint init fails with non-wendy URL scheme")
    func testEndpointInitFailsWithNonWendyURLScheme() {
        let endpoint = TargetOptions.Endpoint(argument: "https://example.com:8080")
        #expect(endpoint == nil)

        let httpEndpoint = TargetOptions.Endpoint(argument: "http://example.com:8080")
        #expect(httpEndpoint == nil)

        let ftpEndpoint = TargetOptions.Endpoint(argument: "ftp://example.com:8080")
        #expect(ftpEndpoint == nil)
    }

    @Test("Endpoint init with wendy scheme")
    func testEndpointInitWithWendyScheme() {
        let endpoint = TargetOptions.Endpoint(argument: "wendy://example.com:9000")
        #expect(endpoint?.remote == .grpc(host: "example.com", port: 9000))
    }

    @Test("Endpoint init with localhost")
    func testEndpointInitWithLocalhost() {
        let endpoint = TargetOptions.Endpoint(argument: "localhost")
        #expect(endpoint?.remote == .grpc(host: "localhost", port: 50051))
    }

    @Test("Endpoint init with IPv4 address")
    func testEndpointInitWithIPv4() {
        let endpoint = TargetOptions.Endpoint(argument: "127.0.0.1:5000")
        #expect(endpoint?.remote == .grpc(host: "127.0.0.1", port: 50051))
    }

    @Test("Endpoint init with IPv6 address")
    func testEndpointInitWithIPv6() {
        // Standard IPv6 format
        let endpoint = TargetOptions.Endpoint(argument: "[::1]:5000")
        #expect(endpoint?.remote == .grpc(host: "::1", port: 5000))

        // IPv6 localhost
        let localhostIPv6 = TargetOptions.Endpoint(argument: "[::1]")
        #expect(localhostIPv6?.remote == .grpc(host: "::1", port: 50051))

        // Full IPv6 address
        let fullIPv6 = TargetOptions.Endpoint(
            argument: "[2001:db8:85a3:8d3:1319:8a2e:370:7348]:443"
        )
        #expect(
            fullIPv6?.remote
                == .grpc(host: "2001:db8:85a3:8d3:1319:8a2e:370:7348", port: 443)
        )

        // IPv6 with wendy:// scheme
        let schemeIPv6 = TargetOptions.Endpoint(argument: "wendy://[2001:db8::1]:8888")
        #expect(schemeIPv6?.remote == .grpc(host: "2001:db8::1", port: 8888))
    }

    @Test("Endpoint init fails with empty string")
    func testEndpointInitFailsWithEmptyString() {
        let endpoint = TargetOptions.Endpoint(argument: "")
        #expect(endpoint == nil)
    }

    @Test("Endpoint init fails with invalid host")
    func testEndpointInitFailsWithInvalidHost() {
        let endpoint = TargetOptions.Endpoint(argument: ":8080")
        #expect(endpoint == nil)
    }

    @Test("Endpoint description")
    func testEndpointDescription() {
        let endpoint = TargetOptions.Endpoint(argument: "example.com:8080")
        #expect(endpoint?.description == "example.com:8080")
    }

    @Test("AgentConnectionOptions parsing with --device")
    func testAgentConnectionOptionsParsingWithDevice() throws {
        struct TestCommand: ParsableCommand {
            @OptionGroup var target: TargetOptions

            mutating func run() {}
        }

        let command = try TestCommand.parse([
            "--device", "test.server.com:9000",
        ])

        #expect(command.target.device?.remote == .grpc(host: "test.server.com", port: 9000))
    }

}
