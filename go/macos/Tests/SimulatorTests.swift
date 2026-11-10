import XCTest
import SceneKit
import simd
@testable import WendyDesktop

final class SimulatorTests: XCTestCase {
    func testCompiledMeshNormalsAndLocalGeometryPose() {
        let mesh = SimulatorMesh(id: 7, positions: [0,0,0, 1,0,0, 0,1,0], indices: [0,1,2])
        let geometry = SimulatorGeometryBuilder.mesh(mesh)
        XCTAssertEqual(geometry.sources(for: .normal).first?.vectorCount, 3)
        let geom = SimulatorGeometry(body: 0, mesh: 7, shape: WDSimMesh, size: SIMD3(1,1,1),
            position: SIMD3(2,3,4), orientation: simd_quatf(angle: .pi/2, axis: SIMD3(0,0,1)), color: SIMD4(1,0,0,1))
        let node = SimulatorGeometryBuilder.node(geom, meshes: [7: geometry])
        XCTAssertEqual(node.simdPosition, SIMD3(2,3,4))
        XCTAssertEqual(node.childNodes.count, 1)
        XCTAssertEqual(node.childNodes[0].geometry?.elements.first?.primitiveCount, 1)
    }
    func testPrimitiveAxisDoesNotOverwriteGeomOrientation() {
        let orientation = simd_quatf(angle: .pi/3, axis: SIMD3(1,0,0))
        let geom = SimulatorGeometry(body: 0, mesh: 0, shape: WDSimCylinder, size: SIMD3(0.2,0.5,0),
            position: SIMD3(0,0,1), orientation: orientation, color: SIMD4(1,1,1,1))
        let node = SimulatorGeometryBuilder.node(geom, meshes: [:])
        XCTAssertEqual(node.simdOrientation.angle, orientation.angle, accuracy: 0.001)
        XCTAssertEqual(node.childNodes[0].eulerAngles.x, .pi/2, accuracy: 0.001)
        XCTAssertEqual((node.childNodes[0].geometry as? SCNCylinder)?.height, 1)
    }
    @MainActor
    func testBodyPoseConversionAndRendererTeardown() {
        let scene = SimulatorScene(id: "test", robotBody: 0, bodyCount: 1, geometries: [], meshes: [])
        let pose = SimulatorPose(sceneID: "test", mode: "paused", time: 1, epoch: 1, generation: 1, valid: true,
            positions: [SIMD3(1,2,3)], orientations: [simd_quatf(angle: 0, axis: SIMD3(0,0,1))])
        let view = SimulatorOrbitView()
        view.scene = SCNScene()
        let coordinator = SimulatorRenderer.Coordinator()
        coordinator.update(view, scene: scene, pose: pose, schematic: nil, follow: true, recenter: 0)
        let world = coordinator.bodies[0].simdWorldPosition
        XCTAssertEqual(world.x, 1, accuracy: 0.001)
        XCTAssertEqual(world.y, 3, accuracy: 0.001)
        XCTAssertEqual(world.z, -2, accuracy: 0.001)
        SimulatorRenderer.dismantleNSView(view, coordinator: coordinator)
        XCTAssertNil(view.scene)
        XCTAssertFalse(view.rendersContinuously)
        XCTAssertTrue(coordinator.bodies.isEmpty)
    }
    @MainActor
    func testSceneKitRendersBoardSchematic() throws {
        let renderer = SCNRenderer(device: nil, options: nil)
        let scene = SCNScene()
        let root = SimulatorRenderer.Coordinator.board(lite: true)
        root.eulerAngles.x = -.pi/2
        scene.rootNode.addChildNode(root)
        let camera = SCNNode(); camera.camera = SCNCamera(); camera.camera?.zNear = 0.001
        camera.position = SCNVector3(0.075,0.09,0.09)
        camera.look(at: SCNVector3Zero)
        scene.rootNode.addChildNode(camera)
        renderer.scene = scene; renderer.pointOfView = camera; renderer.autoenablesDefaultLighting = true
        let image = renderer.snapshot(atTime: 0, with: CGSize(width: 900, height: 650), antialiasingMode: .multisampling4X)
        let bitmap = try XCTUnwrap(NSBitmapImageRep(data: try XCTUnwrap(image.tiffRepresentation)))
        let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
        XCTAssertGreaterThan(data.count, 5000)
        try data.write(to: URL(fileURLWithPath: "/tmp/wendy-simulator-board.png"))
    }
}
