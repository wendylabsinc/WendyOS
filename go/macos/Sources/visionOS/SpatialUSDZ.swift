import Foundation

/// A self-contained, uncompressed USD package. The single USDA member starts
/// on a 64-byte boundary, as required by USDZ; no paths or external assets enter it.
enum SpatialUSDZ {
    static func data(chunks: [SpatialMeshChunk]) throws -> Data {
        guard !chunks.isEmpty, chunks.count <= 1024,
              chunks.reduce(0, { $0 + $1.triangles }) <= 300_000 else { throw SiteError.tooLarge }
        var text = "#usda 1.0\n(\n    defaultPrim = \"Room\"\n    metersPerUnit = 1\n    upAxis = \"Y\"\n)\ndef Xform \"Room\" {\n"
        for (index, chunk) in chunks.enumerated() {
            try chunk.validate()
            text += "def Mesh \"Surface\(index)\" {\n uniform token subdivisionScheme = \"none\"\n bool doubleSided = true\n point3f[] points = ["
            text += chunk.vertices.map { "(\($0.x),\($0.y),\($0.z))" }.joined(separator: ",")
            text += "]\n int[] faceVertexCounts = [" + Array(repeating: "3", count: chunk.triangles).joined(separator: ",")
            text += "]\n int[] faceVertexIndices = [" + chunk.indices.map(String.init).joined(separator: ",")
            text += "]\n color3f[] primvars:displayColor = [(0.65,0.70,0.72)] (interpolation = \"constant\")\n}\n"
            guard text.utf8.count <= 64 * 1024 * 1024 else { throw SiteError.tooLarge }
        }
        text += "}\n"
        return package(Data(text.utf8))
    }
    private static let crcTable: [UInt32] = (0..<256).map { value in
        var c = UInt32(value)
        for _ in 0..<8 { c = c & 1 != 0 ? 0xedb88320 ^ (c >> 1) : c >> 1 }
        return c
    }
    private static func package(_ data: Data) -> Data {
        let name = Data("room.usda".utf8)
        let crc = data.reduce(UInt32.max) { crcTable[Int(($0 ^ UInt32($1)) & 255)] ^ ($0 >> 8) } ^ UInt32.max
        var output = Data()
        func u16(_ value: UInt16) { var v = value.littleEndian; withUnsafeBytes(of: &v) { output.append(contentsOf: $0) } }
        func u32(_ value: UInt32) { var v = value.littleEndian; withUnsafeBytes(of: &v) { output.append(contentsOf: $0) } }
        let padding = (64 - ((30 + name.count + 4) % 64)) % 64
        u32(0x04034b50); u16(20); u16(0x800); u16(0); u16(0); u16(0)
        u32(crc); u32(UInt32(data.count)); u32(UInt32(data.count)); u16(UInt16(name.count)); u16(UInt16(padding + 4))
        output.append(name); u16(0x1986); u16(UInt16(padding)); output.append(Data(repeating: 0, count: padding))
        output.append(data)
        let central = output.count
        u32(0x02014b50); u16(20); u16(20); u16(0x800); u16(0); u16(0); u16(0)
        u32(crc); u32(UInt32(data.count)); u32(UInt32(data.count)); u16(UInt16(name.count)); u16(0); u16(0)
        u16(0); u16(0); u32(0); u32(0); output.append(name)
        let centralSize = output.count - central
        u32(0x06054b50); u16(0); u16(0); u16(1); u16(1); u32(UInt32(centralSize)); u32(UInt32(central)); u16(0)
        return output
    }
}
