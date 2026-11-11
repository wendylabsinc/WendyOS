import XCTest
import SwiftUI
import Metal
import QuickLookThumbnailing
@preconcurrency import SceneKit
@testable import WendyMobile

@MainActor final class MobileSitesTests: XCTestCase {
    func testRealityViewDisplaysImportedModel() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let input = try model(in: base)
        let site = Site(name: "RealityKit preview")
        let scene = try XCTUnwrap(UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.first)
        let window = UIWindow(windowScene: scene)
        window.frame = scene.coordinateSpace.bounds
        let controller = UIHostingController(rootView: MobileSiteRealityView(saved: StoredSite(site: site, modelURL: input), select: { _ in }))
        window.rootViewController = controller
        window.makeKeyAndVisible()
        defer { window.isHidden = true }
        try await Task.sleep(for: .seconds(3))
        controller.view.layoutIfNeeded()
        let image = UIGraphicsImageRenderer(bounds: window.bounds).image { _ in window.drawHierarchy(in: window.bounds, afterScreenUpdates: true) }
        let attachment = XCTAttachment(image: image)
        attachment.name = "RealityView imported model"
        attachment.lifetime = .keepAlways
        add(attachment)
    }
    func testRealityViewerPreservesMetricCoordinatesAndSavedCameraIdentity() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let input = try model(in: base)
        var site = Site(name: "RealityKit room", metresPerModelUnit: 2)
        let camera = SiteCamera(gatewayID: "gateway", sourceID: "camera", name: "Door", position: SitePoint(x: 1, y: 2, z: 3))
        site.cameras = [camera]
        let before = try Data(contentsOf: input)
        let scene = MobileSiteRealityScene()
        try await scene.load(StoredSite(site: site, modelURL: input))
        let loaded = try XCTUnwrap(scene.modelEntity)
        XCTAssertEqual(loaded.scale, SIMD3<Float>(repeating: 2))
        let marker = try XCTUnwrap(scene.markers.findEntity(named: camera.id.uuidString))
        XCTAssertEqual(marker.position, SIMD3<Float>(1, 2, 3), "Camera placements are already in metres and must not be scaled twice")
        XCTAssertGreaterThan(scene.radius, 0)
        scene.orbit(yaw: 0.5, pitch: 100, zoom: 0)
        XCTAssertTrue((0..<3).allSatisfy { scene.camera.position[$0].isFinite })
        XCTAssertEqual(try Data(contentsOf: input), before, "Preview must not rewrite the saved model")
        site.cameras = []
        scene.updateCameras(site)
        XCTAssertTrue(scene.markers.children.isEmpty)
    }
    func testManualAlignmentPersistsAndRejectsOtherModel() throws {
        var site = Site(name: "Room")
        var camera = SiteCamera(gatewayID: "gateway", sourceID: "camera", name: "Camera", position: SitePoint(x: 1, y: 2, z: 3))
        camera.manualAlignment = SiteManualAlignment(modelRevision: site.modelRevision, yaw: 45, pitch: 20, roll: 180, horizontalFOV: 90, width: 1600, height: 1200)
        site.cameras = [camera]
        let restored = try JSONDecoder().decode(Site.self, from: JSONEncoder().encode(site))
        XCTAssertEqual(restored.cameras, site.cameras)
        let identity = SiteGatewayIdentity(id: "gateway", aliases: [])
        let geometry = try XCTUnwrap(MobileSiteCoveragePolicy.geometry(camera: restored.cameras[0], site: restored, records: [], gateway: identity, now: Date()))
        XCTAssertEqual(try XCTUnwrap(geometry.lens?.horizontalFOV), 90, accuracy: 0.001)
        XCTAssertFalse(try XCTUnwrap(geometry.lens).measured)
        XCTAssertEqual(geometry.pose.position, camera.position)
        XCTAssertNil(MobileSiteCoveragePolicy.geometry(camera: camera, site: restored, records: [], gateway: SiteGatewayIdentity(id: "other", aliases: []), now: Date()))
        var replacement = restored
        replacement.replaceModel()
        XCTAssertNil(replacement.cameras[0].manualAlignment)
        XCTAssertNil(camera.manualAlignment?.geometry(position: camera.position!, revision: replacement.modelRevision, now: Date()))
    }
    private func directory() throws -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }
    private func model(in directory: URL) throws -> URL {
        let scene = SCNScene()
        scene.rootNode.addChildNode(SCNNode(geometry: SCNBox(width: 2, height: 1, length: 2, chamferRadius: 0)))
        let url = directory.appendingPathComponent("Test room.usdz")
        XCTAssertTrue(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
        return url
    }
    func testSiteCreationPersistsAndIsolatesAccountAndOrganization() throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "alice", organization: "one", base: base)
        XCTAssertThrowsError(try store.create(name: "  ", assets: []))
        let assets = [MobileSiteAsset(id: "device-1", name: "Thor", robot: false), MobileSiteAsset(id: "robot-1", name: "Go2", robot: true)]
        let id = try store.create(name: "  Office  ", assets: assets)
        XCTAssertEqual(store.current?.name, "Office")
        XCTAssertEqual(store.events.count, 1)
        XCTAssertTrue(store.current?.models.isEmpty == true)
        let reopened = MobileSitesStore(account: "alice", organization: "one", base: base)
        XCTAssertEqual(reopened.selected, id)
        XCTAssertEqual(reopened.current?.assets, assets)
        XCTAssertTrue(MobileSitesStore(account: "alice", organization: "two", base: base).sites.isEmpty)
        XCTAssertTrue(MobileSitesStore(account: "bob", organization: "one", base: base).sites.isEmpty)
        XCTAssertNotEqual(MobileSitesStore.scope(account: "a/b", organization: "c"), MobileSitesStore.scope(account: "a", organization: "b/c"))
    }
    func testMultipleModelsDefaultAndArchiveRoundTrip() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let input = try model(in: base)
        let store = MobileSitesStore(account: "a", organization: "one", base: base)
        let id = try store.create(name: "Warehouse", assets: [])
        try await store.addModel(from: input, to: id)
        let first = try XCTUnwrap(store.current?.defaultModel)
        let saved = try await store.models.load(id: first)
        XCTAssertTrue(FileManager.default.fileExists(atPath: saved.modelURL.path))
        let archive = try await store.models.archive(id: first)
        let file = base.appendingPathComponent("Copy.wendysite")
        try archive.data().write(to: file)
        try await store.addModel(from: file, to: id)
        XCTAssertEqual(store.current?.models.count, 2)
        XCTAssertEqual(store.current?.defaultModel, first)
        let second = try XCTUnwrap(store.current?.models.last?.id)
        XCTAssertNotEqual(first, second)
        try store.setDefault(modelID: second, siteID: id)
        XCTAssertEqual(store.events.count, 3)
        let reopened = MobileSitesStore(account: "a", organization: "one", base: base)
        XCTAssertEqual(reopened.current?.defaultModel, second)
        XCTAssertEqual(reopened.current?.models.count, 2)
        let other = try store.create(name: "Other site", assets: [])
        XCTAssertThrowsError(try store.setDefault(modelID: second, siteID: other))
    }
    func testCameraPlacementPersistsInArchiveAndRejectsWrongSite() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "a", organization: "b", base: base)
        let id = try store.create(name: "Office", assets: [])
        try await store.addModel(from: model(in: base), to: id)
        let modelID = try XCTUnwrap(store.current?.defaultModel)
        var value = try await store.models.load(id: modelID).site
        let camera = SiteCamera(gatewayID: "cloud-v2/org/device", sourceID: "sensorlink:camera", name: "Door", position: SitePoint(x: 1, y: 2, z: 3))
        value.cameras = [camera]
        try await store.saveCameraModel(value, siteID: id, event: "Door placed")
        let archive = try await store.models.archive(id: modelID)
        let decoded = try SiteArchive(data: archive.data())
        XCTAssertEqual(decoded.site.cameras, [camera])
        XCTAssertEqual(store.events.last?.text, "Door placed")
        let other = try store.create(name: "Other", assets: [])
        do { try await store.saveCameraModel(value, siteID: other, event: "wrong"); XCTFail("Wrong site accepted") } catch {}
        value.cameras.append(camera)
        do { try await store.saveCameraModel(value, siteID: id, event: "duplicate"); XCTFail("Duplicate camera accepted") } catch {}
        let persisted = try await store.models.load(id: modelID)
        XCTAssertEqual(persisted.site.cameras, [camera])
    }
    func testProjectionAcceptsFreshFramesAndRejectsStaleVideo() throws {
        let device = try XCTUnwrap(MTLCreateSystemDefaultDevice())
        let now = Date(), revision = UUID()
        let context = try XCTUnwrap(CGContext(data: nil, width: 64, height: 64, bitsPerComponent: 8, bytesPerRow: 256, space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        context.setFillColor(UIColor.red.cgColor); context.fill(CGRect(x: 0, y: 0, width: 64, height: 64))
        let image = try XCTUnwrap(context.makeImage())
        let frame = SiteCameraColorFrame(image: image, received: now, sequence: 1)
        let source = SiteCameraColorSource(frame: { frame }, acquire: { UUID() }, release: { _ in })
        let pose = SiteCameraPose(modelRevision: revision, position: SitePoint(x: 0, y: 0, z: 0), rotation: SIMD4(1, 0, 0, 0), method: 1, provenance: "test", observedAt: now, validUntil: now.addingTimeInterval(60))
        let lens = SiteCameraLens(width: 64, height: 64, fx: 32, fy: 32, cx: 32, cy: 32, distortionModel: "none", distortion: [], measured: true, note: "test", captureMode: "test")
        var geometry = SiteCameraGeometry(pose: pose, lens: lens)
        let model = SCNNode(), wall = SCNNode(geometry: SCNPlane(width: 3, height: 3))
        wall.position.z = -2; model.addChildNode(wall)
        let renderer = SiteCameraColorRenderer()
        XCTAssertTrue(renderer.update(model: model, revision: revision, source: source, geometry: geometry, device: device, now: now))
        geometry.lens?.measured = false
        XCTAssertTrue(renderer.update(model: model, revision: revision, source: source, geometry: geometry, device: device, now: now))
        XCTAssertTrue(renderer.status.contains("Estimated"))
        XCTAssertFalse(renderer.update(model: model, revision: revision, source: source, geometry: geometry, device: device, now: now.addingTimeInterval(4)))
        XCTAssertTrue(renderer.status.contains("fresh"))
        XCTAssertFalse(renderer.update(model: model, revision: UUID(), source: source, geometry: geometry, device: device, now: now))
    }
    func testCameraFeedRebindsAfterInventoryRefreshWithoutSwitchingIdentity() {
        let before = CameraFeed(camera: Camera(id: 97, name: "Old label", stableID: "sensorlink:source:0"))
        let refreshed = CameraFeed(camera: Camera(id: 98, name: "Updated label", stableID: "sensorlink:source:0"))
        let other = CameraFeed(camera: Camera(id: 97, name: "Old label", stableID: "sensorlink:other:0"))
        func resolve(_ feeds: [CameraFeed], current: UInt64 = 7) -> CameraFeed? {
            MobileSiteCameraFeedLookup.resolve(sourceID: "sensorlink:source:0", session: 7, currentSession: current, feeds: feeds)
        }
        XCTAssertTrue(resolve([before]) === before)
        XCTAssertNil(resolve([]))
        XCTAssertTrue(resolve([other, refreshed]) === refreshed)
        XCTAssertNil(resolve([before, refreshed]))
        XCTAssertNil(resolve([other]))
        XCTAssertNil(resolve([refreshed], current: 8))
    }
    func testCoverageRejectsWrongGatewayAmbiguousExpiredAndMovedCameras() throws {
        let now = Date(), site = Site(name: "Lab")
        let camera = SiteCamera(gatewayID: "gateway", sourceID: "camera", name: "Door")
        let lens = SiteCameraLens(width: 640, height: 480, fx: 500, fy: 500, cx: 320, cy: 240, distortionModel: "none", distortion: [], measured: true, note: "test", captureMode: "H.264 640x480")
        let pose = SiteCameraPose(modelRevision: site.modelRevision, position: SitePoint(x: 0, y: 1, z: 0), rotation: SIMD4(0, 0, 0, 1), method: 1, provenance: "test", observedAt: now, validUntil: now.addingTimeInterval(60))
        let record = SiteCameraMetadata(sourceID: "camera", revision: 1, lens: lens, poses: [pose])
        let gateway = SiteGatewayIdentity(id: "gateway")
        func resolve(_ c: SiteCamera = camera, _ records: [SiteCameraMetadata] = [record], _ g: SiteGatewayIdentity = gateway, _ time: Date = now) -> SiteCameraGeometry? {
            MobileSiteCoveragePolicy.geometry(camera: c, site: site, records: records, gateway: g, now: time)
        }
        XCTAssertNotNil(resolve())
        XCTAssertNil(resolve(camera, [record], SiteGatewayIdentity(id: "another")))
        XCTAssertNil(resolve(camera, [record, record]))
        XCTAssertNil(resolve(camera, [record], gateway, now.addingTimeInterval(61)))
        var moved = camera; moved.position = SitePoint(x: 5, y: 1, z: 0)
        XCTAssertNil(resolve(moved))
        var otherModel = record; otherModel.poses[0].modelRevision = UUID()
        XCTAssertNil(resolve(camera, [otherModel]))
        var estimated = record; estimated.lens?.measured = false
        XCTAssertEqual(resolve(camera, [estimated])?.lens?.measured, false)
    }
    func testModelThumbnailRendersActualUSDZ() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let thumbnail = try await MobileSiteModelThumbnail.render(url: model(in: base), scale: 2)
        XCTAssertGreaterThan(try XCTUnwrap(thumbnail.cgImage).width, 64)
        XCTAssertGreaterThan(try XCTUnwrap(thumbnail.cgImage).height, 64)
        let pixels = try XCTUnwrap(thumbnail.cgImage?.dataProvider?.data) as Data
        XCTAssertGreaterThan(Set(pixels).count, 4, "Preview must contain rendered geometry rather than a solid placeholder")
    }
    func testRenameModelPersistsIntoArchiveWithoutChangingGeometryOrPlacement() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "a", organization: "b", base: base)
        let id = try store.create(name: "Office", assets: [])
        try await store.addModel(from: model(in: base), to: id)
        let modelID = try XCTUnwrap(store.current?.defaultModel)
        var original = try await store.models.load(id: modelID).site
        original.cameras = [SiteCamera(gatewayID: "gateway", sourceID: "door", name: "Door", position: SitePoint(x: 1, y: 2, z: 3))]
        try await store.saveCameraModel(original, siteID: id, event: "Camera placed")
        try await store.renameModel(modelID, siteID: id, name: "  Ground floor  ")
        let reopened = MobileSitesStore(account: "a", organization: "b", base: base)
        XCTAssertEqual(reopened.current?.models.first?.name, "Ground floor")
        XCTAssertEqual(reopened.current?.defaultModel, modelID)
        let archive = try await store.models.archive(id: modelID)
        XCTAssertEqual(archive.site.name, "Ground floor")
        XCTAssertEqual(archive.site.modelRevision, original.modelRevision)
        XCTAssertEqual(archive.site.cameras, original.cameras)
        for name in ["  ", String(repeating: "a", count: 201)] {
            do { try await store.renameModel(modelID, siteID: id, name: name); XCTFail("Invalid name accepted") } catch {}
        }
        do { try await store.renameModel(modelID, siteID: UUID(), name: "Wrong site"); XCTFail("Wrong site accepted") } catch {}
        let unchanged = try await store.models.load(id: modelID)
        XCTAssertEqual(unchanged.site.name, "Ground floor")
    }
    func testEditingAssetsPreservesModelsAndPersistsEmptySelection() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "a", organization: "b", base: base)
        let old = MobileSiteAsset(id: "offline", name: "Offline device", robot: false)
        let id = try store.create(name: "Office", assets: [old])
        try await store.addModel(from: model(in: base), to: id)
        let modelID = store.current?.defaultModel
        let robot = MobileSiteAsset(id: "robot", name: "Go2", robot: true)
        try store.setAssets([old, robot], siteID: id)
        XCTAssertEqual(store.current?.assets, [old, robot])
        XCTAssertEqual(store.current?.defaultModel, modelID)
        XCTAssertThrowsError(try store.setAssets([old, old], siteID: id))
        try store.setAssets([], siteID: id)
        let reopened = MobileSitesStore(account: "a", organization: "b", base: base)
        XCTAssertEqual(reopened.current?.assets, [])
        XCTAssertEqual(reopened.current?.defaultModel, modelID)
        XCTAssertEqual(reopened.current?.name, "Office")
        XCTAssertThrowsError(try store.setAssets([old], siteID: UUID()))
    }
    func testInvalidImportDoesNotLinkModelOrLogSuccess() async throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "a", organization: "b", base: base)
        let id = try store.create(name: "Office", assets: [])
        let file = base.appendingPathComponent("Broken.usdz")
        try Data("not a model".utf8).write(to: file)
        do { try await store.addModel(from: file, to: id); XCTFail("Invalid model accepted") } catch {}
        XCTAssertTrue(store.current?.models.isEmpty == true)
        XCTAssertNil(store.current?.defaultModel)
        XCTAssertEqual(store.events.count, 1)
        let models = try await store.models.list()
        XCTAssertTrue(models.isEmpty)
    }
    func testCorruptIndexIsNotOverwritten() throws {
        let base = try directory(); defer { try? FileManager.default.removeItem(at: base) }
        let store = MobileSitesStore(account: "a", organization: "b", base: base)
        try store.create(name: "Office", assets: [])
        let index = base.appendingPathComponent(MobileSitesStore.scope(account: "a", organization: "b")).appendingPathComponent("sites.json")
        let corrupt = Data("broken".utf8); try corrupt.write(to: index)
        let reopened = MobileSitesStore(account: "a", organization: "b", base: base)
        XCTAssertNotNil(reopened.loadError)
        XCTAssertThrowsError(try reopened.create(name: "Replacement", assets: []))
        XCTAssertEqual(try Data(contentsOf: index), corrupt)
    }
}
