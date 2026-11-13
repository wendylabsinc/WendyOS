import CoreGraphics
import MetalKit
@preconcurrency import SceneKit

public struct SiteCameraColorFrame: Sendable {
    let image: CGImage
    let received: Date
    let sequence: UInt64

    func matches(_ lens: SiteCameraLens, at now: Date) -> Bool {
        lens.isValid && image.width == lens.width && image.height == lens.height
            && (0...3).contains(now.timeIntervalSince(received))
    }
}

/// The scene owns decoding leases; twin capture manages video subscriptions.
@MainActor public final class SiteCameraColorSource: Identifiable {
    public let id = UUID()
    let frame: () -> SiteCameraColorFrame?
    let acquire: () -> UUID
    let release: (UUID) -> Void
    var unavailableReason: () -> String? = { nil }

    init(frame: @escaping () -> SiteCameraColorFrame?, acquire: @escaping () -> UUID,
         release: @escaping (UUID) -> Void, unavailableReason: @escaping () -> String? = { nil }) {
        self.frame = frame; self.acquire = acquire; self.release = release
        self.unavailableReason = unavailableReason
    }
}

/// Renders only the imported model from the physical camera to find the first
/// visible surface. Linear optical depth avoids far-plane precision artifacts.
@MainActor final class SiteCameraColorRenderer {
    private var modelID: ObjectIdentifier?
    private var transform: simd_float4x4?
    private var geometry: SiteCameraGeometry?
    private var depth: (any MTLTexture)?
    private var color: (any MTLTexture)?
    private var received: Date?
    private var sourceID: UUID?
    private var mountingWallOverlap = false
    private var extent = SIMD2<Float>(repeating: 1)
    private(set) var status = "Camera color off"

    func reset() {
        modelID = nil; transform = nil; geometry = nil; depth = nil
        color = nil; received = nil; sourceID = nil
    }

    func update(model: SCNNode, revision: UUID, source: SiteCameraColorSource?,
                geometry: SiteCameraGeometry?, device: (any MTLDevice)?, now: Date) -> Bool {
        guard let source else { reset(); status = "Camera color off"; return false }
        guard let geometry, geometry.pose.isCurrent(revision: revision, now: now),
              let lens = geometry.lens, lens.isValid, lens.boundaryRays != nil else {
            reset(); status = "Calibrate and align this camera to add color."; return false
        }
        guard let frame = source.frame() else {
            status = source.unavailableReason() ?? "Waiting for a color frame…"; return false
        }
        guard frame.matches(lens, at: now) else {
            status = frame.image.width != lens.width || frame.image.height != lens.height
                ? "Camera resolution changed. Recalibrate the lens."
                : "Waiting for a fresh color frame…"
            return false
        }
        guard let device else { status = "Camera color needs Metal rendering."; return false }
        if modelID != ObjectIdentifier(model) || transform != model.simdWorldTransform || self.geometry != geometry {
            depth = nil
            guard let result = makeDepth(model: model, geometry: geometry, device: device) else {
                status = "Could not check camera visibility."; return false
            }
            depth = result.texture; extent = result.extent
            modelID = ObjectIdentifier(model); transform = model.simdWorldTransform; self.geometry = geometry
        }
        if received != frame.received || sourceID != source.id || color == nil {
            do {
                color = try MTKTextureLoader(device: device).newTexture(cgImage: frame.image, options: [
                    .SRGB: true, .origin: MTKTextureLoader.Origin.topLeft.rawValue,
                    .generateMipmaps: false,
                ])
                received = frame.received; sourceID = source.id
            } catch { color = nil; status = "Could not load the camera color frame."; return false }
        }
        status = lens.measured ? "Live camera color" : "Live camera color · Estimated lens"
        if mountingWallOverlap { status += " · Mounting wall overlap" }
        return depth != nil && color != nil
    }

    func apply(to material: SCNMaterial, slot: Int = 0) {
        guard let geometry, let lens = geometry.lens, let depth, let color else { return }
        var opticalToWorld = simd_float4x4(simd_quatf(vector: SIMD4<Float>(geometry.pose.rotation)))
        let p = geometry.pose.position
        opticalToWorld.columns.3 = SIMD4(Float(p.x), Float(p.y), Float(p.z), 1)
        material.setValue(NSValue(scnMatrix4: SCNMatrix4(opticalToWorld.inverse)), forKey: "wendyColorWorldToCamera_\(slot)")
        material.setValue(SCNMaterialProperty(contents: color), forKey: "wendyCameraColor_\(slot)")
        material.setValue(SCNMaterialProperty(contents: depth), forKey: "wendyCameraDepth_\(slot)")
        material.setValue(NSValue(scnVector4: SCNVector4(lens.fx, lens.fy, lens.cx, lens.cy)), forKey: "wendyColorLens_\(slot)")
        material.setValue(NSValue(scnVector4: SCNVector4(Float(lens.width), Float(lens.height), extent.x, extent.y)), forKey: "wendyColorSize_\(slot)")
        let d = lens.distortionModel == "brown_conrady" ? lens.distortion : [0, 0, 0, 0, 0]
        material.setValue(NSValue(scnVector4: SCNVector4(d[0], d[1], d[2], d[3])), forKey: "wendyColorDistortion_\(slot)")
        material.setValue(NSNumber(value: d[4]), forKey: "wendyColorK3_\(slot)")
    }

    func makeDepth(model: SCNNode, geometry: SiteCameraGeometry, device: any MTLDevice)
        -> (texture: any MTLTexture, extent: SIMD2<Float>)? {
        guard let rays = geometry.lens?.boundaryRays, rays.allSatisfy({ $0.z > 0.001 }),
              let queue = device.makeCommandQueue(), let command = queue.makeCommandBuffer() else { return nil }
        let extent = SIMD2<Float>(Float(rays.map { abs($0.x / $0.z) }.max() ?? 1),
                                  Float(rays.map { abs($0.y / $0.z) }.max() ?? 1)) * 1.02
        guard extent.x > 0, extent.y > 0, extent.x.isFinite, extent.y.isFinite else { return nil }
        let scene = SCNScene()
        let copy = model.clone()
        copy.simdTransform = model.simdWorldTransform
        let material = SCNMaterial()
        material.lightingModel = .constant
        material.isDoubleSided = true
        material.blendMode = .replace
        material.shaderModifiers = [.fragment: "#pragma body\n_output.color = float4(-_surface.position.z, 0.0, 0.0, 1.0);"]
        func replace(_ node: SCNNode) {
            if let geometry = node.geometry?.copy() as? SCNGeometry {
                geometry.materials = Array(repeating: material, count: max(1, geometry.materials.count))
                node.geometry = geometry
            }
            node.camera = nil; node.light = nil
        }
        replace(copy); copy.enumerateChildNodes { node, _ in replace(node) }
        scene.rootNode.addChildNode(copy)
        SCNTransaction.flush()
        mountingWallOverlap = Self.excludeMountingWall(in: copy, pose: geometry.pose)
        let camera = SCNNode()
        camera.camera = SCNCamera()
        let pose = geometry.pose
        camera.simdPosition = SIMD3(Float(pose.position.x), Float(pose.position.y), Float(pose.position.z))
        camera.simdOrientation = simd_quatf(vector: SIMD4<Float>(pose.rotation)) * simd_quatf(angle: .pi, axis: SIMD3(1, 0, 0))
        let (low, high) = model.boundingBox
        var far: Float = 1
        for x in [low.x, high.x] { for y in [low.y, high.y] { for z in [low.z, high.z] {
            far = max(far, simd_distance(SIMD3<Float>(model.convertPosition(SCNVector3(x, y, z), to: nil)), camera.simdPosition) + 1)
        } } }
        let near: Float = 0.01
        camera.camera?.zNear = Double(near); camera.camera?.zFar = Double(far)
        camera.camera?.projectionTransform = SCNMatrix4(simd_float4x4(columns: (
            SIMD4(1 / extent.x, 0, 0, 0), SIMD4(0, 1 / extent.y, 0, 0),
            SIMD4(0, 0, -(far + near) / (far - near), -1), SIMD4(0, 0, -2 * far * near / (far - near), 0))))
        scene.rootNode.addChildNode(camera)
        let renderer = SCNRenderer(device: device, options: nil)
        renderer.scene = scene; renderer.pointOfView = camera
        let descriptor = MTLTextureDescriptor.texture2DDescriptor(pixelFormat: .r32Float, width: 1024, height: 1024, mipmapped: false)
        descriptor.usage = [.renderTarget, .shaderRead]
        guard let texture = device.makeTexture(descriptor: descriptor) else { return nil }
        descriptor.pixelFormat = .depth32Float
        descriptor.usage = [.renderTarget]
        guard let zBuffer = device.makeTexture(descriptor: descriptor) else { return nil }
        let pass = MTLRenderPassDescriptor()
        pass.colorAttachments[0].texture = texture
        pass.colorAttachments[0].loadAction = .clear; pass.colorAttachments[0].storeAction = .store
        pass.colorAttachments[0].clearColor = MTLClearColorMake(0, 0, 0, 0)
        pass.depthAttachment.texture = zBuffer
        pass.depthAttachment.loadAction = .clear; pass.depthAttachment.storeAction = .dontCare
        pass.depthAttachment.clearDepth = 1
        renderer.render(atTime: 0, viewport: CGRect(x: 0, y: 0, width: 1024, height: 1024), commandBuffer: command, passDescriptor: pass)
        command.commit(); command.waitUntilCompleted()
        guard command.status == .completed else { return nil }
        return (texture, extent)
    }

    /// RoomPlan walls have thickness, while fitted optical centers may lie
    /// inside that thickness. Exclude only the identified mounting wall crossed
    /// by the existing annotation-clearance correction, never other furniture
    /// or a general near-distance range. The optical projection stays unchanged.
    static func excludeMountingWall(in model: SCNNode, pose: SiteCameraPose) -> Bool {
        let origin = SIMD3<Float>(Float(pose.position.x), Float(pose.position.y), Float(pose.position.z))
        let cleared = SiteCameraWallPlacement.displayPosition(origin, forward: SIMD3<Float>(pose.forward), in: model)
        guard simd_distance(origin, cleared) > 0.001 else { return false }
        let hits = model.hitTestWithSegment(from: model.convertPosition(SCNVector3(origin), from: nil),
            to: model.convertPosition(SCNVector3(cleared), from: nil), options: [
                SCNHitTestOption.backFaceCulling.rawValue: false,
                SCNHitTestOption.searchMode.rawValue: SCNHitTestSearchMode.all.rawValue,
            ])
        var excluded = false
        for hit in hits {
            guard let name = hit.node.name, name.hasPrefix("Wall"),
                  !name.dropFirst(4).isEmpty, name.dropFirst(4).allSatisfy(\.isNumber) else { continue }
            hit.node.isHidden = true
            excluded = true
        }
        return excluded
    }

    static func shader(_ source: String, slot: Int) -> String {
        source.replacingOccurrences(of: "\\b(wendy(?:Camera|Color)\\w+)\\b", with: "$1_\(slot)", options: .regularExpression)
    }

    static let arguments = """
    texture2d<float> wendyCameraColor;
    texture2d<float> wendyCameraDepth;
    float4x4 wendyColorWorldToCamera;
    float4 wendyColorLens;
    float4 wendyColorSize;
    float4 wendyColorDistortion;
    float wendyColorK3;
    """

    static let body = """
    float3 colorWorld = (scn_frame.inverseViewTransform * float4(_surface.position, 1.0)).xyz;
    float3 optical = (wendyColorWorldToCamera * float4(colorWorld, 1.0)).xyz;
    float3 planeNormal = cross(dfdx(optical), dfdy(optical));
    if (optical.z > 0.01) {
        float2 p = optical.xy / optical.z;
        float r2 = dot(p, p);
        float radial = 1.0 + r2 * (wendyColorDistortion.x + r2 * (wendyColorDistortion.y + r2 * wendyColorK3));
        float2 distorted = p * radial + float2(
            2.0 * wendyColorDistortion.z * p.x * p.y + wendyColorDistortion.w * (r2 + 2.0 * p.x * p.x),
            wendyColorDistortion.z * (r2 + 2.0 * p.y * p.y) + 2.0 * wendyColorDistortion.w * p.x * p.y);
        float2 uv = (distorted * wendyColorLens.xy + wendyColorLens.zw) / wendyColorSize.xy;
        float2 depthUV = 0.5 + 0.5 * p / wendyColorSize.zw;
        if (all(uv >= 0.0) && all(uv <= 1.0) && all(depthUV >= 0.0) && all(depthUV <= 1.0)) {
            constexpr sampler depthSampler(coord::normalized, address::clamp_to_edge, filter::nearest);
            float visibleDepth = wendyCameraDepth.sample(depthSampler, depthUV).r;
            // Compare at the depth texel's ray, not the observer's fragment
            // ray. A fixed bias against optical.z stripes sloping walls/floors.
            float2 depthSize = float2(wendyCameraDepth.get_width(), wendyCameraDepth.get_height());
            float2 texelCenter = (floor(depthUV * depthSize) + 0.5) / depthSize;
            float3 depthRay = float3((texelCenter - 0.5) * 2.0 * wendyColorSize.zw, 1.0);
            float denominator = dot(planeNormal, depthRay);
            float expectedDepth = abs(denominator) > 1e-12 ? dot(planeNormal, optical) / denominator : optical.z;
            float tolerance = max(0.003, min(0.02, expectedDepth * 0.002));
            if (visibleDepth > 0.0 && expectedDepth > 0.01 && abs(expectedDepth - visibleDepth) <= tolerance) {
                constexpr sampler colorSampler(coord::normalized, address::clamp_to_edge, filter::linear);
                float3 worldNormal = (scn_frame.inverseViewTransform * float4(_surface.normal, 0.0)).xyz;
                float3 cameraNormal = (wendyColorWorldToCamera * float4(worldNormal, 0.0)).xyz;
                float quality = abs(dot(normalize(cameraNormal), normalize(-optical)))
                    * min(wendyColorLens.x, wendyColorLens.y) / optical.z;
                if (quality > bestCameraQuality) {
                    bestCameraQuality = quality;
                    _output.color.rgb = wendyCameraColor.sample(colorSampler, uv).rgb * _output.color.a;
                }
            }
        }
    }
    """
}
