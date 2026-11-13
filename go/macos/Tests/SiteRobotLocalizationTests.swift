import Foundation
import Testing
@preconcurrency import SceneKit
@testable import WendyDesktop

@Test func unconfirmedRobotSuggestionRefreshesWhenEvidenceChanges() {
    let now = Date(timeIntervalSince1970: 1000)
    let snapshot = RobotSuggestionFreshness(observationRevision: 10, received: now)
    #expect(!snapshot.shouldRefresh(observationRevision: 11, now: now.addingTimeInterval(1)))
    #expect(snapshot.shouldRefresh(observationRevision: 11, now: now.addingTimeInterval(5)))
    #expect(!snapshot.shouldRefresh(observationRevision: 10, now: now.addingTimeInterval(5)))
    #expect(snapshot.shouldRefresh(observationRevision: 10, now: now.addingTimeInterval(30)))
}

@Test func robotGeometryExcludesSeatOnlyChairBoxes() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    let scene = SCNScene()
    let floor = SCNNode(geometry: SCNBox(width: 4, height: 0.1, length: 4, chamferRadius: 0))
    floor.name = "Floor0"
    scene.rootNode.addChildNode(floor)
    let objects = SCNNode()
    objects.name = "Object_grp"
    scene.rootNode.addChildNode(objects)
    let chairs = SCNNode()
    chairs.name = "Chair_grp"
    chairs.position = SCNVector3(1, 0, 2)
    chairs.eulerAngles.y = 0.4
    objects.addChildNode(chairs)
    let seat = SCNNode(geometry: SCNBox(width: 0.5, height: 0.5, length: 0.5, chamferRadius: 0))
    seat.name = "SeatGeometry"
    chairs.addChildNode(seat)
    let table = SCNNode(geometry: SCNBox(width: 2, height: 0.75, length: 1, chamferRadius: 0))
    table.name = "Table0"
    objects.addChildNode(table)
    let url = root.appendingPathComponent("room.scn")
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    let architecture = try SiteRobotGeometry.triangles(url, scale: 1)
    let furniture = try SiteRobotGeometry.triangles(url, scale: 1, furnitureOnly: true)
    #expect(!architecture.isEmpty)
    #expect(!furniture.isEmpty)
    let footprints = try SiteRobotGeometry.chairFootprints(url, scale: 2)
    #expect(footprints.count == 1)
    #expect(footprints[0].contains(x: 2, z: 4))
    #expect(!footprints[0].contains(x: 0, z: 0))
    // Removing the chair must not change either localization mesh: the table
    // remains available, and architecture never included the chair.
    chairs.removeFromParentNode()
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    #expect(try SiteRobotGeometry.triangles(url, scale: 1) == architecture)
    #expect(try SiteRobotGeometry.triangles(url, scale: 1, furnitureOnly: true) == furniture)
}

@Test func robotCameraExcludesChairBackAboveSeatBox() {
    func landmark(x: Float, y: Float, z: Float) -> [Float] {
        [x, y, z] + [Float](repeating: 1, count: 128)
    }
    let fixed = landmark(x: 3, y: 1.4, z: 2)
    let values = landmark(x: 1, y: 0.4, z: 2) + landmark(x: 1, y: 1.4, z: 2) + fixed
    let map = SiteVisualMap(modelRevision: UUID(), modelSHA256: String(repeating: "0", count: 64), capturedAt: Date(), frameCount: 1, landmarks: values.withUnsafeBytes { Data($0) })
    let chairs = [SiteRobotGeometry.ChairFootprint(minX: 0.5, maxX: 1.5, minZ: 1.5, maxZ: 2.5)]
    let filtered = RobotVisualLocalization.removingChairReferences(from: map, chairs: chairs)
    #expect(filtered.count == 1)
    #expect(filtered.landmarks == fixed.withUnsafeBytes { Data($0) })
    #expect(map.count == 3)
}

@Test func robotReferenceMapIsBoundToModelRevisionAndRobot() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    let model = root.appendingPathComponent("model.usdz")
    try Data("first model".utf8).write(to: model)
    let surface = RobotObservedSurface(position: RobotMapVertex(X:1,Y:2,Z:0.5), direction: RobotMapPoint(X:1,Y:0), horizontal:false, stable:true)
    var stored = try RobotReferenceStore.read(model:model,robot:"go2-test",frame:"site/revision-1")
    stored = RobotReferenceStore.merging([surface],into:stored)
    try RobotReferenceStore.save(stored,model:model)
    #expect(try RobotReferenceStore.read(model:model,robot:"go2-test",frame:"site/revision-1").surfaces.count == 1)
    #expect(try RobotReferenceStore.read(model:model,robot:"another-go2",frame:"site/revision-1").surfaces.isEmpty)
    #expect(try RobotReferenceStore.read(model:model,robot:"go2-test",frame:"site/revision-2").surfaces.isEmpty)
    try Data("replacement model".utf8).write(to:model)
    #expect(try RobotReferenceStore.read(model:model,robot:"go2-test",frame:"site/revision-1").surfaces.isEmpty)
}

@Test func savedRobotReferencesDoNotGrowOnRepeatedPackets() {
    let surface = RobotObservedSurface(position: RobotMapVertex(X:1,Y:2,Z:0.5), direction: RobotMapPoint(X:1,Y:0), horizontal:false, stable:true)
    var stored = RobotSavedMap(frame:"site/revision",modelDigest:"digest",robot:"go2-test")
    for _ in 0..<100 { stored = RobotReferenceStore.merging([surface],into:stored) }
    var weaker = surface
    weaker.stable = false
    stored = RobotReferenceStore.merging([weaker], into: stored)
    #expect(stored.surfaces.count == 1)
    #expect(stored.surfaces[0].stable)
}

@Test func doorSwingReferencesRespectTransformsHeightAndNestedMeshes() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    let scene = SCNScene()
    let wall = SCNNode(geometry: SCNBox(width: 5, height: 3, length: 0.1, chamferRadius: 0))
    wall.name = "Wall0"
    scene.rootNode.addChildNode(wall)
    let door = SCNNode()
    door.name = "Door0"
    door.position = SCNVector3(2, 1, 3)
    door.eulerAngles.y = .pi/2
    let leaf = SCNNode(geometry: SCNBox(width: 1, height: 2, length: 0.05, chamferRadius: 0))
    leaf.name = "LeafGeometry"
    door.addChildNode(leaf)
    scene.rootNode.addChildNode(door)
    let url = root.appendingPathComponent("room.scn")
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    let geometry = try SiteRobotGeometry.triangles(url, scale: 2)
    let exclusions = try SiteRobotGeometry.referenceExclusions(url, scale: 2)
    #expect(exclusions.doors.count == 1)
    #expect(exclusions.contains(x: 5.5, y: 2, z: 6))
    #expect(exclusions.contains(x: 2.5, y: 2, z: 6))
    #expect(!exclusions.contains(x: 9, y: 2, z: 6))
    #expect(!exclusions.contains(x: 5.5, y: 6, z: 6))
    func landmark(_ x: Float, _ y: Float, _ z: Float) -> [Float] { [x,y,z] + Array(repeating: 1, count: 128) }
    let retained = landmark(9,2,6) + landmark(5.5,6,6)
    let values = landmark(5.5,2,6) + retained
    let map = SiteVisualMap(modelRevision: UUID(), modelSHA256: String(repeating: "0", count: 64), capturedAt: Date(), frameCount: 1, landmarks: values.withUnsafeBytes { Data($0) })
    let filtered = RobotVisualLocalization.removingMovableReferences(from: map, exclusions: exclusions)
    #expect(filtered.landmarks == retained.withUnsafeBytes { Data($0) })
    #expect(map.count == 3)
    door.removeFromParentNode()
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    #expect(try SiteRobotGeometry.triangles(url, scale: 2) == geometry)
}
