import AppKit
import AVFoundation
import Metal
@preconcurrency import SceneKit
import Testing
@testable import WendyDesktop

@Suite @MainActor struct SiteCameraColorTests {
    func image(width: Int = 64, height: Int = 64) throws -> CGImage {
        var bytes = [UInt8](repeating: 255, count: width * height * 4)
        for y in 0..<height { for x in 0..<width {
            let offset = (y * width + x) * 4
            bytes[offset] = x < width / 2 && y < height / 2 ? 255 : 0
            bytes[offset + 1] = x >= width / 2 ? 255 : 0
            bytes[offset + 2] = y >= height / 2 && x < width / 2 ? 255 : 0
        } }
        let provider = try #require(CGDataProvider(data: Data(bytes) as CFData))
        return try #require(CGImage(width: width, height: height, bitsPerComponent: 8, bitsPerPixel: 32,
            bytesPerRow: width * 4, space: CGColorSpace(name: CGColorSpace.sRGB)!,
            bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedLast.rawValue), provider: provider,
            decode: nil, shouldInterpolate: false, intent: .defaultIntent))
    }

    func untexturedPlane(width: Float, height: Float) -> SCNGeometry {
        let w = width / 2, h = height / 2
        let vertices = [SCNVector3(-w, -h, 0), SCNVector3(w, -h, 0), SCNVector3(w, h, 0), SCNVector3(-w, h, 0)]
        let result = SCNGeometry(sources: [SCNGeometrySource(vertices: vertices),
            SCNGeometrySource(normals: Array(repeating: SCNVector3(0, 0, 1), count: 4))],
            elements: [SCNGeometryElement(indices: [Int32(0), 1, 2, 0, 2, 3], primitiveType: .triangles)])
        result.materials = [SCNMaterial()]
        return result
    }

    func geometry(revision: UUID, now: Date) -> SiteCameraGeometry {
        SiteCameraGeometry(pose: SiteCameraPose(modelRevision: revision, position: .init(x: 0, y: 0, z: 0),
            rotation: SIMD4(1, 0, 0, 0), method: 1, provenance: "test", observedAt: now,
            validUntil: now.addingTimeInterval(60)),
            lens: SiteCameraLens(width: 64, height: 64, fx: 32, fy: 32, cx: 32, cy: 32,
                distortionModel: "none", distortion: [], measured: true, note: "test", captureMode: "full"))
    }

    @Test func framesRequireMatchingResolutionAndFreshness() throws {
        let now = Date(), calibration = geometry(revision: UUID(), now: Date())
        let lens = try #require(calibration.lens)
        let frame = SiteCameraColorFrame(image: try image(), received: now, sequence: 1)
        #expect(frame.matches(lens, at: now))
        #expect(!frame.matches(lens, at: now.addingTimeInterval(4)))
        #expect(!frame.matches(lens, at: now.addingTimeInterval(-1)))
        let cropped = SiteCameraColorFrame(image: try image(width: 32), received: now, sequence: 2)
        #expect(!cropped.matches(lens, at: now))
    }

    @Test func colorDecodingLeaseDoesNotDisableCalibration() {
        let decoder = H264CalibrationFrames()
        let calibration = decoder.acquire()
        let color = decoder.acquireColor()
        decoder.releaseColor(color)
        #expect(decoder.isEnabled)
        decoder.release(calibration)
        #expect(!decoder.isEnabled)
        let onlyColor = decoder.acquireColor()
        #expect(decoder.isEnabled)
        decoder.reset()
        #expect(decoder.latestColor == nil)
        #expect(decoder.isEnabled)
        decoder.releaseColor(onlyColor)
        #expect(!decoder.isEnabled)
    }

    // Generated 64x48 H.264 IDR: red upper half, blue lower half.
    @Test func h264ColorFramesPreserveChromaAndClearOnReset() async throws {
        let encoded = try #require(Data(base64Encoded: "AAAAAWdkEAqsuI9CAAADAAIAAAMAZAgAAAABaO4PLIsAAAFliIQEv/7oyfzLKy/j1DezRimzSaccwj0dCRIAS8ZkRxJg1XYraZBoOsZosrfre8DEqu9/Ol6LHwnAR7fejsMX63fGc+gIYQ=="))
        let decoder = H264AnnexBDecoder()
        // Global coloring must decode without a visible camera preview.
        let lease = decoder.calibrationFrames.acquireColor()
        defer { decoder.calibrationFrames.releaseColor(lease) }
        decoder.feed(encoded + Data([0, 0, 0, 1, 9, 16, 0, 0, 0, 1, 9, 16]))
        let deadline = ContinuousClock.now.advanced(by: .seconds(8))
        while decoder.calibrationFrames.latestColor == nil && ContinuousClock.now < deadline {
            try await Task.sleep(for: .milliseconds(50))
        }
        let frame = try #require(decoder.calibrationFrames.latestColor)
        #expect(frame.image.width == 64 && frame.image.height == 48)
        let bitmap = NSBitmapImageRep(cgImage: frame.image)
        let top = try #require(bitmap.colorAt(x: 32, y: 8)?.usingColorSpace(.sRGB))
        let bottom = try #require(bitmap.colorAt(x: 32, y: 40)?.usingColorSpace(.sRGB))
        #expect(top.redComponent > 0.8 && top.blueComponent < 0.15)
        #expect(bottom.blueComponent > 0.8 && bottom.redComponent < 0.15)
        decoder.reset()
        #expect(decoder.calibrationFrames.latestColor == nil)
        decoder.feed(encoded + Data([0, 0, 0, 1, 9, 16, 0, 0, 0, 1, 9, 16]))
        decoder.calibrationFrames.releaseColor(lease)
        try await Task.sleep(for: .milliseconds(200))
        #expect(decoder.calibrationFrames.latestColor == nil)
    }

    @Test(arguments: [false, true]) func projectsColorsWithOcclusionAndRestoresMaterials(transformed: Bool) throws {
        let device = try #require(MTLCreateSystemDefaultDevice())
        let now = Date(), revision = UUID()
        var calibration = geometry(revision: revision, now: now)
        let frame = SiteCameraColorFrame(image: try image(), received: now, sequence: 1)
        let source = SiteCameraColorSource(frame: { frame }, acquire: { UUID() }, release: { _ in })
        let scene = SCNScene(), model = SCNNode()
        model.scale = SCNVector3(2, 2, 2)
        if transformed {
            model.position = SCNVector3(1, 0.5, -0.25)
            model.simdOrientation = simd_quatf(angle: 0.35, axis: SIMD3(0, 1, 0))
            calibration.pose.position = .init(x: 1, y: 0.5, z: -0.25)
            calibration.pose.rotation = SIMD4<Double>((model.simdOrientation * simd_quatf(angle: .pi, axis: SIMD3(1, 0, 0))).vector)
            calibration.lens?.fx = 40; calibration.lens?.fy = 35
            calibration.lens?.cx = 26; calibration.lens?.cy = 38
            calibration.lens?.distortionModel = "brown_conrady"
            calibration.lens?.distortion = [0.08, -0.01, 0.002, -0.003, 0.001]
        }
        scene.rootNode.addChildNode(model)
        let untextured = untexturedPlane(width: 3, height: 3)
        let wall = SCNNode(geometry: untextured)
        wall.position.z = -1.5
        wall.name = "Wall1"
        let original = try #require(wall.geometry?.firstMaterial)
        original.lightingModel = .physicallyBased
        original.diffuse.contents = NSColor(white: 0.5, alpha: 1)
        model.addChildNode(wall)
        let blocker = SCNNode(geometry: SCNPlane(width: 0.3, height: 0.3))
        blocker.position.z = -0.75
        model.addChildNode(blocker)
        // A fitted camera inside a RoomPlan wall must still see into the room.
        let mountingWall = SCNNode(geometry: SCNBox(width: 3, height: 3, length: 0.1, chamferRadius: 0))
        mountingWall.name = "Wall0"
        mountingWall.position.z = 0.025
        mountingWall.eulerAngles.y = .pi
        model.addChildNode(mountingWall)
        SCNTransaction.flush()
        let projection = SiteCameraColorRenderer()
        #expect(projection.update(model: model, revision: revision, source: source,
            geometry: calibration, device: device, now: now))
        #expect(projection.status.contains("Mounting wall overlap"))
        let appearance = SiteFogRenderer()
        var settings = SiteFogConfiguration()
        _ = appearance.update(model: model, configuration: settings, robot: nil, color: projection)
        // Hide only in the observer's view, after generating the visibility map,
        // to inspect the wall that was behind the blocker in the camera image.
        blocker.isHidden = true
        mountingWall.isHidden = true
        let observer = SCNNode()
        observer.camera = SCNCamera()
        observer.camera?.fieldOfView = 90
        observer.camera?.zNear = 0.01
        observer.simdPosition = model.simdPosition
        observer.simdOrientation = model.simdOrientation
        scene.rootNode.addChildNode(observer)
        let renderer = SCNRenderer(device: device, options: nil)
        renderer.scene = scene; renderer.pointOfView = observer
        renderer.autoenablesDefaultLighting = true
        func snapshot(_ name: String) throws -> NSBitmapImageRep {
            let rendered = renderer.snapshot(atTime: 0, with: CGSize(width: 256, height: 256), antialiasingMode: .none)
            let data = try #require(rendered.tiffRepresentation)
            let bitmap = try #require(NSBitmapImageRep(data: data))
            try #require(bitmap.representation(using: .png, properties: [:]))
                .write(to: URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("wendy-camera-color-\(transformed)-\(name).png"))
            return bitmap
        }
        func rgb(_ bitmap: NSBitmapImageRep, _ x: Int, _ y: Int) throws -> [CGFloat] {
            let color = try #require(bitmap.colorAt(x: x, y: y))
            return [color.redComponent, color.greenComponent, color.blueComponent]
        }
        let projected = try snapshot("projected")
        let red = try rgb(projected, 70, 70), green = try rgb(projected, 186, 70), blue = try rgb(projected, 70, 186)
        #expect(red[0] > 0.8 && red[1] < 0.1 && red[2] < 0.1)
        #expect(green[1] > 0.8 && green[0] < 0.1 && green[2] < 0.1)
        #expect(blue[2] > 0.8 && blue[0] < 0.1 && blue[1] < 0.1)
        if transformed {
            // These pixels cross the quadrant boundaries only with the
            // off-center principal point, not with a symmetric FoV projection.
            let shiftedRed = try rgb(projected, 140, 70)
            let shiftedBlue = try rgb(projected, 70, 114)
            #expect(shiftedRed[0] > 0.8 && shiftedRed[1] < 0.1)
            #expect(shiftedBlue[2] > 0.8 && shiftedBlue[0] < 0.1)
        }
        let occluded = try rgb(projected, 120, 120)
        #expect(abs(occluded[0] - occluded[1]) < 0.02 && abs(occluded[1] - occluded[2]) < 0.02)
        #expect(occluded[0] > 0.2)

        if !transformed {
            let context = try #require(CGContext(data: nil, width: 64, height: 64, bitsPerComponent: 8,
                bytesPerRow: 64 * 4, space: CGColorSpace(name: CGColorSpace.sRGB)!,
                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
            context.setFillColor(CGColor(colorSpace: CGColorSpace(name: CGColorSpace.sRGB)!, components: [1, 0, 1, 1])!)
            context.fill(CGRect(x: 0, y: 0, width: 64, height: 64))
            let secondFrame = SiteCameraColorFrame(image: try #require(context.makeImage()), received: now, sequence: 2)
            let secondSource = SiteCameraColorSource(frame: { secondFrame }, acquire: { UUID() }, release: { _ in })
            var secondGeometry = calibration
            secondGeometry.lens?.fx = 64; secondGeometry.lens?.fy = 64
            let second = SiteCameraColorRenderer()
            #expect(second.update(model: model, revision: revision, source: secondSource,
                geometry: secondGeometry, device: device, now: now))
            _ = appearance.update(model: model, configuration: settings, robot: nil, colors: [projection, second])
            let combined = try snapshot("combined")
            let betterView = try rgb(combined, 80, 70)
            let wideOnly = try rgb(combined, 40, 70)
            #expect(betterView[0] > 0.8 && betterView[2] > 0.8 && betterView[1] < 0.1)
            #expect(wideOnly[0] > 0.8 && wideOnly[2] < 0.1 && wideOnly[1] < 0.1)
            _ = appearance.update(model: model, configuration: settings, robot: nil, color: projection)
        }

        settings.enabled = true
        _ = appearance.update(model: model, configuration: settings, robot: nil, color: projection)
        let fogged = try snapshot("fogged")
        #expect(try rgb(fogged, 70, 70).reduce(0, +) < 0.7)
        settings.enabled = false
        _ = appearance.update(model: model, configuration: settings, robot: nil, color: projection)
        #expect(try rgb(snapshot("unfogged"), 70, 70)[0] > 0.8)
        _ = appearance.update(model: model, configuration: settings, robot: nil)
        #expect(wall.geometry === untextured)
        #expect(wall.geometry?.sources(for: .texcoord).isEmpty == true)
        #expect(wall.geometry?.firstMaterial === original)
        let restored = try rgb(snapshot("restored"), 70, 70)
        #expect(abs(restored[0] - restored[1]) < 0.02)

        #expect(!projection.update(model: model, revision: UUID(), source: source,
            geometry: calibration, device: device, now: now))
        #expect(!projection.update(model: model, revision: revision, source: source,
            geometry: calibration, device: device, now: now.addingTimeInterval(4)))
        #expect(!projection.update(model: model, revision: revision, source: nil,
            geometry: calibration, device: device, now: now))
    }
    @Test func slopingUntexturedFloorHasContinuousColor() throws {
        let device = try #require(MTLCreateSystemDefaultDevice()), revision = UUID(), now = Date()
        var calibration = geometry(revision: revision, now: now)
        calibration.pose.position.y = 1
        let frame = SiteCameraColorFrame(image: try image(), received: now, sequence: 1)
        let source = SiteCameraColorSource(frame: { frame }, acquire: { UUID() }, release: { _ in })
        let scene = SCNScene(), model = SCNNode()
        let geometry = untexturedPlane(width: 6, height: 8)
        let floor = SCNNode(geometry: geometry)
        floor.eulerAngles.x = -.pi / 2; floor.position.z = -4
        model.addChildNode(floor); scene.rootNode.addChildNode(model)
        let projection = SiteCameraColorRenderer()
        #expect(projection.update(model: model, revision: revision, source: source,
            geometry: calibration, device: device, now: now))
        let appearance = SiteFogRenderer()
        _ = appearance.update(model: model, configuration: SiteFogConfiguration(), robot: nil, color: projection)
        let camera = SCNNode(); camera.camera = SCNCamera(); camera.camera?.fieldOfView = 90
        camera.position.y = 1; scene.rootNode.addChildNode(camera)
        let renderer = SCNRenderer(device: device, options: nil)
        renderer.scene = scene; renderer.pointOfView = camera; renderer.autoenablesDefaultLighting = true
        let rendered = renderer.snapshot(atTime: 0, with: CGSize(width: 256, height: 256), antialiasingMode: .none)
        let data = try #require(rendered.tiffRepresentation)
        let bitmap = try #require(NSBitmapImageRep(data: data))
        var colored = 0, samples = 0
        for y in 156..<184 { for x in 96..<112 {
            let pixel = try #require(bitmap.colorAt(x: x, y: y))
            if pixel.blueComponent > 0.8 && pixel.redComponent < 0.1 { colored += 1 }
            samples += 1
        } }
        #expect(Double(colored) / Double(samples) > 0.98)
    }

}
