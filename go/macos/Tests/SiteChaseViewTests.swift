import AppKit
import Foundation
@preconcurrency import SceneKit
import SwiftUI
import Testing
@testable import WendyDesktop

/// An 8 x 6 m room with a west wall at x = -4 and a box where the object is.
@MainActor private func chaseRoom() throws -> URL {
    let scene = SCNScene()
    func box(_ width: CGFloat, _ height: CGFloat, _ length: CGFloat, at position: SCNVector3, color: NSColor) {
        let node = SCNNode(geometry: SCNBox(width: width, height: height, length: length, chamferRadius: 0))
        node.geometry?.firstMaterial?.diffuse.contents = color
        node.position = position
        scene.rootNode.addChildNode(node)
    }
    box(8, 0.02, 6, at: SCNVector3(0, -0.01, 0), color: .lightGray)
    box(0.1, 2.4, 6, at: SCNVector3(-4, 1.2, 0), color: .white)
    box(0.1, 2.4, 6, at: SCNVector3(4, 1.2, 0), color: .white)
    box(8, 2.4, 0.1, at: SCNVector3(0, 1.2, -3), color: .white)
    box(8, 2.4, 0.1, at: SCNVector3(0, 1.2, 3), color: .white)
    box(0.5, 0.3, 0.25, at: SCNVector3(1.5, 0.15, -0.5), color: .systemRed)
    let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".scn")
    #expect(scene.write(to: url, options: nil, delegate: nil, progressHandler: nil))
    return url
}

@MainActor private func sceneView(in view: NSView) -> SCNView? {
    if let found = view as? SCNView { return found }
    for child in view.subviews { if let found = sceneView(in: child) { return found } }
    return nil
}

@MainActor @Test func chaseViewTrailsGo2AndStaysInsideTheRoom() async throws {
    let url = try chaseRoom()
    defer { try? FileManager.default.removeItem(at: url) }
    // Go2 stands 0.8 m from the west wall facing east; an unobstructed chase
    // camera would sit 1.5 m outside the room.
    let robot = SiteRobotRenderState(position: SitePoint(x: -3.2, y: 0, z: 0), yaw: 0, fresh: true, moving: true,
        route: [SitePoint(x: -3.2, y: 0, z: 0), SitePoint(x: -1, y: 0, z: 0.4), SitePoint(x: 0.7, y: 0, z: -0.3)],
        destination: SitePoint(x: 0.7, y: 0, z: -0.3))
    let points = stride(from: -3.0, through: 3.0, by: 0.1).map { SitePoint(x: 3.9, y: 0.6, z: $0) }
    let scene = SiteScene(modelURL: url, points: points, revision: 1, site: Site(name: "Workshop"), showCameraCoverage: false,
        placingCamera: nil, select: { _ in }, place: { _, _ in }, robot: robot, chase: true,
        target: SitePoint(x: 1.5, y: 0, z: -0.5))
    let host = NSHostingView(rootView: scene.frame(width: 960, height: 540))
    let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 540), styleMask: [.borderless], backing: .buffered, defer: false)
    window.contentView = host
    window.orderFrontRegardless()
    defer { window.close() }
    var view: SCNView?
    for _ in 0..<50 where view?.pointOfView?.camera?.fieldOfView != 62 {
        try await Task.sleep(for: .milliseconds(100))
        view = sceneView(in: host)
    }
    let found = try #require(view)
    let camera = try #require(found.pointOfView)
    #expect(camera.camera?.fieldOfView == 62)
    #expect(!found.allowsCameraControl)
    let eye = camera.presentation.simdWorldPosition
    // Behind and above Go2, but pulled in front of the wall.
    #expect(eye.x < -3.4 && eye.x > -3.95)
    #expect(eye.y > 0.45)
    #expect(camera.presentation.simdWorldFront.x > 0.8)

    let image = found.snapshot()
    let tiff = try #require(image.tiffRepresentation)
    let bitmap = try #require(NSBitmapImageRep(data: tiff))
    let data = try #require(bitmap.representation(using: .png, properties: [:]))
    #expect(data.count > 5000)
    // Kept outside the repository for a human to look at.
    try data.write(to: FileManager.default.temporaryDirectory.appendingPathComponent("wendy-go2-chase-view.png"))
}
