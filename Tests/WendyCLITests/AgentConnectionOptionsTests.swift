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

    // MARK: - IPv6 Link-Local with Scope ID

    @Test("Endpoint init with bracketed IPv6 link-local and scope ID")
    func testEndpointInitWithBracketedIPv6LinkLocalScopeID() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "[fe80::1%eth0]:9000")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::1%eth0")
        #expect(endpoint?.port == 9000)
    }

    @Test("Endpoint init with bracketed IPv6 link-local, scope ID, default port")
    func testEndpointInitWithBracketedIPv6LinkLocalScopeIDDefaultPort() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "[fe80::1%usb0]")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::1%usb0")
        #expect(endpoint?.port == 50051)
    }

    @Test("Endpoint init with URL-encoded scope ID (%25)")
    func testEndpointInitWithURLEncodedScopeID() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "[fe80::1%25eth0]:8080")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::1%eth0")
        #expect(endpoint?.port == 8080)
    }

    @Test("Endpoint init with bare IPv6 link-local and scope ID")
    func testEndpointInitWithBareIPv6LinkLocalScopeID() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "fe80::dead:beef%usb0")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::dead:beef%usb0")
        #expect(endpoint?.port == 50051)
    }

    @Test("Endpoint init with bare IPv6 link-local, URL-encoded scope ID")
    func testEndpointInitWithBareIPv6URLEncodedScopeID() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "fe80::1%25enp0s3")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::1%enp0s3")
        #expect(endpoint?.port == 50051)
    }

    @Test("Endpoint init with wendy scheme and IPv6 link-local scope ID")
    func testEndpointInitWithWendySchemeIPv6ScopeID() {
        let endpoint = AgentConnectionOptions.Endpoint(argument: "wendy://[fe80::1%eth0]:9000")
        #expect(endpoint != nil)
        #expect(endpoint?.host == "fe80::1%eth0")
        #expect(endpoint?.port == 9000)
    }

    // MARK: - Endpoint Computed Properties

    @Test("isIPv6LinkLocal returns true for fe80:: addresses")
    func testIsIPv6LinkLocal() {
        let linkLocal = AgentConnectionOptions.Endpoint(host: "fe80::1%eth0", port: 50051)
        #expect(linkLocal.isIPv6LinkLocal)

        let linkLocalNoScope = AgentConnectionOptions.Endpoint(host: "fe80::dead:beef", port: 50051)
        #expect(linkLocalNoScope.isIPv6LinkLocal)

        // Case insensitive
        let upperCase = AgentConnectionOptions.Endpoint(host: "FE80::1", port: 50051)
        #expect(upperCase.isIPv6LinkLocal)
    }

    @Test("isIPv6LinkLocal returns false for non-link-local addresses")
    func testIsIPv6LinkLocalFalse() {
        let globalIPv6 = AgentConnectionOptions.Endpoint(host: "2001:db8::1", port: 50051)
        #expect(!globalIPv6.isIPv6LinkLocal)

        let ipv4 = AgentConnectionOptions.Endpoint(host: "192.168.1.1", port: 50051)
        #expect(!ipv4.isIPv6LinkLocal)

        let hostname = AgentConnectionOptions.Endpoint(host: "device.local", port: 50051)
        #expect(!hostname.isIPv6LinkLocal)
    }

    @Test("scopeID extracts interface name from scoped address")
    func testScopeID() {
        let scoped = AgentConnectionOptions.Endpoint(host: "fe80::1%eth0", port: 50051)
        #expect(scoped.scopeID == "eth0")

        let longInterface = AgentConnectionOptions.Endpoint(host: "fe80::1%enp0s31f6", port: 50051)
        #expect(longInterface.scopeID == "enp0s31f6")
    }

    @Test("scopeID returns nil when no scope present")
    func testScopeIDNil() {
        let noScope = AgentConnectionOptions.Endpoint(host: "fe80::1", port: 50051)
        #expect(noScope.scopeID == nil)

        let ipv4 = AgentConnectionOptions.Endpoint(host: "192.168.1.1", port: 50051)
        #expect(ipv4.scopeID == nil)
    }

    @Test("hostWithoutScope strips the scope ID suffix")
    func testHostWithoutScope() {
        let scoped = AgentConnectionOptions.Endpoint(host: "fe80::1%eth0", port: 50051)
        #expect(scoped.hostWithoutScope == "fe80::1")

        let noScope = AgentConnectionOptions.Endpoint(host: "fe80::1", port: 50051)
        #expect(noScope.hostWithoutScope == "fe80::1")

        let ipv4 = AgentConnectionOptions.Endpoint(host: "192.168.1.1", port: 50051)
        #expect(ipv4.hostWithoutScope == "192.168.1.1")
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

    @Test("AgentConnectionOptions parsing with --device IPv6 link-local")
    func testAgentConnectionOptionsParsingWithDeviceIPv6LinkLocal() throws {
        struct TestCommand: ParsableCommand {
            @OptionGroup var agentConnectionOptions: AgentConnectionOptions

            mutating func run() {}
        }

        let command = try TestCommand.parse([
            "--device", "[fe80::1%eth0]:9000",
        ])

        #expect(command.agentConnectionOptions.device?.host == "fe80::1%eth0")
        #expect(command.agentConnectionOptions.device?.port == 9000)
        #expect(command.agentConnectionOptions.device?.isIPv6LinkLocal == true)
        #expect(command.agentConnectionOptions.device?.scopeID == "eth0")
    }

}
