import XCTest
import RealityKit
import Metal
import UIKit
import SwiftUI
@preconcurrency import SceneKit
@testable import WendyMobile

@MainActor final class RealityProjectionTests: XCTestCase {
    func testRealityViewProjectsCameraPattern() async throws {
        let folder = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: folder) }
        let url = folder.appendingPathComponent("Projection.usdz")
        let imported = SCNScene()
        imported.rootNode.addChildNode(SCNNode(geometry: SCNBox(width: 2,height: 1,length: 2,chamferRadius: 0)))
        XCTAssertTrue(imported.write(to: url,options: nil,delegate: nil,progressHandler: nil))
        var site = Site(name: "Projection test")
        var camera = SiteCamera(gatewayID: "test",sourceID: "test",name: "Test pattern",position: SitePoint(x: 0,y: 0,z: 3))
        camera.manualAlignment = SiteManualAlignment(modelRevision: site.modelRevision,yaw: 180,pitch: 0,roll: 180,horizontalFOV: 70,width: 64,height: 64)
        site.cameras = [camera]
        let geometry = camera.manualAlignment!.geometry(position: camera.position!,revision: site.modelRevision,now: Date())!
        let format = UIGraphicsImageRendererFormat(); format.scale = 1
        let image = UIGraphicsImageRenderer(size: CGSize(width: 64,height: 64),format: format).image { context in
            for (index,color) in [UIColor.red,.green,.blue,.yellow].enumerated() {
                color.setFill(); context.fill(CGRect(x: (index%2)*32,y: (index/2)*32,width: 32,height: 32))
            }
        }.cgImage!
        let source = SiteCameraColorSource(frame: { SiteCameraColorFrame(image: image,received: Date().addingTimeInterval(-0.01),sequence: 1) },acquire: { UUID() },release: { _ in })
        var status = ""
        let scene = try XCTUnwrap(UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.first)
        let window = UIWindow(windowScene: scene); window.frame = scene.coordinateSpace.bounds
        let controller = UIHostingController(rootView: MobileSiteRealityView(saved: StoredSite(site: site,modelURL: url),
            cameraGeometry: [camera.id: geometry],colorSources: [camera.id: source],colorStatus: { status = $0.values.first ?? "" },select: { _ in }))
        window.rootViewController = controller; window.makeKeyAndVisible()
        defer { window.isHidden = true }
        try await Task.sleep(for: .seconds(4))
        XCTAssertTrue(status.contains("Live projection"),status)
        let screenshot = UIGraphicsImageRenderer(bounds: window.bounds).image { _ in window.drawHierarchy(in: window.bounds,afterScreenUpdates: true) }
        let attachment = XCTAttachment(image: screenshot); attachment.name = "RealityKit projected camera pattern"; attachment.lifetime = .keepAlways; add(attachment)
    }
    private func plane(name: String, z: Float, halfWidth: Float) throws -> ModelEntity {
        var descriptor = MeshDescriptor(name: name)
        descriptor.positions = .init([SIMD3(-halfWidth,-halfWidth,z), SIMD3(halfWidth,-halfWidth,z), SIMD3(halfWidth,halfWidth,z), SIMD3(-halfWidth,halfWidth,z)])
        descriptor.primitives = .triangles([0,1,2,0,2,3])
        let entity = ModelEntity(mesh: try .generate(from: [descriptor]), materials: [SimpleMaterial(color: .gray, roughness: 1, isMetallic: false)])
        entity.name = name
        return entity
    }
    private func geometry(_ revision: UUID, position: SitePoint = SitePoint(x: 0,y: 0,z: 0)) -> SiteCameraGeometry {
        SiteManualAlignment(modelRevision: revision, yaw: 0,pitch: 0,roll: 0,horizontalFOV: 90,width: 64,height: 64)
            .geometry(position: position, revision: revision, now: Date())!
    }
    func testGPUProjectionRejectsSurfacesBehindWallAndBehindCamera() throws {
        let root = Entity()
        root.addChild(try plane(name: "Furniture", z: 2, halfWidth: 1))
        root.addChild(try plane(name: "Wall2", z: 4, halfWidth: 4))
        let renderer = try MobileRealityProjection(model: root)
        try renderer.renderDepth(geometry(UUID()))
        XCTAssertFalse(renderer.mountingWallOverlap)
        let function = try XCTUnwrap(renderer.library.makeFunction(name: "wendyRealityVisibilityTest"))
        let pipeline = try renderer.device.makeComputePipelineState(function: function)
        let points: [SIMD4<Float>] = [SIMD4(0,0,2,1), SIMD4(0,0,4,1), SIMD4(2.5,0,4,1), SIMD4(0,0,-1,1)]
        let input = try XCTUnwrap(renderer.device.makeBuffer(bytes: points, length: points.count * MemoryLayout<SIMD4<Float>>.stride))
        let output = try XCTUnwrap(renderer.device.makeBuffer(length: points.count * MemoryLayout<UInt32>.stride, options: .storageModeShared))
        let command = try XCTUnwrap(renderer.queue.makeCommandBuffer())
        let encoder = try XCTUnwrap(command.makeComputeCommandEncoder())
        encoder.setComputePipelineState(pipeline)
        encoder.setTexture(try XCTUnwrap(renderer.atlas).read(), index: 0)
        encoder.setBuffer(input,offset: 0,index: 0); encoder.setBuffer(output,offset: 0,index: 1)
        encoder.dispatchThreads(MTLSize(width: points.count,height: 1,depth: 1), threadsPerThreadgroup: MTLSize(width: points.count,height: 1,depth: 1))
        encoder.endEncoding(); command.commit(); command.waitUntilCompleted()
        XCTAssertEqual(command.status,.completed)
        let visibility = Array(UnsafeBufferPointer(start: output.contents().assumingMemoryBound(to: UInt32.self), count: points.count))
        XCTAssertEqual(visibility,[1,0,1,0], "Projection must stop at the nearest surface and never appear behind the optical center")
    }
    func testOnlyIdentifiedMountingWallIsExcluded() throws {
        let root = Entity()
        root.addChild(try plane(name: "Wall0", z: 0, halfWidth: 1))
        root.addChild(try plane(name: "Furniture", z: 0.1, halfWidth: 1))
        root.addChild(try plane(name: "Wall1", z: 2, halfWidth: 3))
        let batches = try MobileRealityProjection.extract(root)
        let pose = geometry(UUID(), position: SitePoint(x: 0,y: 0,z: -0.02)).pose
        XCTAssertEqual(MobileRealityProjection.mountingWall(in: batches, pose: pose), 0)
        var outside = pose; outside.position.x = 2
        XCTAssertNil(MobileRealityProjection.mountingWall(in: batches, pose: outside), "An opening outside the wall mesh must not hide a wall")
    }
    func testStaleOrWrongSizeFramesRestoreOriginalMaterials() throws {
        let revision = UUID(), root = Entity()
        let entity = try plane(name: "Wall1", z: 2, halfWidth: 2); root.addChild(entity)
        let renderer = try MobileRealityProjection(model: root)
        let format = UIGraphicsImageRendererFormat(); format.scale = 1
        let image = UIGraphicsImageRenderer(size: CGSize(width: 64,height: 64),format: format).image { context in
            UIColor.red.setFill(); context.fill(CGRect(x: 0,y: 0,width: 64,height: 64))
        }.cgImage!
        var frame = SiteCameraColorFrame(image: image,received: Date(),sequence: 1)
        let source = SiteCameraColorSource(frame: { frame },acquire: { UUID() },release: { _ in })
        XCTAssertTrue(try renderer.update(geometry: geometry(revision),source: source,revision: revision,now: Date()).contains("Live projection"))
        XCTAssertTrue(renderer.installed)
        frame = SiteCameraColorFrame(image: image,received: Date().addingTimeInterval(-10),sequence: 2)
        XCTAssertTrue(try renderer.update(geometry: geometry(revision),source: source,revision: revision,now: Date()).contains("Waiting"))
        XCTAssertFalse(renderer.installed)
        XCTAssertTrue(entity.model?.materials.first is SimpleMaterial)
        var mismatched = geometry(revision); mismatched.lens?.width = 128
        frame = SiteCameraColorFrame(image: image,received: Date(),sequence: 3)
        _ = try renderer.update(geometry: mismatched,source: source,revision: revision,now: Date())
        XCTAssertFalse(renderer.installed)
        _ = try renderer.update(geometry: geometry(revision),source: source,revision: UUID(),now: Date())
        XCTAssertFalse(renderer.installed)
    }
}
