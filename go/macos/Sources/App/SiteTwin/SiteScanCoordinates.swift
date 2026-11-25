import Foundation
import SceneKit
import simd

/// RoomPlan's USDZ export may change coordinate conventions. Accept a reference
/// map only when identified exported meshes retain the AR scan transforms.
enum SiteScanCoordinates {
    static func verify(model: URL, transforms: [UUID: simd_float4x4]) throws {
        let data = try Data(contentsOf: model)
        let names = try meshIdentities(data)
        let scene = try SCNScene(url: model, options: [.convertToYUp: true, .convertUnitsToMeters: 1.0])
        var checked = Set<UUID>()
        var mismatch = false
        scene.rootNode.enumerateChildNodes { node, _ in
            guard node.geometry != nil, let name = node.name, let id = names[name], let expected = transforms[id] else { return }
            let actual = node.simdWorldTransform
            for column in 0..<4 {
                if simd_length(actual[column] - expected[column]) > 0.01 { mismatch = true }
            }
            checked.insert(id)
        }
        guard !mismatch, checked.count >= 3 else { throw SiteLocalizationError.coordinateMismatch }
    }

    // USDZ is an uncompressed ZIP. Read bounded members in memory, never extract
    // paths or follow links. Reject unsupported archive variants.
    static func meshIdentities(_ data: Data) throws -> [String: UUID] {
        func fail() -> SiteLocalizationError { .coordinateMismatch }
        guard data.count >= 22, data.count <= SiteStore.maximumModelBytes else { throw fail() }
        func u16(_ i: Int) -> Int { Int(data[i]) | Int(data[i+1]) << 8 }
        func u32(_ i: Int) -> Int { u16(i) | u16(i+2) << 16 }
        var end: Int?
        for i in stride(from: data.count-22, through: max(0, data.count-65557), by: -1) {
            if u32(i) == 0x06054b50, i+22+u16(i+20) == data.count { end = i; break }
        }
        guard let end, u16(end+4) == 0, u16(end+6) == 0,
              u16(end+8) == u16(end+10), (1...2048).contains(u16(end+10)) else { throw fail() }
        let directoryEnd = u32(end+16)+u32(end+12)
        var cursor = u32(end+16), total = 0
        guard directoryEnd == end, cursor >= 0 else { throw fail() }
        let uuidPattern = try NSRegularExpression(pattern: #"string UUID = "([0-9A-Fa-f-]+)""#)
        let meshPattern = try NSRegularExpression(pattern: #"def Mesh "([^"]+)""#)
        var result: [String: UUID] = [:]
        for _ in 0..<u16(end+10) {
            guard cursor+46 <= directoryEnd, u32(cursor) == 0x02014b50 else { throw fail() }
            let size = u32(cursor+24), nameLength = u16(cursor+28), extra = u16(cursor+30), comment = u16(cursor+32)
            let next = cursor+46+nameLength+extra+comment
            guard next <= directoryEnd, u16(cursor+8) & 1 == 0, u16(cursor+10) == 0,
                  u32(cursor+20) == size else { throw fail() }
            let name = String(data: data.subdata(in: cursor+46..<cursor+46+nameLength), encoding: .utf8) ?? ""
            if name.hasSuffix(".usda") {
                total += size
                let local = u32(cursor+42)
                guard total <= 16*1024*1024, local+30 <= u32(end+16), u32(local) == 0x04034b50 else { throw fail() }
                let start = local+30+u16(local+26)+u16(local+28)
                guard start+size <= u32(end+16), let text = String(data: data.subdata(in: start..<start+size), encoding: .utf8) else { throw fail() }
                let ns = text as NSString, range = NSRange(location: 0, length: ns.length)
                if let idMatch = uuidPattern.firstMatch(in: text, range: range),
                   let mesh = meshPattern.firstMatch(in: text, range: range),
                   let id = UUID(uuidString: ns.substring(with: idMatch.range(at: 1))) {
                    let meshName = ns.substring(with: mesh.range(at: 1))
                    guard result[meshName] == nil else { throw fail() }
                    result[meshName] = id
                }
            }
            cursor = next
        }
        guard cursor == directoryEnd else { throw fail() }
        return result
    }
}
