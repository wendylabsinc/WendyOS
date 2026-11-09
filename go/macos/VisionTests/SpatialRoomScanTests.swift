import XCTest
import RealityKit
@testable import WendySpatial

final class SpatialRoomScanTests: XCTestCase {
    private let triangle = SpatialMeshChunk(vertices: [[0, 0, 0], [2, 0, 0], [0, 0, 3]], indices: [0, 1, 2])

    func testRejectsInvalidGeometryBeforeExport() {
        for chunk in [SpatialMeshChunk(vertices: [[0, 0, 0]], indices: [0, 1, 2]),
                      SpatialMeshChunk(vertices: [[.nan, 0, 0]], indices: [0, 0, 0]),
                      SpatialMeshChunk(vertices: [[0, 0, 0]], indices: [0, 0])] {
            XCTAssertThrowsError(try SpatialUSDZ.data(chunks: [chunk]))
        }
    }

    @MainActor func testUSDZLoadsAtMetricScaleAndRoundTripsArchive() async throws {
        let data = try SpatialUSDZ.data(chunks: [triangle])
        let folder = try SpatialSite.importFolder()
        let url = folder.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try data.write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }
        let nameLength = Int(data[26]) + Int(data[27]) * 256
        let extraLength = Int(data[28]) + Int(data[29]) * 256
        XCTAssertEqual((30 + nameLength + extraLength) % 64, 0)
        let entity = try await Entity(contentsOf: url)
        let bounds = entity.visualBounds(relativeTo: nil)
        XCTAssertEqual(bounds.extents.x, 2, accuracy: 0.001)
        XCTAssertEqual(bounds.extents.z, 3, accuracy: 0.001)
        let site = SpatialSite(name: "Captured room", modelURL: url, metresPerUnit: 1, cameras: [])
        let archive = try site.writeArchive()
        defer { try? FileManager.default.removeItem(at: archive) }
        let restored = try SpatialSite.load(archive)
        defer { try? FileManager.default.removeItem(at: restored.modelURL) }
        XCTAssertEqual(restored.name, site.name)
        XCTAssertEqual(restored.metresPerUnit, 1)
        XCTAssertEqual(try Data(contentsOf: restored.modelURL), data)
    }

    func testPersistedRoomRebasesIntoCurrentAppContainer() throws {
        let folder = try SpatialSite.importFolder(), manifest = folder.appendingPathComponent("current.json")
        let previous = try? Data(contentsOf: manifest)
        defer {
            if let previous { try? previous.write(to: manifest) }
            else { try? FileManager.default.removeItem(at: manifest) }
        }
        let url = folder.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try SpatialUSDZ.data(chunks: [triangle]).write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }
        let old = SpatialSite(name: "Room", modelURL: URL(fileURLWithPath: "/old/container/SpatialImports/" + url.lastPathComponent), metresPerUnit: 1, cameras: [])
        try JSONEncoder().encode(old).write(to: manifest)
        let restored = try XCTUnwrap(SpatialSite.restore())
        XCTAssertEqual(restored.modelURL, url)
        XCTAssertEqual(restored.name, "Room")
    }
}
