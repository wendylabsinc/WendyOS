import Foundation
import GRPCCore
import Testing
import X509

@testable import WendyAgentCore

@Suite("Wendy Mesh")
struct WendyMeshTests {
    @Test("device IDs map to stable 10.99/16 addresses")
    func addressPlan() {
        #expect(WendyMeshAddressPlan.addressString(for: 1) == "10.99.0.1")
        #expect(WendyMeshAddressPlan.addressString(for: 513) == "10.99.2.1")
        #expect(WendyMeshAddressPlan.deviceID(for: "10.99.2.1") == 513)
        #expect(WendyMeshAddressPlan.addressString(for: 0) == nil)
        #expect(WendyMeshAddressPlan.deviceID(for: "10.98.2.1") == nil)
    }

    @Test("directory and credentials use the extension wire format")
    func configurationCoding() throws {
        let directory = WendyMeshDirectory(devices: [
            WendyMeshDevice(assetID: 42, name: "camera", organizationID: 7, online: true)
        ])
        #expect(try WendyMeshDirectory.decode(WendyMeshDirectory.encode(directory)) == directory)

        let credentials = WendyCloudCredentials(
            pemCertificate: "cert",
            pemCertificateChain: "chain",
            pemPrivateKey: "key",
            organizationID: 7,
            userID: "user"
        )
        let object = try #require(
            JSONSerialization.jsonObject(with: JSONEncoder().encode(credentials)) as? [String: Any]
        )
        #expect(object["pem_certificate"] as? String == "cert")
        #expect(object["organization_id"] as? Int == 7)
        #expect(credentials.debugDescription == "WendyCloudCredentials(<redacted>)")
    }

    @Test("certificate proof binds the user identity and RPC method")
    func certificateProof() throws {
        let ca = try TestPKI.makeCA()
        let identity = try TestPKI.makeIdentity(commonName: "proof-test", ca: ca)
        let credentials = WendyCloudCredentials(
            pemCertificate: identity.certPEM,
            pemCertificateChain: ca.pem,
            pemPrivateKey: identity.keyPEM,
            organizationID: 7,
            userID: "user-a"
        )
        let metadata = try LegacyCertificateProofSigner(credentials: credentials).metadata(
            fullMethod: "/wendycloud.v1.TunnelBrokerService/ClientTunnel"
        )
        func value(_ key: String) throws -> String {
            try #require(Array(metadata[stringValues: key]).first)
        }

        let method = "wendycloud.v1.TunnelBrokerService/ClientTunnel"
        let uri = try value("x-wendy-certificate-uri")
        let serial = try value("x-wendy-certificate-serial")
        let timestamp = try value("x-wendy-certificate-timestamp")
        let nonce = try value("x-wendy-certificate-nonce")
        let signature = try #require(base64URLData(try value("x-wendy-certificate-signature")))
        #expect(uri == "wendy://user/user-a")
        #expect(serial.count >= 2)
        #expect(base64URLData(nonce)?.count == 16)

        let canonical = proofCanonical([method, uri, serial, timestamp, nonce])
        #expect(
            identity.certificate.publicKey.isValidSignature(
                signature,
                for: canonical,
                signatureAlgorithm: .ecdsaWithSHA256
            )
        )
        let otherMethod = proofCanonical([
            "wendycloud.v1.AssetService/ListAssets", uri, serial, timestamp, nonce,
        ])
        #expect(
            !identity.certificate.publicKey.isValidSignature(
                signature,
                for: otherMethod,
                signatureAlgorithm: .ecdsaWithSHA256
            )
        )
    }

    @Test("mesh DNS answers enrolled device names")
    func dnsAnswer() throws {
        let response = try #require(
            WendyMeshDNS.answer(dnsQuery("device-513.mesh.wendy.internal")) {
                WendyMeshAddressPlan.address(for: $0)
            }
        )
        #expect(Array(response.suffix(4)) == [10, 99, 2, 1])
        #expect(response[6] == 0)
        #expect(response[7] == 1)
    }

    @Test("legacy mesh DNS name remains resolvable")
    func legacyDNSName() throws {
        let response = try #require(
            WendyMeshDNS.answer(dnsQuery("device-513.cloud.wendy.dev")) {
                WendyMeshAddressPlan.address(for: $0)
            }
        )
        #expect(Array(response.suffix(4)) == [10, 99, 2, 1])
    }

    @Test("malformed and oversized DNS messages are rejected without trapping")
    func rejectsUnsafeDNSMessages() {
        var compressedQuestion = dnsQuery("device-1.mesh.wendy.internal")
        compressedQuestion[12] = 0xc0
        #expect(WendyMeshDNS.answer(compressedQuestion) { _ in (10, 99, 0, 1) } == nil)
        #expect(WendyMeshDNS.frameForTCP(Data(count: Int(UInt16.max) + 1)).isEmpty)
    }

    @Test("DNS-over-TCP framing preserves partial messages")
    func dnsTCPFraming() {
        let first = WendyMeshDNS.frameForTCP(Data([1, 2, 3]))
        let second = WendyMeshDNS.frameForTCP(Data([4, 5]))
        var buffer = first + second.prefix(3)
        #expect(WendyMeshDNS.extractTCPMessages(from: &buffer) == [Data([1, 2, 3])])
        buffer.append(contentsOf: second.dropFirst(3))
        #expect(WendyMeshDNS.extractTCPMessages(from: &buffer) == [Data([4, 5])])
        #expect(buffer.isEmpty)
    }

    @Test("malformed ICMP packets and replies are rejected without trapping")
    func rejectsUnsafeICMPPackets() {
        let truncated = Data([
            0x45, 0, 0, 32, 0, 0, 0x40, 0, 64, 1, 0, 0,
            192, 168, 1, 4, 10, 99, 0, 42,
            8, 0, 0, 0, 0x12, 0x34, 0, 9,
        ])
        #expect(WendyICMPv4.parseEchoRequest(truncated) == nil)

        let invalidAddress = WendyICMPv4.EchoRequest(
            sourceAddress: "invalid",
            destinationAddress: "10.99.0.42",
            identifier: 1,
            sequence: 1,
            payload: Data()
        )
        #expect(WendyICMPv4.makeEchoReply(to: invalidAddress, payload: Data()).isEmpty)
        #expect(
            WendyICMPv4.makeEchoReply(
                to: invalidAddress,
                payload: Data(count: Int(UInt16.max))
            ).isEmpty
        )
    }

    @Test("cloud endpoints handle defaults, ports, and IPv6 without ambiguous parsing")
    func cloudEndpoints() throws {
        let defaultEndpoint = try parseCloudEndpoint("cloud.wendy.dev")
        #expect(defaultEndpoint.host == "cloud.wendy.dev")
        #expect(defaultEndpoint.port == 443)

        let explicitEndpoint = try parseCloudEndpoint("localhost:50052")
        #expect(explicitEndpoint.host == "localhost")
        #expect(explicitEndpoint.port == 50052)

        let ipv6Endpoint = try parseCloudEndpoint("[::1]:50052")
        #expect(ipv6Endpoint.host == "::1")
        #expect(ipv6Endpoint.port == 50052)

        #expect(throws: (any Error).self) { try parseCloudEndpoint("localhost:0") }
        #expect(throws: (any Error).self) { try parseCloudEndpoint("::1") }
        #expect(throws: (any Error).self) { try parseCloudEndpoint("[::1]junk") }
    }

    @Test("ICMP echo replies reverse endpoints and retain echo fields")
    func icmpEcho() throws {
        var request = Data([
            0x45, 0, 0, 31, 0, 0, 0x40, 0, 64, 1, 0, 0,
            192, 168, 1, 4, 10, 99, 0, 42,
            8, 0, 0, 0, 0x12, 0x34, 0, 9,
        ])
        request.append(contentsOf: [1, 2, 3])
        let parsed = try #require(WendyICMPv4.parseEchoRequest(request))
        #expect(parsed.identifier == 0x1234)
        #expect(parsed.sequence == 9)

        let reply = WendyICMPv4.makeEchoReply(to: parsed, payload: parsed.payload)
        #expect(Array(reply[12...15]) == [10, 99, 0, 42])
        #expect(Array(reply[16...19]) == [192, 168, 1, 4])
        #expect(reply[20] == 0)
        #expect(Array(reply.suffix(3)) == [1, 2, 3])
    }

    private func proofCanonical(_ fields: [String]) -> Data {
        var result = Data()
        for field in ["wendy-legacy-certificate-proof/v1"] + fields {
            let bytes = Array(field.utf8)
            let length = UInt32(bytes.count)
            result.append(UInt8((length >> 24) & 0xff))
            result.append(UInt8((length >> 16) & 0xff))
            result.append(UInt8((length >> 8) & 0xff))
            result.append(UInt8(length & 0xff))
            result.append(contentsOf: bytes)
        }
        return result
    }

    private func base64URLData(_ encoded: String) -> Data? {
        var value = encoded.replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/")
        value += String(repeating: "=", count: (4 - value.count % 4) % 4)
        return Data(base64Encoded: value)
    }

    private func dnsQuery(_ name: String) -> Data {
        var data = Data([
            0x12, 0x34, 0x01, 0x00,
            0x00, 0x01, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x00,
        ])
        for label in name.split(separator: ".") {
            data.append(UInt8(label.utf8.count))
            data.append(contentsOf: label.utf8)
        }
        data.append(contentsOf: [0, 0, 1, 0, 1])
        return data
    }
}
