import XCTest
@testable import WendySpatial

@MainActor
final class SpatialDocumentTests: XCTestCase {
    func testPickerCancelFinishesOnceAndCannotLaterImport() {
        var results: [URL?] = []
        let coordinator = SpatialDocumentPicker.Coordinator { results.append($0) }
        coordinator.finish(nil)
        coordinator.finish(URL(fileURLWithPath: "/late.usdz"))
        XCTAssertEqual(results.count, 1)
        XCTAssertNil(results[0])
    }
    func testFailedSaveReportsFailureWithoutDiscardingExistingTwin() async {
        let model = SpatialModel()
        let original = SpatialSite(name: "Existing", modelURL: URL(fileURLWithPath: "/existing.usdz"), metresPerUnit: 1, cameras: [])
        model.site = original
        let saved = await model.saveScan(name: "Empty scan")
        XCTAssertFalse(saved)
        XCTAssertEqual(model.site?.modelURL, original.modelURL)
        XCTAssertNotNil(model.error)
        XCTAssertTrue(model.roomStatus?.hasPrefix("Could not save") == true)
        XCTAssertNil(model.roomOperation)
    }
    func testImportReportsSuccessAndExportWorksAfterRestoringRoom() async throws {
        let folder = try SpatialSite.importFolder(), manifest = folder.appendingPathComponent("current.json")
        let previous = try? Data(contentsOf: manifest)
        defer {
            if let previous { try? previous.write(to: manifest) }
            else { try? FileManager.default.removeItem(at: manifest) }
        }
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try SpatialUSDZ.data(chunks: [.init(vertices: [[0,0,0],[2,0,0],[0,0,2]], indices: [0,1,2])]).write(to: source)
        defer { try? FileManager.default.removeItem(at: source) }
        let model = SpatialModel()
        let result = await model.importFile(source)
        guard case .room = result else { return XCTFail(model.error ?? "Import did not return a room") }
        let imported = try XCTUnwrap(model.site)
        defer { try? FileManager.default.removeItem(at: imported.modelURL) }
        XCTAssertNotNil(model.roomStatus)
        XCTAssertNil(model.roomOperation)
        let restored = SpatialModel()
        restored.site = try SpatialSite.restore()
        XCTAssertNil(restored.scanExportURL)
        let exported = await restored.prepareTwinExport()
        let archive = try XCTUnwrap(exported)
        defer { try? FileManager.default.removeItem(at: archive.deletingLastPathComponent()) }
        XCTAssertEqual(archive.deletingPathExtension().lastPathComponent, imported.name)
        let reopened = try SpatialSite.load(archive)
        defer { try? FileManager.default.removeItem(at: reopened.modelURL) }
        XCTAssertEqual(reopened.name, imported.name)
    }
    func testFailedImportKeepsCurrentRoomAndReportsFailure() async throws {
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathExtension("usdz")
        try Data("not a model".utf8).write(to: source)
        defer { try? FileManager.default.removeItem(at: source) }
        let model = SpatialModel()
        model.site = SpatialSite(name: "Existing", modelURL: URL(fileURLWithPath: "/existing.usdz"), metresPerUnit: 1, cameras: [])
        let result = await model.importFile(source)
        XCTAssertNil(result)
        XCTAssertEqual(model.site?.name, "Existing")
        XCTAssertTrue(model.roomStatus?.hasPrefix("Import failed") == true)
        XCTAssertNil(model.roomOperation)
    }
    func testCoordinatedImportRejectsOversizedFiles() throws {
        let source = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data(repeating: 0, count: 8).write(to: source)
        defer { try? FileManager.default.removeItem(at: source) }
        XCTAssertThrowsError(try SpatialFileImport.read(source, maximumBytes: 4))
        XCTAssertEqual(try SpatialFileImport.read(source, maximumBytes: 8).count, 8)
    }
}
