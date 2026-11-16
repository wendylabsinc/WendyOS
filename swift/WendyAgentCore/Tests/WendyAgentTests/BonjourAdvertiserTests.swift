import Foundation
import Testing

@testable import WendyAgentCore

@Suite("BonjourAdvertiser TXT")
struct BonjourAdvertiserTests {
    private func fields(_ data: Data) -> [String] {
        var out: [String] = []
        var i = data.startIndex
        while i < data.endIndex {
            let len = Int(data[i])
            let start = data.index(after: i)
            let end = data.index(start, offsetBy: len)
            out.append(String(decoding: data[start..<end], as: UTF8.self))
            i = end
        }
        return out
    }

    @Test("unprovisioned TXT carries tls=false and no assetid")
    func unprovisioned() {
        let data = BonjourAdvertiser.encodeTXT(
            displayName: "mac",
            deviceID: "mac",
            tls: false,
            assetID: nil,
            orgID: nil,
            caps: []
        )
        let f = fields(data)
        #expect(f.contains("displayname=mac"))
        #expect(f.contains("id=mac"))
        #expect(f.contains("tls=false"))
        #expect(!f.contains(where: { $0.hasPrefix("assetid=") }))
        #expect(!f.contains(where: { $0.hasPrefix("orgid=") }))
    }

    @Test("provisioned TXT carries tls=true, assetid, and orgid")
    func provisioned() {
        let data = BonjourAdvertiser.encodeTXT(
            displayName: "mac",
            deviceID: "mac",
            tls: true,
            assetID: 42,
            orgID: 2,
            caps: [],
            principalURI:
                "spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/device/4c386c9d-9351-47b1-b176-19099e7e0fa2"
        )
        let f = fields(data)
        #expect(
            f.contains(
                "principal=spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/device/4c386c9d-9351-47b1-b176-19099e7e0fa2"
            )
        )
        #expect(f.contains("tls=true"))
        #expect(f.contains("assetid=42"))
        #expect(f.contains("orgid=2"))
    }

    @Test("TXT includes caps when non-empty")
    func encodeTXTIncludesCaps() {
        let data = BonjourAdvertiser.encodeTXT(
            displayName: "d",
            deviceID: "id",
            tls: true,
            assetID: 5,
            orgID: 2,
            caps: ["sensors"]
        )
        let s = String(decoding: data, as: UTF8.self)
        #expect(s.contains("caps=sensors"))
    }

    @Test("TXT omits caps record when empty")
    func encodeTXTOmitsCapsWhenEmpty() {
        let data = BonjourAdvertiser.encodeTXT(
            displayName: "d",
            deviceID: "id",
            tls: true,
            assetID: 5,
            orgID: 2,
            caps: []
        )
        let f = fields(data)
        #expect(!f.contains(where: { $0.hasPrefix("caps=") }))
    }
}
