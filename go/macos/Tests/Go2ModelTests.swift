#if os(macOS)
import Testing
import Combine
@preconcurrency import SceneKit
@testable import WendyDesktop

@MainActor @Test func go2MeshesHaveRealScaleAndIndependentJoints() throws {
    let first = Go2Model.make()
    let second = Go2Model.make()
    var geometries = 0
    first.enumerateChildNodes { node, _ in
        if node.geometry != nil { geometries += 1 }
    }
    #expect(geometries > 20)
    let (low, high) = first.boundingBox
    #expect(high.x - low.x > 0.5 && high.x - low.x < 1)
    #expect(high.y - low.y > 0.25 && high.y - low.y < 0.7)
    #expect(high.z - low.z > 0.2 && high.z - low.z < 0.6)
    for name in Go2Model.jointNames {
        #expect(first.childNode(withName: name, recursively: true) != nil)
    }
    let untouched = try #require(second.childNode(withName: "FR_thigh_joint", recursively: true))
    let original = untouched.simdOrientation
    Go2Model.pose(first, joints: Array(repeating: 0.1, count: 12))
    let changed = try #require(first.childNode(withName: "FR_thigh_joint", recursively: true))
    #expect(abs(changed.simdOrientation.angle - 0.1) < 0.001)
    #expect(untouched.simdOrientation == original)
}

@MainActor @Test func robotLocalizationGeometryPreservesTransformsAndScale() throws {
    let scene = SCNScene()
    let parent = SCNNode()
    parent.position = SCNVector3(2, 0, -3)
    parent.eulerAngles.y = .pi/2
    let box = SCNNode(geometry: SCNBox(width: 2, height: 0.2, length: 4, chamferRadius: 0))
    box.position = SCNVector3(1, 2, 0)
    parent.addChildNode(box)
    scene.rootNode.addChildNode(parent)
    let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString+".scn")
    defer { try? FileManager.default.removeItem(at: url) }
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    let values = try SiteRobotGeometry.triangles(url, scale: 2)
    #expect(values.count == 108)
    for (axis, bounds) in [(0, (0.0, 8.0)), (1, (3.8, 4.2)), (2, (-10.0, -6.0))] {
        let components = stride(from: 0, to: values.count, by: 3).map { values[$0+axis] }
        #expect(abs(try #require(components.min())-bounds.0) < 0.001)
        #expect(abs(try #require(components.max())-bounds.1) < 0.001)
    }
}
@MainActor @Test func robotLocalizationExcludesMovableRoomPlanObjects() throws {
    let scene = SCNScene()
    let wall = SCNNode(geometry: SCNBox(width: 4, height: 3, length: 0.1, chamferRadius: 0))
    wall.name = "Wall0"
    scene.rootNode.addChildNode(wall)
    let objects = SCNNode()
    objects.name = "Object_grp"
    let chairGroup = SCNNode()
    chairGroup.name = "Chair_grp"
    let chair = SCNNode(geometry: SCNBox(width: 1, height: 1, length: 1, chamferRadius: 0))
    chair.position.x = 20
    chairGroup.addChildNode(chair)
    objects.addChildNode(chairGroup)
    scene.rootNode.addChildNode(objects)
    let door = SCNNode(geometry: SCNBox(width: 1, height: 2, length: 0.1, chamferRadius: 0))
    door.name = "Door0"
    door.position.x = 30
    scene.rootNode.addChildNode(door)
    let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString+".scn")
    defer { try? FileManager.default.removeItem(at: url) }
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    let values = try SiteRobotGeometry.triangles(url, scale: 1)
    #expect(values.count == 108)
    #expect(stride(from: 0, to: values.count, by: 3).allSatisfy { abs(values[$0]) <= 2 })
}
@MainActor @Test func go2SceneHeadingMatchesMapForwardDirection() {
    let root = Go2Model.make()
    for yaw in [0.0, Double.pi / 2, -Double.pi / 2, Double.pi] {
        root.eulerAngles.y = yaw
        let forward = root.convertVector(SCNVector3(1, 0, 0), to: nil)
        #expect(abs(Double(forward.x) - cos(yaw)) < 0.00001)
        #expect(abs(Double(forward.z) + sin(yaw)) < 0.00001)
    }
}
@MainActor @Test func robotTelemetryDoesNotRepublishControlsButUpdatesScene() {
    let model = SiteRobotMapModel()
    var reading = SiteRobotReading(message: "Live", pose: true, ready: true, aligned: true, moving: false,
        arrivalTiming: true, usingArrivalTiming: true, timingUnavailable: false, scanReady: true, measurementsReady: true,
        underBodyAllowance: false, x: 1, z: 2, yaw: 0, floorY: 0, route: [], joints: [])
    model.accept(reading)
    var controlUpdates = 0
    let subscription = model.objectWillChange.sink { controlUpdates += 1 }
    for index in 1...20 {
        reading.x = Double(index)
        reading.yaw = Double(index) * 0.01
        reading.joints = [Double(index) * 0.02]
        model.accept(reading)
    }
    #expect(controlUpdates == 0)
    #expect(model.render.position?.x == 20)
    #expect(model.render.yaw == 0.2)
    #expect(model.render.joints == [0.4])
    reading.ready = false
    reading.message = "Obstacle ahead"
    model.accept(reading)
    #expect(controlUpdates == 1)
    #expect(model.reading?.ready == false)
    #expect(model.reading?.message == "Obstacle ahead")
    reading.measurementsReady = false
    model.accept(reading)
    #expect(controlUpdates == 2)
    #expect(model.reading?.measurementsReady == false)
    withExtendedLifetime(subscription) {}
}

@MainActor @Test func siteFeedActivityRemainsLiveWithoutWorkspaceTimer() {
    let now = Date()
    var received: Date? = nil
    var connected = false
    let feed = SiteFeed(id: "camera", name: "Camera", activity: { (received, connected) })
    #expect(!feed.isReceiving(at: now))
    received = now; connected = true
    #expect(feed.isReceiving(at: now))
    #expect(!feed.isReceiving(at: now.addingTimeInterval(6)))
    connected = false
    #expect(!feed.isReceiving(at: now))
}

private actor RecoveringRobotObserver: SiteRobotObserving {
    var events: [String] = []
    var opens = 0
    let alwaysDisconnect: Bool

    init(alwaysDisconnect: Bool = false) { self.alwaysDisconnect = alwaysDisconnect }

    func open(session: UInt64, serial: String) async throws -> UInt64 {
        opens += 1
        events.append("open \(opens) \(session) \(serial)")
        return UInt64(opens)
    }

    func poll(_ handle: UInt64) async throws -> SiteRobotReading {
        try await Task.sleep(for: .milliseconds(10))
        let disconnected = alwaysDisconnect || handle == 1
        return SiteRobotReading(message: disconnected ? "Go2 observation stream disconnected." : "Live pose",
            disconnected: disconnected, pose: !disconnected, ready: false, aligned: false, moving: false,
            arrivalTiming: true, usingArrivalTiming: true, timingUnavailable: false, scanReady: false,
            measurementsReady: false, underBodyAllowance: true, x: 0, z: 0, yaw: 0, floorY: 0, route: [], joints: [])
    }

    func close(_ handle: UInt64) async { events.append("close \(handle)") }
}

@MainActor @Test func robotObservationReconnectsWithNewHandleAndClosesOldOne() async throws {
    let observer = RecoveringRobotObserver()
    let model = SiteRobotMapModel(observationClient: observer)
    model.serial = "test-go2"
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask {
            await model.observe(session: 7, site: Site(name: "Test"),
                modelURL: URL(fileURLWithPath: "/unused.scn"), active: true)
        }
        defer { group.cancelAll() }
        for _ in 0..<300 {
            if model.handle == 2 && model.reading?.pose == true { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(model.handle == 2)
        #expect(model.reading?.pose == true)
        #expect(model.reading?.moving == false)
        #expect(model.reading?.aligned == false)
        #expect(model.destination == nil && model.candidates.isEmpty)
        #expect(model.error == nil)
        #expect(await observer.events == ["open 1 7 test-go2", "close 1", "open 2 7 test-go2"])
    }
    #expect(await observer.events.last == "close 2")
    #expect(model.handle == 0)
}

@MainActor @Test func robotObservationCancellationDuringBackoffDoesNotReconnect() async throws {
    let observer = RecoveringRobotObserver()
    let model = SiteRobotMapModel(observationClient: observer)
    model.serial = "test-go2"
    try await withThrowingTaskGroup(of: Void.self) { group in
        group.addTask {
            await model.observe(session: 7, site: Site(name: "Test"),
                modelURL: URL(fileURLWithPath: "/unused.scn"), active: true)
        }
        defer { group.cancelAll() }
        for _ in 0..<100 {
            if await observer.events.contains("close 1") { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(model.statusLabel == "Go2 disconnected")
    }
    #expect(await observer.events == ["open 1 7 test-go2", "close 1"])
}

@MainActor @Test func robotObservationRecoveryStopsAfterThreeRetries() async {
    let observer = RecoveringRobotObserver(alwaysDisconnect: true)
    let model = SiteRobotMapModel(observationClient: observer)
    model.serial = "test-go2"
    await model.observe(session: 7, site: Site(name: "Test"),
        modelURL: URL(fileURLWithPath: "/unused.scn"), active: true)
    #expect(await observer.opens == 4)
    #expect(await observer.events.last == "close 4")
    #expect(model.statusLabel == "Go2 disconnected")
    #expect(model.error == "Go2 observation stream disconnected.")
    #expect(model.handle == 0)
}
#endif
