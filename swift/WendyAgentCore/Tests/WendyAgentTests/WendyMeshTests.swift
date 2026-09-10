import Foundation
import Testing

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
