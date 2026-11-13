import Testing

@testable import WendyAgentCore

@Suite("TunnelBrokerClient")
struct TunnelBrokerClientTests {
    private actor Collector<Element: Sendable> {
        var items: [Element] = []
        func add(_ element: Element) { self.items.append(element) }
    }

    private actor Flag {
        var value = false
        func set() { self.value = true }
    }

    // MARK: - brokerURL

    @Test("brokerURL prefers the override")
    func brokerURLOverride() {
        #expect(
            TunnelBrokerClient.brokerURL(cloudHost: "cloud.example:443", override: "b.example:9999")
                == "b.example:9999"
        )
    }

    @Test("brokerURL keeps a :443 cloud host verbatim, else uses :50052")
    func brokerURLDerivation() {
        #expect(
            TunnelBrokerClient.brokerURL(cloudHost: "cloud.example:443", override: nil)
                == "cloud.example:443"
        )
        #expect(
            TunnelBrokerClient.brokerURL(cloudHost: "cloud.example:50051", override: nil)
                == "cloud.example:50052"
        )
        #expect(
            TunnelBrokerClient.brokerURL(cloudHost: "cloud.example", override: nil)
                == "cloud.example:50052"
        )
        #expect(
            TunnelBrokerClient.brokerURL(cloudHost: "cloud.example:443", override: "")
                == "cloud.example:443"
        )
    }

    @Test("splitHostPort parses a trailing numeric port only")
    func splitHostPort() {
        #expect(TunnelBrokerClient.splitHostPort("host:443")?.host == "host")
        #expect(TunnelBrokerClient.splitHostPort("host:443")?.port == 443)
        #expect(TunnelBrokerClient.splitHostPort("1.2.3.4:50052")?.port == 50052)
        #expect(TunnelBrokerClient.splitHostPort("host") == nil)
        #expect(TunnelBrokerClient.splitHostPort("host:") == nil)
    }

    // MARK: - identity header

    @Test("identityHeader formats the wendy asset URN")
    func identityHeader() {
        #expect(
            TunnelBrokerClient.identityHeader(orgID: 2, assetID: 281)
                == "URI=urn:wendy:org:2:asset:281"
        )
    }

    // MARK: - SSRF guard

    @Test("isLoopback accepts only loopback targets")
    func isLoopback() {
        #expect(TunnelBrokerClient.isLoopback("localhost"))
        #expect(TunnelBrokerClient.isLoopback("127.0.0.1"))
        #expect(TunnelBrokerClient.isLoopback("127.99.5.4"))
        #expect(TunnelBrokerClient.isLoopback("::1"))
        #expect(!TunnelBrokerClient.isLoopback("10.0.0.1"))
        #expect(!TunnelBrokerClient.isLoopback("192.168.1.5"))
        #expect(!TunnelBrokerClient.isLoopback("example.com"))
        #expect(!TunnelBrokerClient.isLoopback("127"))
        #expect(!TunnelBrokerClient.isLoopback("127.0.0"))
    }

    // MARK: - port remap

    @Test("remapPort redirects the well-known mTLS port to the actual one")
    func remapPort() {
        #expect(TunnelBrokerClient.remapPort(requested: 50052, mtlsPort: 50053) == 50053)
        #expect(TunnelBrokerClient.remapPort(requested: 50052, mtlsPort: 50052) == 50052)
        #expect(TunnelBrokerClient.remapPort(requested: 8080, mtlsPort: 50053) == 8080)
        #expect(TunnelBrokerClient.remapPort(requested: 50052, mtlsPort: 0) == 50052)
    }

    // MARK: - backoff

    @Test("backoff doubles and caps at 90s")
    func backoff() {
        #expect(TunnelBrokerClient.backoff(attempt: 0) == .seconds(1))
        #expect(TunnelBrokerClient.backoff(attempt: 1) == .seconds(2))
        #expect(TunnelBrokerClient.backoff(attempt: 2) == .seconds(4))
        #expect(TunnelBrokerClient.backoff(attempt: 6) == .seconds(64))
        #expect(TunnelBrokerClient.backoff(attempt: 7) == .seconds(90))
        #expect(TunnelBrokerClient.backoff(attempt: 20) == .seconds(90))
        #expect(TunnelBrokerClient.backoff(attempt: 100) == .seconds(90))
    }

    // MARK: - relay pumps

    @Test("pumpLocalToGRPC emits join, one data per non-empty chunk, then half-close")
    func pumpLocalToGRPC() async {
        let (stream, continuation) = AsyncStream.makeStream(of: [UInt8].self)
        continuation.yield([1, 2, 3])
        continuation.yield([])  // empty — must be skipped
        continuation.yield([4, 5])
        continuation.finish()

        let sent = Collector<TunnelBrokerClient.TunnelOutbound>()
        await TunnelBrokerClient.pumpLocalToGRPC(
            sessionID: "sess-1",
            inbound: stream,
            bytes: { $0 }
        ) { await sent.add($0) }

        #expect(
            await sent.items == [
                .join(sessionID: "sess-1"),
                .data([1, 2, 3]),
                .data([4, 5]),
                .halfClose,
            ]
        )
    }

    @Test("pumpGRPCToLocal writes payloads, skips empties, and stops at half-close")
    func pumpGRPCToLocal() async {
        let (stream, continuation) = AsyncStream.makeStream(
            of: TunnelBrokerClient.TunnelInbound.self
        )
        continuation.yield(.init(payload: [1, 2], halfClose: false))
        continuation.yield(.init(payload: [], halfClose: false))  // empty — no write
        continuation.yield(.init(payload: [], halfClose: true))  // stops the loop
        continuation.yield(.init(payload: [9], halfClose: false))  // ignored — loop broke
        continuation.finish()

        let writes = Collector<[UInt8]>()
        let finished = Flag()
        await TunnelBrokerClient.pumpGRPCToLocal(
            messages: stream,
            frame: { $0 },
            write: { await writes.add($0) },
            finishWrite: { await finished.set() }
        )

        #expect(await writes.items == [[1, 2]])
        #expect(await finished.value)
    }
}
