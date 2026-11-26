import SceneKit
import XCTest
@testable import WorldTwin

final class SceneLabelCacheTests: XCTestCase {
    @MainActor
    func testRepeatedLabelsShareGeometryButKeepIndependentTransforms() throws {
        let cache = SceneLabelCache()
        let first = cache.label("Track 12", position: SCNVector3(1, 2, 3), size: 0.14)
        let second = cache.label("Track 12", position: SCNVector3(4, 5, 6), size: 0.25)
        XCTAssertFalse(first === second)
        XCTAssertEqual(first.childNodes.count, 2)
        XCTAssertEqual(second.childNodes.count, 2)
        XCTAssertFalse(first.childNodes[0] === second.childNodes[0])
        XCTAssertTrue(first.childNodes[0].geometry === second.childNodes[0].geometry)
        XCTAssertTrue(first.childNodes[1].geometry === second.childNodes[1].geometry)
        XCTAssertEqual((first.childNodes[1].geometry as? SCNText)?.string as? String, "Track 12")
        second.position.x = 100
        second.scale.x = 2
        XCTAssertEqual(first.position.x, 1)
        XCTAssertEqual(first.scale.x, 0.14, accuracy: 0.0001)
        XCTAssertEqual(second.position.y, 5)
        XCTAssertFalse(try XCTUnwrap(first.constraints?.first) === XCTUnwrap(second.constraints?.first))
        XCTAssertEqual(first.childNodes[0].renderingOrder, 100)
        XCTAssertEqual(first.childNodes[1].renderingOrder, 101)
        XCTAssertFalse(try XCTUnwrap(first.childNodes[1].geometry?.firstMaterial).readsFromDepthBuffer)
        XCTAssertFalse(try XCTUnwrap(first.childNodes[0].geometry?.firstMaterial).writesToDepthBuffer)
        XCTAssertEqual(cache.count, 1)
    }

    @MainActor
    func testEvictionLeavesExistingSceneNodesIntact() {
        let cache = SceneLabelCache(capacity: 2)
        let first = cache.label("First", position: SCNVector3Zero, size: 1)
        _ = cache.label("Second", position: SCNVector3Zero, size: 1)
        _ = cache.label("Third", position: SCNVector3Zero, size: 1)
        XCTAssertEqual(cache.count, 2)
        let rebuilt = cache.label("First", position: SCNVector3Zero, size: 1)
        XCTAssertFalse(first.childNodes[1].geometry === rebuilt.childNodes[1].geometry)
        XCTAssertEqual((first.childNodes[1].geometry as? SCNText)?.string as? String, "First")
        XCTAssertEqual(cache.count, 2)
    }

    @MainActor
    func testOversizedLabelsRenderWithoutBeingRetained() {
        let cache = SceneLabelCache()
        let text = String(repeating: "x", count: 513)
        let label = cache.label(text, position: SCNVector3Zero, size: 1)
        XCTAssertEqual((label.childNodes[1].geometry as? SCNText)?.string as? String, text)
        XCTAssertEqual(cache.count, 0)
    }
}
