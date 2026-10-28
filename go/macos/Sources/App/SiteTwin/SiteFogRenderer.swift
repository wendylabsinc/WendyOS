@preconcurrency import SceneKit

/// Applies fog only to imported model materials. Robot, routes, sensors and
/// localization evidence remain visible and retain their existing hit targets.
@MainActor
final class SiteFogRenderer {
    private var originals: [(SCNGeometry, [SCNMaterial])] = []
    private var materials: [SCNMaterial] = []
    private(set) var grid: SiteFogGrid?
    private var configuration: SiteFogConfiguration?
    private var modelID: ObjectIdentifier?

    func update(model: SCNNode, configuration: SiteFogConfiguration,
                robot: SiteRobotRenderState?, now: Date = .now) -> SiteFogSummary {
        if self.configuration?.reset != configuration.reset ||
            self.configuration?.context != configuration.context || modelID != ObjectIdentifier(model) {
            restore()
            let (low, high) = model.boundingBox
            var minX = Double.infinity, maxX = -Double.infinity
            var minZ = Double.infinity, maxZ = -Double.infinity
            for x in [low.x, high.x] {
                for y in [low.y, high.y] {
                    for z in [low.z, high.z] {
                        let p = model.convertPosition(SCNVector3(x, y, z), to: nil)
                        minX = min(minX, Double(p.x)); maxX = max(maxX, Double(p.x))
                        minZ = min(minZ, Double(p.z)); maxZ = max(maxZ, Double(p.z))
                    }
                }
            }
            grid = SiteFogGrid(minX: minX, maxX: maxX, minZ: minZ, maxZ: maxZ)
            modelID = ObjectIdentifier(model)
        }
        self.configuration = configuration
        guard configuration.enabled else {
            restore()
            // Turning the effect off pauses recording and removes live visibility.
            grid?.observe(nil, fresh: false, canSee: { _, _ in false })
            return summary(receiving: false)
        }
        guard grid != nil else { return SiteFogSummary(exploredArea: 0, receiving: false, available: false) }
        let installed = materials.isEmpty
        if installed { install(on: model) }
        let fresh = robot?.fresh == true && robot?.placing != true && robot?.position?.isValid == true &&
            robot.map { (0...0.75).contains(now.timeIntervalSince($0.receivedAt)) } == true
        // Segment tests use SceneKit's committed transforms. Flush once per
        // update before casting, including the first frame of an imported model.
        SCNTransaction.flush()
        let changed = grid?.observe(robot?.position, fresh: fresh) { start, end in
            Self.canSee(in: model, from: start, to: end)
        } == true
        if changed || installed { updateTexture() }
        return summary(receiving: fresh && grid?.visible.isEmpty == false)
    }

    private func summary(receiving: Bool) -> SiteFogSummary {
        SiteFogSummary(exploredArea: grid?.exploredArea ?? 0, receiving: receiving, available: grid != nil)
    }

    static func canSee(in model: SCNNode, from start: SitePoint, to end: SitePoint) -> Bool {
        let a = model.convertPosition(SCNVector3(start.x, start.y, start.z), from: nil)
        let b = model.convertPosition(SCNVector3(end.x, end.y, end.z), from: nil)
        return model.hitTestWithSegment(from: a, to: b, options: [
            SCNHitTestOption.backFaceCulling.rawValue: false,
            SCNHitTestOption.clipToZRange.rawValue: false,
            SCNHitTestOption.ignoreChildNodes.rawValue: false,
            SCNHitTestOption.searchMode.rawValue: SCNHitTestSearchMode.any.rawValue,
        ]).isEmpty
    }

    private func install(on model: SCNNode) {
        func apply(_ node: SCNNode) {
            guard let geometry = node.geometry else { return }
            // Geometry may be shared by imported instances. Copy it before
            // replacing materials so the source scene remains untouched.
            guard let copy = geometry.copy() as? SCNGeometry else { return }
            node.geometry = copy
            originals.append((copy, copy.materials))
            copy.materials = (copy.materials.isEmpty ? [SCNMaterial()] : copy.materials).map { original in
                guard let material = original.copy() as? SCNMaterial else { return original }
                var modifiers = material.shaderModifiers ?? [:]
                // Imported USDZ materials ordinarily have no shader modifiers.
                // Keep other stages; restore every original material on disable.
                modifiers[.fragment] = Self.fragment
                material.shaderModifiers = modifiers
                materials.append(material)
                return material
            }
        }
        apply(model)
        model.enumerateChildNodes { node, _ in apply(node) }
    }

    func restore() {
        for (geometry, materials) in originals { geometry.materials = materials }
        originals = []; materials = []
    }

    private func updateTexture() {
        guard let grid, let provider = CGDataProvider(data: Data(grid.pixels) as CFData),
              let image = CGImage(width: grid.width, height: grid.height,
                bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: grid.width*4,
                space: CGColorSpaceCreateDeviceRGB(),
                bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedLast.rawValue),
                provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent) else { return }
        let texture = SCNMaterialProperty(contents: image)
        texture.minificationFilter = .nearest; texture.magnificationFilter = .nearest
        texture.wrapS = .clamp; texture.wrapT = .clamp
        let bounds = SCNVector4(grid.minX, grid.minZ, Double(grid.width)*grid.cellSize, Double(grid.height)*grid.cellSize)
        for material in materials {
            material.setValue(texture, forKey: "wendyFogMask")
            material.setValue(NSValue(scnVector4: bounds), forKey: "wendyFogBounds")
        }
    }

    static let fragment = """
    #pragma arguments
    texture2d<float> wendyFogMask;
    float4 wendyFogBounds;
    #pragma body
    float3 world = (scn_frame.inverseViewTransform * float4(_surface.position, 1.0)).xyz;
    float2 uv = (world.xz - wendyFogBounds.xy) / wendyFogBounds.zw;
    constexpr sampler fogSampler(coord::normalized, address::clamp_to_edge, filter::nearest);
    float2 coverage = wendyFogMask.sample(fogSampler, uv).rg;
    float brightness = coverage.g > 0.5 ? 1.0 : (coverage.r > 0.5 ? 0.32 : 0.0);
    _output.color.rgb = mix(float3(0.018, 0.024, 0.038) * _output.color.a,
                            _output.color.rgb, brightness);
    """
}

private extension SiteFogSummary {
    init(exploredArea: Double, receiving: Bool, available: Bool) {
        self.init()
        self.exploredArea = exploredArea; self.receiving = receiving; self.available = available
    }
}
