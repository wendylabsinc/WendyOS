import Foundation
@preconcurrency import SceneKit
import Testing
@testable import WendyDesktop

@Test func fogRetainsExplorationButClearsLiveVisibility() throws {
    var grid = try #require(SiteFogGrid(minX: -5, maxX: 5, minZ: -3, maxZ: 3))
    let first = SitePoint(x: -2, y: 0, z: 0)
    let revealedFirst = grid.observe(first, fresh: true, canSee: { _, _ in true })
    #expect(revealedFirst)
    let initial = grid.explored
    #expect(!initial.isEmpty && initial == grid.visible)
    let repeated = grid.observe(first, fresh: true, canSee: { _, _ in Issue.record("Stationary pose repeated ray tests"); return true })
    #expect(!repeated)
    let moved = grid.observe(SitePoint(x: 2, y: 0, z: 0), fresh: true, canSee: { _, _ in true })
    #expect(moved)
    #expect(grid.explored.isSuperset(of: initial))
    #expect(grid.explored.count > grid.visible.count)
    let history = grid.explored
    let lostPose = grid.observe(nil, fresh: false, canSee: { _, _ in true })
    #expect(lostPose)
    #expect(grid.visible.isEmpty && grid.explored == history)
    #expect(grid.pixels.enumerated().filter { $0.offset % 4 == 1 }.allSatisfy { $0.element == 0 })
}

@Test func fogRejectsInvalidUnconfirmedAndOutOfBoundsPositions() throws {
    #expect(SiteFogGrid(minX: .nan, maxX: 1, minZ: 0, maxZ: 1) == nil)
    #expect(SiteFogGrid(minX: 1, maxX: 0, minZ: 0, maxZ: 1) == nil)
    var grid = try #require(SiteFogGrid(minX: 0, maxX: 8, minZ: 0, maxZ: 8))
    for position in [SitePoint(x: .nan, y: 0, z: 1), SitePoint(x: 20, y: 0, z: 1)] {
        grid.observe(position, fresh: true, canSee: { _, _ in Issue.record("Invalid pose reached occlusion test"); return true })
    }
    grid.observe(SitePoint(x: 1, y: 0, z: 1), fresh: false, canSee: { _, _ in true })
    #expect(grid.explored.isEmpty)
    let huge = try #require(SiteFogGrid(minX: -1000, maxX: 1000, minZ: -500, maxZ: 500))
    #expect(huge.width <= 128 && huge.height <= 128)
}

@Test func fogClipsRadiusAndOcclusionAndKeepsTextureCoordinates() throws {
    var grid = try #require(SiteFogGrid(minX: -4, maxX: 4, minZ: -3, maxZ: 3))
    let origin = SitePoint(x: -0.5, y: 0, z: -1)
    grid.observe(origin, fresh: true, canSee: { _, target in target.x < 0 })
    #expect(!grid.visible.isEmpty)
    for cell in grid.visible {
        let x = grid.minX + (Double(cell % grid.width)+0.5)*grid.cellSize
        let z = grid.minZ + (Double(cell / grid.width)+0.5)*grid.cellSize
        #expect(x < 0 && hypot(x-origin.x, z-origin.z) <= 2.5)
        #expect(grid.pixels[cell*4] == 255 && grid.pixels[cell*4+1] == 255)
    }
}

@MainActor
@Test func fogUsesScaledModelOcclusionAndRestoresMaterials() throws {
    let scene = SCNScene()
    let model = SCNNode()
    model.scale = SCNVector3(2, 2, 2)
    scene.rootNode.addChildNode(model)
    let floor = SCNNode(geometry: SCNBox(width: 4, height: 0.05, length: 3, chamferRadius: 0))
    floor.position.y = -0.025
    let original = try #require(floor.geometry?.firstMaterial)
    original.diffuse.contents = NSColor.white
    original.lightingModel = .constant
    model.addChildNode(floor)
    let wall = SCNNode(geometry: SCNBox(width: 0.05, height: 1, length: 3, chamferRadius: 0))
    wall.position.y = 0.5
    model.addChildNode(wall)
    SCNTransaction.flush()
    #expect(!SiteFogRenderer.canSee(in: model, from: .init(x: -1, y: 0.35, z: 0), to: .init(x: 1, y: 0.35, z: 0)))
    #expect(SiteFogRenderer.canSee(in: model, from: .init(x: -1, y: 0.35, z: 0), to: .init(x: -2, y: 0.35, z: 0)))

    let fog = SiteFogRenderer()
    var settings = SiteFogConfiguration()
    settings.enabled = true
    let epoch = UUID()
    let robot = SiteRobotRenderState(explorationEpoch: epoch, position: .init(x: -1, y: 0, z: -1), fresh: true)
    let summary = fog.update(model: model, configuration: settings, robot: robot)
    #expect(summary.receiving && summary.exploredArea > 0)
    #expect(floor.geometry?.firstMaterial !== original)
    #expect(fog.grid?.minX == -4 && fog.grid?.minZ == -3)
    let renderer = SCNRenderer(device: nil, options: nil)
    renderer.scene = scene
    let camera = SCNNode()
    camera.camera = SCNCamera()
    camera.camera?.usesOrthographicProjection = true
    camera.camera?.orthographicScale = 4.5
    camera.position = SCNVector3(0, 10, 0)
    camera.eulerAngles.x = -.pi/2
    scene.rootNode.addChildNode(camera)
    renderer.pointOfView = camera

    func snapshot(_ name: String) throws -> NSBitmapImageRep {
        let image = renderer.snapshot(atTime: 0, with: CGSize(width: 640, height: 480), antialiasingMode: .none)
        let tiff = try #require(image.tiffRepresentation)
        let bitmap = try #require(NSBitmapImageRep(data: tiff))
        let url = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("wendy-fog-\(name).png")
        try #require(bitmap.representation(using: .png, properties: [:])).write(to: url)
        return bitmap
    }
    let revealed = try snapshot("revealed")
    func luminance(_ image: NSBitmapImageRep, x: Int, y: Int) throws -> CGFloat {
        let color = try #require(image.colorAt(x: x, y: y)?.usingColorSpace(.deviceRGB))
        return color.redComponent + color.greenComponent + color.blueComponent
    }
    // Negative Z is towards the top of this camera. The visible left room
    // must be bright, while the wall occludes the equally near right room.
    #expect(try luminance(revealed, x: 250, y: 180) > 2)
    #expect(try luminance(revealed, x: 390, y: 180) < 0.7)
    var stale = robot; stale.fresh = false
    _ = fog.update(model: model, configuration: settings, robot: stale)
    let remembered = try snapshot("remembered")
    #expect(try luminance(remembered, x: 250, y: 180) < luminance(revealed, x: 250, y: 180))
    #expect(fog.grid?.visible.isEmpty == true && fog.grid?.explored.isEmpty == false)
    var expired = robot; expired.receivedAt = Date().addingTimeInterval(-2)
    _ = fog.update(model: model, configuration: settings, robot: expired)
    #expect(fog.grid?.visible.isEmpty == true)
    var placing = robot; placing.placing = true
    _ = fog.update(model: model, configuration: settings, robot: placing)
    #expect(fog.grid?.visible.isEmpty == true)
    settings.reset = UUID()
    _ = fog.update(model: model, configuration: settings, robot: nil)
    #expect(fog.grid?.explored.isEmpty == true)
    settings.enabled = false
    _ = fog.update(model: model, configuration: settings, robot: robot)
    #expect(floor.geometry?.firstMaterial === original)
    let restored = try snapshot("restored")
    #expect(try luminance(restored, x: 390, y: 180) > 2)
    settings.enabled = true
    _ = fog.update(model: model, configuration: settings, robot: robot)
    settings.context = "another-site-or-session"
    _ = fog.update(model: model, configuration: settings, robot: nil)
    #expect(fog.grid?.explored.isEmpty == true)
    model.position = SCNVector3(5, 0, -2)
    model.eulerAngles.y = .pi/2
    settings.reset = UUID()
    _ = fog.update(model: model, configuration: settings, robot: nil)
    let transformed = try #require(fog.grid)
    #expect(abs(transformed.minX-2) < 0.001 && abs(transformed.minZ+6) < 0.001)
}
