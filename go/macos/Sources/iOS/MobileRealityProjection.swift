import RealityKit
import MetalKit

/// Static mesh extraction and optical depth rendering use RealityKit and Metal;
/// neither the depth pass nor the material depends on SceneKit.
@MainActor final class MobileRealityProjection {
    struct Batch {
        var vertices: [SIMD4<Float>]
        var wallNormal: SIMD3<Float>?
        var wallSurface: SIMD3<Float>?
    }
    struct DepthUniforms {
        var worldToOptical: simd_float4x4
        var extentNearFar: SIMD4<Float>
    }
    let device: any MTLDevice
    let library: any MTLLibrary
    let queue: any MTLCommandQueue
    private let pipeline: any MTLRenderPipelineState
    private let depthState: any MTLDepthStencilState
    private let model: Entity
    private let batches: [Batch]
    private var original: [(Entity, ModelComponent)] = []
    private(set) var atlas: LowLevelTexture?
    private var atlasResource: TextureResource?
    private var color: LowLevelTexture?
    private var colorResource: TextureResource?
    private var received: Date?
    private(set) var mountingWallOverlap = false
    private(set) var installed = false
    private var geometryKey = ""
    private let depthSize = 1024

    init(model: Entity) throws {
        guard let device = MTLCreateSystemDefaultDevice(), let queue = device.makeCommandQueue() else {
            throw WendyError(message: "Live projection needs Metal rendering.")
        }
        self.device = device; self.queue = queue; self.model = model
        library = try device.makeDefaultLibrary(bundle: .main)
        let descriptor = MTLRenderPipelineDescriptor()
        descriptor.vertexFunction = library.makeFunction(name: "wendyRealityDepthVertex")
        descriptor.fragmentFunction = library.makeFunction(name: "wendyRealityDepthFragment")
        descriptor.colorAttachments[0].pixelFormat = .rgba8Unorm
        descriptor.depthAttachmentPixelFormat = .depth32Float
        pipeline = try device.makeRenderPipelineState(descriptor: descriptor)
        let depthDescriptor = MTLDepthStencilDescriptor()
        depthDescriptor.isDepthWriteEnabled = true; depthDescriptor.depthCompareFunction = .less
        guard let state = device.makeDepthStencilState(descriptor: depthDescriptor) else { throw SiteError.invalidModel }
        depthState = state
        batches = try Self.extract(model)
        func retain(_ entity: Entity) {
            if let component = entity.components[ModelComponent.self] { original.append((entity, component)) }
            for child in entity.children { retain(child) }
        }
        retain(model)
    }

    static func key(_ geometry: SiteCameraGeometry) -> String {
        "\(geometry.pose.modelRevision)-\(geometry.pose.position)-\(geometry.pose.rotation)-\(String(describing: geometry.lens))"
    }

    func update(geometry: SiteCameraGeometry, source: SiteCameraColorSource, revision: UUID, now: Date) throws -> String {
        guard geometry.pose.isCurrent(revision: revision, now: now), let lens = geometry.lens, lens.isValid else {
            restore(); return "Camera alignment is no longer valid."
        }
        guard let frame = source.frame(), frame.matches(lens, at: now) else {
            restore()
            return "Waiting for fresh video at the aligned resolution…"
        }
        let key = Self.key(geometry)
        if geometryKey != key || atlas == nil {
            try renderDepth(geometry)
            geometryKey = key
        }
        if received != frame.received || color == nil {
            try upload(frame)
            received = frame.received
        }
        if !installed { try install() }
        return "Live projection" + (mountingWallOverlap ? " · Mounting wall overlap" : "")
    }

    func restore() {
        if installed {
            for (entity, component) in original { entity.components.set(component) }
            installed = false
        }
    }

    private func install() throws {
        guard let atlasResource, let colorResource else { return }
        let shader = CustomMaterial.SurfaceShader(named: "wendyRealityProjection", in: library)
        // Prepare every material before mutation, so an unsupported material
        // cannot leave a model with only some surfaces replaced.
        let replacements = try original.map { entity, component -> (Entity, ModelComponent) in
            var component = component
            component.materials = try component.materials.map { base in
                var material = try CustomMaterial(from: base, surfaceShader: shader)
                material.custom.texture = .init(atlasResource)
                material.emissiveColor.texture = .init(colorResource)
                return material
            }
            return (entity, component)
        }
        for (entity, component) in replacements { entity.components.set(component) }
        installed = true
    }

    private func upload(_ frame: SiteCameraColorFrame) throws {
        let source = try MTKTextureLoader(device: device).newTexture(cgImage: frame.image, options: [
            .SRGB: true, .origin: MTKTextureLoader.Origin.topLeft.rawValue, .generateMipmaps: false,
        ])
        if color?.descriptor.width != source.width || color?.descriptor.height != source.height || color?.descriptor.pixelFormat != source.pixelFormat {
            restore()
            let value = try LowLevelTexture(descriptor: .init(pixelFormat: source.pixelFormat,
                width: source.width, height: source.height, textureUsage: [.shaderRead]))
            color = value; colorResource = try TextureResource(from: value)
        }
        guard let command = queue.makeCommandBuffer(), let color, let blit = command.makeBlitCommandEncoder() else { throw SiteError.invalidModel }
        let target = color.replace(using: command)
        blit.copy(from: source, sourceSlice: 0, sourceLevel: 0, sourceOrigin: .init(x: 0, y: 0, z: 0),
            sourceSize: .init(width: source.width, height: source.height, depth: 1), to: target,
            destinationSlice: 0, destinationLevel: 0, destinationOrigin: .init(x: 0, y: 0, z: 0))
        blit.endEncoding(); command.commit()
    }

    func renderDepth(_ geometry: SiteCameraGeometry) throws {
        guard let lens = geometry.lens, let rays = lens.boundaryRays, rays.allSatisfy({ $0.z > 0.001 }) else { throw SiteLocalizationError.invalidResult }
        let extent = SIMD2<Float>(Float(rays.map { abs($0.x/$0.z) }.max() ?? 1), Float(rays.map { abs($0.y/$0.z) }.max() ?? 1)) * 1.02
        var opticalToWorld = simd_float4x4(simd_quatf(vector: SIMD4<Float>(geometry.pose.rotation)))
        let point = geometry.pose.position
        opticalToWorld.columns.3 = SIMD4(Float(point.x), Float(point.y), Float(point.z), 1)
        let matrix = opticalToWorld.inverse
        let excluded = Self.mountingWall(in: batches, pose: geometry.pose)
        mountingWallOverlap = excluded != nil
        let vertices = batches.enumerated().filter { $0.offset != excluded }.flatMap { $0.element.vertices }
        guard !vertices.isEmpty, let vertexBuffer = device.makeBuffer(bytes: vertices, length: vertices.count * MemoryLayout<SIMD4<Float>>.stride) else { throw SiteError.invalidModel }
        let far = vertices.reduce(Float(1)) { max($0, simd_distance(SIMD3($1.x,$1.y,$1.z), SIMD3(Float(point.x),Float(point.y),Float(point.z)))+1) }
        var uniforms = DepthUniforms(worldToOptical: matrix, extentNearFar: SIMD4(extent.x,extent.y,0.01,far))
        if atlas == nil {
            let value = try LowLevelTexture(descriptor: .init(pixelFormat: .rgba8Unorm, width: depthSize, height: depthSize+1, textureUsage: [.renderTarget,.shaderRead]))
            atlas = value; atlasResource = try TextureResource(from: value)
        }
        guard let atlas, let command = queue.makeCommandBuffer() else { throw SiteError.invalidModel }
        let texture = atlas.replace(using: command)
        let descriptor = MTLTextureDescriptor.texture2DDescriptor(pixelFormat: .depth32Float, width: depthSize, height: depthSize+1, mipmapped: false)
        descriptor.storageMode = .private; descriptor.usage = [.renderTarget]
        guard let depth = device.makeTexture(descriptor: descriptor) else { throw SiteError.invalidModel }
        let pass = MTLRenderPassDescriptor()
        pass.colorAttachments[0].texture = texture; pass.colorAttachments[0].loadAction = .clear; pass.colorAttachments[0].storeAction = .store
        pass.colorAttachments[0].clearColor = MTLClearColorMake(0,0,0,0)
        pass.depthAttachment.texture = depth; pass.depthAttachment.loadAction = .clear; pass.depthAttachment.storeAction = .dontCare; pass.depthAttachment.clearDepth = 1
        guard let render = command.makeRenderCommandEncoder(descriptor: pass) else { throw SiteError.invalidModel }
        render.setRenderPipelineState(pipeline); render.setDepthStencilState(depthState); render.setCullMode(.none)
        render.setViewport(MTLViewport(originX: 0, originY: 1, width: Double(depthSize), height: Double(depthSize), znear: 0, zfar: 1))
        render.setVertexBuffer(vertexBuffer, offset: 0, index: 0)
        render.setVertexBytes(&uniforms, length: MemoryLayout<DepthUniforms>.stride, index: 1)
        render.drawPrimitives(type: .triangle, vertexStart: 0, vertexCount: vertices.count)
        render.endEncoding()
        var parameters = (0..<4).flatMap { column in (0..<4).map { row in matrix[column][row] } }
        parameters += [Float(lens.fx),Float(lens.fy),Float(lens.cx),Float(lens.cy),Float(lens.width),Float(lens.height),extent.x,extent.y]
        parameters += lens.distortionModel == "brown_conrady" ? lens.distortion.map(Float.init) : [Float](repeating: 0, count: 5)
        parameters += [Float](repeating: 0, count: 64-parameters.count)
        let bytes = parameters.map(\.bitPattern)
        guard let buffer = device.makeBuffer(bytes: bytes, length: 256), let blit = command.makeBlitCommandEncoder() else { throw SiteError.invalidModel }
        blit.copy(from: buffer, sourceOffset: 0, sourceBytesPerRow: 256, sourceBytesPerImage: 256,
            sourceSize: MTLSize(width: 64,height: 1,depth: 1), to: texture, destinationSlice: 0, destinationLevel: 0, destinationOrigin: MTLOrigin(x: 0,y: 0,z: 0))
        blit.endEncoding(); command.commit(); command.waitUntilCompleted()
        guard command.status == .completed else { throw WendyError(message: "Could not check camera visibility.") }
    }

    static func extract(_ root: Entity) throws -> [Batch] {
        var batches: [Batch] = []
        var vertexCount = 0
        func visit(_ entity: Entity) throws {
            if let component = entity.components[ModelComponent.self] {
                let contents = component.mesh.contents
                let models = Dictionary(uniqueKeysWithValues: contents.models.map { ($0.id,$0) })
                let instances = contents.instances.isEmpty ? contents.models.map { MeshResource.Instance(id: $0.id, model: $0.id) } : Array(contents.instances)
                for instance in instances {
                    guard let model = models[instance.model] else { throw SiteError.invalidModel }
                    let transform = entity.transformMatrix(relativeTo: nil) * instance.transform
                    for part in model.parts {
                        guard let triangles = part.triangleIndices else { continue }
                        guard triangles.count <= 1_500_000 - vertexCount else {
                            throw WendyError(message: "This model is too detailed for the projection preview. Use a simplified model.")
                        }
                        vertexCount += triangles.count
                        let positions = part.positions.elements
                        var vertices: [SIMD4<Float>] = []
                        for index in triangles {
                            guard Int(index) < positions.count else { throw SiteError.invalidModel }
                            vertices.append(transform * SIMD4(positions[Int(index)],1))
                        }
                        guard vertices.count % 3 == 0 else { throw SiteError.invalidModel }
                        let wall = entity.name.hasPrefix("Wall") && !entity.name.dropFirst(4).isEmpty && entity.name.dropFirst(4).allSatisfy(\.isNumber)
                        let normal = simd_normalize(SIMD3(transform.columns.2.x,transform.columns.2.y,transform.columns.2.z))
                        let surface = transform * SIMD4<Float>(0,0,positions.map(\.z).max() ?? 0,1)
                        batches.append(Batch(vertices: vertices, wallNormal: wall ? normal : nil, wallSurface: wall ? SIMD3(surface.x,surface.y,surface.z) : nil))
                    }
                }
            }
            for child in entity.children { try visit(child) }
        }
        try visit(root)
        guard !batches.isEmpty else { throw SiteError.invalidModel }
        return batches
    }

    static func mountingWall(in batches: [Batch], pose: SiteCameraPose) -> Int? {
        let origin = SIMD3<Float>(Float(pose.position.x),Float(pose.position.y),Float(pose.position.z))
        var nearest: Float = .infinity
        var selected: Int?
        for (index,batch) in batches.enumerated() {
            guard let normal = batch.wallNormal, let surface = batch.wallSurface,
                  abs(normal.y)<0.2, simd_dot(normal,SIMD3<Float>(pose.forward))>0.2 else { continue }
            let distance = simd_dot(origin-surface,normal)
            guard distance<0.09, distance>=(-0.5) else { continue }
            let start = origin+normal*0.59, direction = -normal
            for offset in stride(from: 0, to: batch.vertices.count, by: 3) {
                let v = (0..<3).map { i -> SIMD3<Float> in let p=batch.vertices[offset+i]; return SIMD3(p.x,p.y,p.z) }
                let e1=v[1]-v[0],e2=v[2]-v[0],cross=simd_cross(direction,e2),det=simd_dot(e1,cross)
                guard abs(det)>1e-8, simd_dot(simd_normalize(simd_cross(e1,e2)),normal)>0.9 else { continue }
                let d=start-v[0],u=simd_dot(d,cross)/det,q=simd_cross(d,e1),v2=simd_dot(direction,q)/det,t=simd_dot(e2,q)/det
                guard u>=0,v2>=0,u+v2<=1,t>=0,t<=0.6 else { continue }
                let correction=simd_distance(origin,start+direction*t+normal*0.09)
                if correction<nearest { nearest=correction; selected=index }
            }
        }
        return selected
    }
}
