import XCTest
@testable import WendySpatial

final class SpatialSiteTests: XCTestCase {
    func testSharedV2ImportSurvivesProviderDismissal() throws {
        let site = Site(name: "iPhone room", metresPerModelUnit: 0.01)
        let model = try SpatialUSDZ.data(chunks: [.init(vertices: [[0, 0, 0], [2, 0, 0], [0, 0, 2]], indices: [0, 1, 2])])
        let references = Data("{\"reference\":\"preserved\"}".utf8)
        let bytes = try SiteArchivePayload(site: site, model: model, visualMap: references).data()
        // The iOS share provider may identify this as generic data.
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathExtension("data")
        try bytes.write(to: source)
        let staged = try SpatialFileImport.stage(source)
        try FileManager.default.removeItem(at: source)
        defer { try? FileManager.default.removeItem(at: staged.deletingLastPathComponent()) }
        let imported = try SpatialSite.load(staged)
        defer {
            try? FileManager.default.removeItem(at: imported.modelURL)
            if let archive = imported.sourceArchiveURL { try? FileManager.default.removeItem(at: archive) }
        }
        XCTAssertEqual(imported.name, "iPhone room")
        XCTAssertEqual(imported.metresPerUnit, 0.01)
        let exported = try imported.writeArchive()
        defer { try? FileManager.default.removeItem(at: exported.deletingLastPathComponent()) }
        let restored = try SiteArchivePayload(data: Data(contentsOf: exported))
        XCTAssertEqual(restored.model, model)
        XCTAssertEqual(restored.visualMap, references)
        XCTAssertEqual(restored.site.modelRevision, site.modelRevision)
    }

    func testArchiveRetainsMetricScaleAndCameraIdentity() throws {
        let site = Site(name: "Lab", metresPerModelUnit: 0.01,
            cameras: [.init(gatewayID: "tenant:gateway", sourceID: "camera:stable", name: "Bench", position: .init(x: 2, y: 1, z: 3))])
        let manifest = try JSONEncoder().encode(site)
        var data = Data("WENDYST1".utf8)
        let size = UInt32(manifest.count)
        data.append(contentsOf: [UInt8(size >> 24), UInt8((size >> 16) & 255), UInt8((size >> 8) & 255), UInt8(size & 255)])
        data.append(manifest)
        data.append(contentsOf: [0x50, 0x4b, 0x03, 0x04, 0])
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathExtension("wendysite")
        try data.write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }
        let imported = try SpatialSite.load(url)
        defer { try? FileManager.default.removeItem(at: imported.modelURL) }
        XCTAssertEqual(imported.name, "Lab")
        XCTAssertEqual(imported.metresPerUnit, 0.01)
        XCTAssertEqual(imported.cameras.first?.sourceID, "camera:stable")
        XCTAssertEqual(imported.cameras.first?.position?.x, 2)
    }
    func testTruncatedAndUnboundedArchiveLengthsFailBeforeIndexing() throws {
        for bytes in [Data(), Data("WENDYST2".utf8) + Data(repeating: 255, count: 12), Data("WENDYST1".utf8) + Data(repeating: 1, count: 9)] {
            let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathExtension("wendysite")
            try bytes.write(to: url)
            defer { try? FileManager.default.removeItem(at: url) }
            XCTAssertThrowsError(try SpatialSite.load(url))
        }
    }
}
