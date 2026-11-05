import Foundation
@preconcurrency import SceneKit
import Testing
@testable import WendyDesktop

@Test func hazardFootprintsRotateInSiteCoordinates() {
    let hazard = SiteRobotHazard(position: .init(x: 3, y: 2, z: -4), size: 0.1, yaw: .pi/2, drop: false)
    let corner = hazard.corners[0]
    #expect(abs(corner.x - 2.95) < 1e-9)
    #expect(abs(corner.z + 3.95) < 1e-9)
    #expect(abs(corner.y - 2.035) < 1e-9)
}

@MainActor @Test func hazardTelemetryUpdatesWithoutRebuildingControlsAndClearsOnLostAlignment() {
    let model = SiteRobotMapModel()
    var reading = SiteRobotReading(message: "Live", pose: true, ready: false, aligned: true, moving: true,
        arrivalTiming: true, usingArrivalTiming: true, timingUnavailable: false, scanReady: true, measurementsReady: true,
        underBodyAllowance: false, x: 0, z: 0, yaw: 0, floorY: 0, route: [], joints: [],
        hazards: [.init(position: .init(x: 1, y: 0, z: 0), size: 0.05, yaw: 0, drop: false)], hazardsReady: true)
    model.accept(reading)
    #expect(model.render.hazards.count == 1) // Visible even when a hazard blocks Ready.
    var updates = 0
    let subscription = model.objectWillChange.sink { updates += 1 }
    reading.hazards[0].position.x = 2
    model.accept(reading)
    #expect(updates == 0)
    #expect(model.render.hazards.first?.position.x == 2)
    model.showObstacles = false
    #expect(model.render.hazards.isEmpty)
    model.showObstacles = true
    model.placing = true
    #expect(model.render.hazards.isEmpty)
    model.placing = false
    reading.aligned = false
    model.accept(reading)
    #expect(model.render.hazards.isEmpty)
    reading.aligned = true
    reading.hazardsReady = false
    model.accept(reading)
    #expect(model.render.hazards.isEmpty)
    subscription.cancel()
}

@MainActor @Test func hazardRendererBatchesAndReplacesMeasuredFootprints() throws {
    let root = SCNNode()
    let hazards = [
        SiteRobotHazard(position: .init(x: -0.6, y: 0, z: 0), size: 0.6, yaw: .pi/4, drop: false),
        SiteRobotHazard(position: .init(x: 0.6, y: 0, z: 0), size: 0.6, yaw: 0, drop: true)
    ]
    SiteRobotHazardRenderer.update(root, hazards: hazards)
    #expect(root.childNodes.count == 2)
    #expect(root.childNodes.allSatisfy { $0.categoryBitMask == 1 << 30 })
    #expect(root.childNodes.allSatisfy { $0.geometry?.firstMaterial?.readsFromDepthBuffer == false })
    let scene = SCNScene()
    scene.rootNode.addChildNode(root)
    let camera = SCNNode()
    camera.camera = SCNCamera()
    camera.camera?.usesOrthographicProjection = true
    camera.camera?.orthographicScale = 2
    camera.position = SCNVector3(0, 5, 0)
    camera.eulerAngles.x = -.pi/2
    scene.rootNode.addChildNode(camera)
    let renderer = SCNRenderer(device: nil, options: nil)
    renderer.scene = scene
    renderer.pointOfView = camera
    let image = renderer.snapshot(atTime: 0, with: CGSize(width: 640, height: 480), antialiasingMode: .multisampling4X)
    let tiff = try #require(image.tiffRepresentation)
    let bitmap = try #require(NSBitmapImageRep(data: tiff))
    try #require(bitmap.representation(using: .png, properties: [:])).write(to:
        URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("wendy-navigation-hazards.png"))
    var red = 0, orange = 0
    for y in stride(from: 0, to: 480, by: 4) {
        for x in stride(from: 0, to: 640, by: 4) {
            guard let color = bitmap.colorAt(x: x, y: y)?.usingColorSpace(.sRGB) else { continue }
            if color.redComponent > 0.5 {
                if color.greenComponent > color.blueComponent + 0.2 {
                    orange += 1
                } else if color.redComponent > color.greenComponent * 1.4 {
                    red += 1
                }
            }
        }
    }
    #expect(red > 20 && orange > 20)
    SiteRobotHazardRenderer.update(root, hazards: [])
    #expect(root.childNodes.isEmpty)
}
