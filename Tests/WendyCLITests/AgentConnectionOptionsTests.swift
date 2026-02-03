import ArgumentParser
import Foundation
import Testing

@testable import wendy

@Suite("AgentConnectionOptions")
struct AgentConnectionOptionsTests {
    @Test("Endpoint init with host only")
    func testEndpointInitWithHostOnly() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "example.com")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "example.com")
        #expect(endpoint?.port == 50051)  // Default port
    }

    @Test("Endpoint init with host and port")
    func testEndpointInitWithHostAndPort() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "example.com:8080")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "example.com")
        #expect(endpoint?.port == 8080)
    }

    @Test("Endpoint init fails with non-wendy URL scheme")
    func testEndpointInitFailsWithNonWendyURLScheme() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "https://example.com:8080")
        #expect(endpoint == nil)

        let httpEndpoint = AgentConnectionOptions.Endpoint(argument: "http://example.com:8080")
        #expect(httpEndpoint == nil)

        let ftpEndpoint = AgentConnectionOptions.Endpoint(argument: "ftp://example.com:8080")
        #expect(ftpEndpoint == nil)
    }

    @Test("Endpoint init with wendy scheme")
    func testEndpointInitWithWendyScheme() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "wendy://example.com:9000")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "example.com")
        #expect(endpoint?.port == 9000)
    }

    @Test("Endpoint init with localhost")
    func testEndpointInitWithLocalhost() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "localhost")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "localhost")
        #expect(endpoint?.port == 50051)
    }

    @Test("Endpoint init with IPv4 address")
    func testEndpointInitWithIPv4() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "127.0.0.1:5000")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "127.0.0.1")
        #expect(endpoint?.port == 5000)
    }

    @Test("Endpoint init with IPv6 address")
    func testEndpointInitWithIPv6() {
        // Standard IPv6 format
        let endpoint = AgentConnectionOptions.Endpoint(argument: "[::1]:5000")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "::1")
        #expect(endpoint?.port == 5000)

        // IPv6 localhost
        let localhostIPv6 = AgentConnectionOptions.Endpoint(argument: "[::1]")
        #expect(localhostIPv6 != nil)
        #expect(localhostIPv6?.host == "::1")
        #expect(localhostIPv6?.port == 50051)

        // Full IPv6 address
        let fullIPv6 = AgentConnectionOptions.Endpoint(
            argument: "[2001:db8:85a3:8d3:1319:8a2e:370:7348]:443"
        )
        #expect(fullIPv6 != nil)
        #expect(fullIPv6?.host == "2001:db8:85a3:8d3:1319:8a2e:370:7348")
        #expect(fullIPv6?.port == 443)

        // IPv6 with wendy:// scheme
        let schemeIPv6 = AgentConnectionOptions.Endpoint(argument: "wendy://[2001:db8::1]:8888")
        #expect(schemeIPv6 != nil)
        #expect(schemeIPv6?.host == "2001:db8::1")
        #expect(schemeIPv6?.port == 8888)
    }

    @Test("Endpoint init fails with empty string")
    func testEndpointInitFailsWithEmptyString() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "")
        #expect(endpoint == nil)
    }

    @Test("Endpoint init fails with invalid host")
    func testEndpointInitFailsWithInvalidHost() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: ":8080")
        #expect(endpoint == nil)
    }

    @Test("Endpoint description")
    func testEndpointDescription() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "example.com:8080")
        #expect(endpoint?.description == "example.com:8080")
    }

    @Test("AgentConnectionOptions parsing with --device")
    func testAgentConnectionOptionsParsingWithDevice() throws {
        struct TestCommand: ParsableCommand {
            @OptionGroup var agentConnectionOptions: AgentConnectionOptions

            mutating func run() {}
        }

        let command = try TestCommand.parse([
            "--device", "test.server.com:9000",
        ])

        #expect(command.agentConnectionOptions.device?.host == "test.server.com")
        #expect(command.agentConnectionOptions.device?.port == 9000)
    }

}
