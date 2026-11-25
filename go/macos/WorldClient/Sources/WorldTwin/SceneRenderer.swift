import SceneKit
import SwiftUI
import simd

@MainActor final class DemoSceneView: SCNView {
    weak var renderer:SceneRenderer?
    override func mouseDragged(with event:NSEvent) {
        if renderer?.following == true { renderer?.followInput(horizontal:Double(event.deltaX),zoom:0) }
        else { super.mouseDragged(with:event) }
    }
    override func scrollWheel(with event:NSEvent) {
        if renderer?.following == true { renderer?.followInput(horizontal:0,zoom:Double(event.scrollingDeltaY)) }
        else { super.scrollWheel(with:event) }
    }
}

@MainActor final class SceneRenderer: NSObject {
    let view = DemoSceneView()
    let scene = SCNScene()
    let world = SCNNode()
    let camera = SCNNode()
    var loaded = false
    private var nodes: [Int:SCNNode] = [:]
    private let building = SCNNode()
    private var groups: [DemoLayer:SCNNode] = [:]
    private var lastSequence = -1
    private var lastZoneVisualState = ""
    var renderedSequence: Int { lastSequence }
    private var lastSelectedRobot = ""
    private var lastSelectedTrack:String?
    private var lastSelectedOriginal:String?
    private var lastInspectionRouteRevision = -1
    private var lastCameraRevision = -1
    private var lastLayers: Set<DemoLayer> = []
    private weak var model: DemoModel?
    private var followYaw = Double.pi / 4
    private var followDistance = 8.0
    private let followElevation = 0.60
    private var followTarget = SIMD3<Float>(0,0.3,0)
    var following:Bool { model?.camera == .robot }

    override init() {
        super.init()
        view.renderer=self
        scene.background.contents = NSColor(red:0.91,green:0.92,blue:0.91,alpha:1)
        view.scene = scene
        view.antialiasingMode = .multisampling4X
        view.allowsCameraControl = true
        view.autoenablesDefaultLighting = false
        view.preferredFramesPerSecond = 30
        view.rendersContinuously = true
        view.defaultCameraController.inertiaEnabled = true
        view.defaultCameraController.interactionMode = .orbitTurntable
        view.defaultCameraController.minimumVerticalAngle = 12
        view.defaultCameraController.maximumVerticalAngle = 88
        world.eulerAngles.x = -.pi / 2
        scene.rootNode.addChildNode(world)
        world.addChildNode(building)
        for layer in DemoLayer.allCases where layer != .building {
            let node = SCNNode(); world.addChildNode(node); groups[layer] = node
        }
        camera.camera = SCNCamera()
        camera.camera?.zNear = 0.05; camera.camera?.zFar = 150
        camera.camera?.fieldOfView = 42
        camera.camera?.wantsHDR = true
        camera.camera?.exposureOffset = -0.15
        camera.camera?.bloomIntensity = 0.07
        camera.camera?.screenSpaceAmbientOcclusionIntensity = 0.55
        camera.camera?.screenSpaceAmbientOcclusionRadius = 0.4
        scene.rootNode.addChildNode(camera); view.pointOfView = camera
        let ambient = SCNNode(); ambient.light = SCNLight(); ambient.light?.type = .ambient
        ambient.light?.color = NSColor(red:0.86,green:0.89,blue:0.95,alpha:1); ambient.light?.intensity = 520
        scene.rootNode.addChildNode(ambient)
        let sun = SCNNode(); sun.light = SCNLight(); sun.light?.type = .directional
        sun.light?.color = NSColor(red:1,green:0.94,blue:0.84,alpha:1); sun.light?.intensity = 1400
        sun.eulerAngles = SCNVector3(-Float.pi / 3.4,-Float.pi / 4,0)
        sun.light?.castsShadow = true; sun.light?.shadowMode = .deferred
        sun.light?.shadowMapSize = CGSize(width:2048,height:2048)
        sun.light?.shadowRadius = 5; sun.light?.shadowSampleCount = 16
        sun.light?.shadowColor = NSColor(white:0.19,alpha:0.2)
        sun.light?.orthographicScale = 25; sun.light?.maximumShadowDistance = 50
        scene.rootNode.addChildNode(sun)
        let click = NSClickGestureRecognizer(target:self, action:#selector(clicked(_:)))
        view.addGestureRecognizer(click)
        preset(.perspective, robot:nil)
    }
    func update(_ model: DemoModel, force: Bool = false) {
        self.model = model
        if !loaded, let geometry = model.geometry { load(geometry) }
        if lastCameraRevision != model.cameraRevision {
            lastCameraRevision = model.cameraRevision; preset(model.camera, robot:model.robot)
        }
        let changedLayers = lastLayers != model.layers
        if changedLayers {
            lastLayers = model.layers
            building.isHidden = !model.layers.contains(.building)
            for (layer,node) in groups { node.isHidden = !model.layers.contains(layer) }
        }
        var zoneVisualChanged = false
        #if canImport(WorldClient)
        let zoneVisualState = (model.zones.active?.id ?? "") + String(model.zones.activeOverlayCurrent)
        zoneVisualChanged = zoneVisualState != lastZoneVisualState
        lastZoneVisualState = zoneVisualState
        #endif
        guard let state = model.state, force || zoneVisualChanged || state.sequence != lastSequence || changedLayers || model.selectedRobot != lastSelectedRobot || model.selectedTrack != lastSelectedTrack || model.selectedOriginal?.id != lastSelectedOriginal || model.inspectionRouteRevision != lastInspectionRouteRevision else { return }
        lastSequence = state.sequence
        lastSelectedRobot = model.selectedRobot
        lastSelectedTrack = model.selectedTrack
        lastSelectedOriginal = model.selectedOriginal?.id
        lastInspectionRouteRevision = model.inspectionRouteRevision
        SCNTransaction.begin(); SCNTransaction.animationDuration = 0
        SCNTransaction.disableActions = true
        for transform in state.dynamic_geoms {
            if let node = nodes[transform.id] { pose(node, position:transform.position, rotation:transform.rotation) }
        }
        overlays(state.snapshot, selected:model.selectedRobot)
        if model.camera == .robot { moveFollowTarget(model.robot) }
        SCNTransaction.commit()
        if force { SCNTransaction.flush() }
    }
    private func load(_ geometry: DemoGeometry) {
        for geom in geometry.geoms {
            // MuJoCo groups 3–5 contain collision/debug geometry, not the visual robot.
            if geom.robot_id != nil && geom.group>=3 {continue}
            guard geom.size.count == 3, geom.rgba.count == 4, geom.rgba[3] > 0.001 else { continue }
            let shape: SCNGeometry
            let size = geom.size.map { CGFloat($0) }
            let correction = SCNNode()
            switch geom.type {
            case "box": shape = SCNBox(width:size[0]*2,height:size[1]*2,length:size[2]*2,chamferRadius:min(size.min()! * 0.15,0.025))
            case "sphere": let sphere = SCNSphere(radius:size[0]); sphere.segmentCount = 16; shape = sphere
            case "ellipsoid": shape = SCNSphere(radius:1); correction.scale = SCNVector3(Float(size[0]),Float(size[1]),Float(size[2]))
            case "cylinder": let cylinder = SCNCylinder(radius:size[0],height:size[1]*2); cylinder.radialSegmentCount = 20; shape = cylinder; correction.eulerAngles.x = .pi/2
            case "capsule": let capsule = SCNCapsule(capRadius:size[0],height:2*(size[1]+size[0])); capsule.radialSegmentCount = 16; shape = capsule; correction.eulerAngles.x = .pi/2
            case "mesh":
                guard let vertices=geom.vertices, let faces=geom.faces, !vertices.isEmpty,
                      vertices.count<=200000, faces.count<=400000,
                      vertices.allSatisfy({$0.count==3 && $0.allSatisfy(\.isFinite)}),
                      faces.allSatisfy({$0.count==3 && $0.allSatisfy({$0>=0 && $0<vertices.count})}) else {continue}
                let source=SCNGeometrySource(vertices:vertices.map { vector($0) })
                let positions=vertices.map {SIMD3<Float>(Float($0[0]),Float($0[1]),Float($0[2]))}
                var normals=Array(repeating:SIMD3<Float>(0,0,0),count:vertices.count)
                for face in faces {
                    let normal=simd_cross(positions[face[1]]-positions[face[0]],positions[face[2]]-positions[face[0]])
                    for index in face {normals[index] += normal}
                }
                let normalSource=SCNGeometrySource(normals:normals.map {SCNVector3(simd_length_squared($0)>0 ? simd_normalize($0):SIMD3(0,0,1))})
                let element=SCNGeometryElement(indices:faces.flatMap {$0}.map(Int32.init),primitiveType:.triangles)
                shape=SCNGeometry(sources:[source,normalSource],elements:[element])
            case "plane": shape = SCNPlane(width:max(size[0]*2,18),height:max(size[1]*2,14))
            default: continue
            }
            shape.firstMaterial = material(geom)
            correction.geometry = shape
            let node = SCNNode(); node.name = geom.robot_id.map { "robot:\($0)" } ?? geom.name
            node.addChildNode(correction)
            node.castsShadow = geom.rgba[3] > 0.8
            pose(node, position:geom.position, rotation:geom.rotation)
            nodes[geom.id] = node
            (geom.robot_id == nil ? building : world).addChildNode(node)
        }
        for space in geometry.presentation?.labels ?? [] {
            guard space.position.count == 3 else { continue }
            let node = label(space.text, position:[space.position[0],space.position[1],2.9], size:0.18)
            groups[.labels]?.addChildNode(node)
        }
        loaded = true
    }
    private var textures: [String:NSImage] = [:]
    private func texture(_ kind: String) -> NSImage {
        if let cached = textures[kind] { return cached }
        let image = NSImage(size:CGSize(width:256,height:256))
        image.lockFocus()
        NSColor(white:0.96,alpha:1).setFill(); NSRect(x:0,y:0,width:256,height:256).fill()
        // Deterministic surface detail only; no sensor or geometry values are synthesized.
        for i in 0..<128 {
            let y = CGFloat(i*2)
            let path = NSBezierPath(); path.lineWidth = kind == "wood" ? 0.5 : 0.25
            NSColor(white:0.28,alpha:kind == "wood" ? 0.06 : 0.035).setStroke()
            path.move(to:NSPoint(x:0,y:y)); path.curve(to:NSPoint(x:256,y:y+CGFloat(i%5-2)),controlPoint1:NSPoint(x:75,y:y+3),controlPoint2:NSPoint(x:160,y:y-2)); path.stroke()
            if kind == "fabric" { let line=NSBezierPath(); line.move(to:NSPoint(x:y,y:0));line.line(to:NSPoint(x:y,y:256));line.lineWidth=0.3;line.stroke() }
        }
        image.unlockFocus(); textures[kind] = image; return image
    }
    private func material(_ geom: DemoGeometry.Geom) -> SCNMaterial {
        let m = SCNMaterial(); m.lightingModel = .physicallyBased
        let rgba = geom.rgba
        let average = geom.material?.texture_average_rgb ?? [1,1,1]
        m.diffuse.contents = NSColor(red:rgba[0]*average[0],green:rgba[1]*average[1],blue:rgba[2]*average[2],alpha:rgba[3])
        m.roughness.contents = max(0.22,1-(geom.material?.shininess ?? 0.22))
        m.specular.contents = geom.material?.specular ?? 0.5
        m.metalness.contents = min(0.3,geom.material?.reflectance ?? 0.03)
        if let emission=geom.material?.emission, emission>0 {
            m.emission.contents=NSColor(red:rgba[0]*emission,green:rgba[1]*emission,blue:rgba[2]*emission,alpha:1)
        }
        m.isDoubleSided = rgba[3] < 0.95
        m.transparency = rgba[3]
        let name = (geom.material_name ?? geom.name).lowercased()
        if name.contains("wood") || name.contains("oak") || name.contains("desk") || name.contains("table") {
            m.multiply.contents = texture("wood"); m.roughness.contents = 0.53
        } else if name.contains("fabric") || name.contains("seat") || name.contains("rug") || name.contains("carpet") {
            m.multiply.contents = texture("fabric"); m.roughness.contents = 0.95
        } else if name.contains("glass") { m.roughness.contents = 0.17; m.metalness.contents = 0.12 }
        return m
    }
    private func pose(_ node:SCNNode, position:[Double], rotation:[Double]) {
        guard position.count == 3, rotation.count == 9 else { return }
        node.simdTransform = simd_float4x4(columns:(
            SIMD4(Float(rotation[0]),Float(rotation[3]),Float(rotation[6]),0),
            SIMD4(Float(rotation[1]),Float(rotation[4]),Float(rotation[7]),0),
            SIMD4(Float(rotation[2]),Float(rotation[5]),Float(rotation[8]),0),
            SIMD4(Float(position[0]),Float(position[1]),Float(position[2]),1)))
    }
    private func clear(_ layer: DemoLayer) { groups[layer]?.childNodes.forEach { $0.removeFromParentNode() } }
    private func color(_ rgb:[Double], alpha:Double=1)->NSColor { NSColor(red:rgb[0],green:rgb[1],blue:rgb[2],alpha:alpha) }
    private func sphere(_ p:[Double], radius:CGFloat, color:NSColor)->SCNNode {
        let s=SCNSphere(radius:radius);s.segmentCount=12; let n=SCNNode(geometry:s);n.position=vector(p);s.firstMaterial=flat(color);return n
    }
    private func flat(_ color:NSColor)->SCNMaterial { let m=SCNMaterial();m.diffuse.contents=color;m.emission.contents=color;m.lightingModel = .constant;return m }
    private func vector(_ p:[Double], z:Double?=nil)->SCNVector3 { SCNVector3(Float(p[0]),Float(p[1]),Float(z ?? (p.count>2 ? p[2]:0))) }
    private func line(_ a:[Double],_ b:[Double],color:NSColor,radius:CGFloat=0.018)->SCNNode {
        let av=SIMD3<Float>(Float(a[0]),Float(a[1]),Float(a[2]));let bv=SIMD3<Float>(Float(b[0]),Float(b[1]),Float(b[2]))
        let delta=bv-av;let length=simd_length(delta)
        let geom=SCNCylinder(radius:radius,height:CGFloat(length));geom.radialSegmentCount=6;geom.firstMaterial=flat(color)
        let node=SCNNode(geometry:geom);node.simdPosition=(av+bv)/2
        if length>0 { node.simdOrientation=simd_quatf(from:SIMD3(0,1,0),to:delta/length) }
        return node
    }
    private func points(_ positions:[[Double]],colors:[NSColor],size:CGFloat)->SCNNode {
        let vertices=positions.map { vector($0) }
        let source=SCNGeometrySource(vertices:vertices)
        var floats:[Float]=[];floats.reserveCapacity(colors.count*4)
        for c in colors { floats += [Float(c.redComponent),Float(c.greenComponent),Float(c.blueComponent),Float(c.alphaComponent)] }
        let data=floats.withUnsafeBufferPointer { Data(buffer:$0) }
        let colorSource=SCNGeometrySource(data:data,semantic:.color,vectorCount:colors.count,usesFloatComponents:true,componentsPerVector:4,bytesPerComponent:4,dataOffset:0,dataStride:16)
        let indices=(0..<vertices.count).map(UInt32.init)
        let element=SCNGeometryElement(indices:indices,primitiveType:.point)
        element.pointSize=size;element.minimumPointScreenSpaceRadius=size/2;element.maximumPointScreenSpaceRadius=size
        let geometry=SCNGeometry(sources:[source,colorSource],elements:[element]);geometry.firstMaterial=flat(.white)
        return SCNNode(geometry:geometry)
    }
    private func overlays(_ state:Snapshot,selected:String) {
        for layer in [DemoLayer.lidar,.voxels,.routes,.links,.jobs,.observations,.hazards,.tracks,.coverage,.localization] { clear(layer) }
        let palette=Dictionary(uniqueKeysWithValues:state.robots.map { ($0.id,color($0.color)) })
        for robot in state.robots {
            let c=palette[robot.id] ?? .systemTeal
            if model?.layers.contains(.lidar) == true, !robot.lidar_points.isEmpty {
                groups[.lidar]?.addChildNode(points(robot.lidar_points,colors:Array(repeating:c, count:robot.lidar_points.count),size:3))
            }
            if model?.layers.contains(.routes) == true {
                let path=model?.patrolMode == true ? robot.path:[robot.position]+robot.path
                for (a,b) in zip(path,path.dropFirst()) where a.count>=2 && b.count>=2 {
                    groups[.routes]?.addChildNode(line([a[0],a[1],0.06],[b[0],b[1],0.06],color:c.withAlphaComponent(0.85),radius:0.026))
                }
            }
            if robot.id == selected { let ring=SCNTorus(ringRadius:0.42,pipeRadius:0.015);ring.firstMaterial=flat(c);let n=SCNNode(geometry:ring);n.position=vector(robot.position,z:0.065);n.eulerAngles.x = .pi/2;groups[.routes]?.addChildNode(n) }
        }
        if model?.layers.contains(.voxels) == true, !state.voxels.isEmpty {
            let cells=Array(state.voxels.suffix(12000))
            groups[.voxels]?.addChildNode(points(cells.map(\.position),colors:cells.map { (palette[$0.source] ?? .systemTeal).withAlphaComponent(0.65) },size:5))
        }
        if model?.layers.contains(.jobs) == true {
            for job in state.jobs {
                let c:NSColor=job.state == "completed" ? .systemGreen : .systemOrange
                groups[.jobs]?.addChildNode(sphere([job.position[0],job.position[1],0.16],radius:0.085,color:c))
                let ring=SCNTorus(ringRadius:0.25,pipeRadius:0.012);ring.firstMaterial=flat(c);let n=SCNNode(geometry:ring);n.position=vector(job.position,z:0.045);n.eulerAngles.x = .pi/2;groups[.jobs]?.addChildNode(n)
            }
        }
        if model?.layers.contains(.links) == true {
            var positions=Dictionary(uniqueKeysWithValues:state.robots.map { ($0.id,[$0.position[0],$0.position[1],0.7]) })
            positions["hub"]=[0,0,1.2];positions["internet"]=[0,7,2.2]
            groups[.links]?.addChildNode(sphere(positions["hub"]!,radius:0.12,color:.systemTeal))
            for edge in state.network.edges + [state.network.internet] {
                guard let a=positions[edge.a], let b=positions[edge.b] else { continue }
                if edge.in_range == false { continue }
                let c:NSColor=edge.up ? .init(red:0.12,green:0.49,blue:0.49,alpha:0.65) : .init(red:0.76,green:0.32,blue:0.27,alpha:0.55)
                for step in 0..<18 {
                    if !edge.up && step%2==0 { continue }
                    func arc(_ i:Int)->[Double] { let t=Double(i)/18;return zip(a,b).enumerated().map { axis, pair in pair.0*(1-t)+pair.1*t+(axis==2 ? sin(t * .pi)*0.75:0) } }
                    let segment=line(arc(step),arc(step+1),color:c,radius:0.018);segment.name="edge:"+edge.id;groups[.links]?.addChildNode(segment)
                }
            }
        }
        fusionOverlays(state.fusion)
        trackOverlays(state.tracking,at:state.time_s)
        inspectionRouteOverlay()
        patrolOverlays(state.patrol,at:state.time_s)
    }
    private func patrolOverlays(_ patrol:PatrolState?,at time:Double) {
        guard let patrol,patrol.valid else{return}
        if model?.layers.contains(.routes)==true,let policy=patrol.exclusions {
            for rect in policy.rectangles where rect.min_mm.count==2 && rect.max_mm.count==2 {
                let x0=Double(rect.min_mm[0])/1000,y0=Double(rect.min_mm[1])/1000,x1=Double(rect.max_mm[0])/1000,y1=Double(rect.max_mm[1])/1000
                let corners=[[x0,y0,0.05],[x1,y0,0.05],[x1,y1,0.05],[x0,y1,0.05],[x0,y0,0.05]]
                for (a,b) in zip(corners,corners.dropFirst()) {groups[.routes]?.addChildNode(line(a,b,color:.systemRed,radius:0.035))}
                let box=SCNBox(width:x1-x0,height:0.035,length:y1-y0,chamferRadius:0)
                box.firstMaterial=flat(NSColor.systemRed.withAlphaComponent(0.22))
                let node=SCNNode(geometry:box);node.position=vector([(x0+x1)/2,(y0+y1)/2,0.03]);node.name="restricted-area:"+rect.id
                groups[.routes]?.addChildNode(node)
            }
        }
        if model?.layers.contains(.coverage)==true {
            for room in patrol.rooms {
                let blocked=room.blocked(at:time)
                let age=room.last_observed_s.map {max(0,time-$0)}
                let tint:NSColor=blocked ? .systemRed:(age == nil ? .systemGray:((age ?? 0)>180 ? .systemOrange:.systemTeal))
                let group=SCNNode();group.name="coverage:"+room.id
                let polygon=room.polygon.map {[$0[0],$0[1],0.045]}
                for (a,b) in zip(polygon,Array(polygon.dropFirst())+[polygon[0]]) {
                    group.addChildNode(line(a,b,color:tint.withAlphaComponent(0.7),radius:0.025))
                }
                let center=[room.polygon.map {$0[0]}.reduce(0,+)/Double(room.polygon.count),
                    room.polygon.map {$0[1]}.reduce(0,+)/Double(room.polygon.count),0.12]
                group.addChildNode(sphere(center,radius:0.075,color:tint))
                groups[.coverage]?.addChildNode(group)
            }
        }
        if model?.layers.contains(.localization)==true,let estimate=patrol.localization,estimate.valid {
            let tint=NSColor.systemPurple
            let p=estimate.position
            let group=SCNNode();group.name="estimated-navigation-pose"
            group.addChildNode(sphere([p[0],p[1],0.10],radius:0.07,color:tint))
            group.addChildNode(line([p[0],p[1],0.10],[p[0]+0.55*cos(estimate.yaw),p[1]+0.55*sin(estimate.yaw),0.10],color:tint,radius:0.025))
            let xx=estimate.covariance[0],xy=estimate.covariance[1],yy=estimate.covariance[3]
            let difference=sqrt(max(0,(xx-yy)*(xx-yy)+4*xy*xy))
            let major=2*sqrt(max(0,(xx+yy+difference)/2)),minor=2*sqrt(max(0,(xx+yy-difference)/2))
            let angle=0.5*atan2(2*xy,xx-yy)
            func point(_ i:Int)->[Double] {
                let a=Double(i)*2 * .pi/64,x=major*cos(a),y=minor*sin(a)
                return [p[0]+x*cos(angle)-y*sin(angle),p[1]+x*sin(angle)+y*cos(angle),0.08]
            }
            for i in 0..<64 {group.addChildNode(line(point(i),point(i+1),color:tint.withAlphaComponent(0.7),radius:0.012))}
            group.addChildNode(label("AMCL · 2σ",position:[p[0],p[1],0.78],size:0.055))
            groups[.localization]?.addChildNode(group)
        }
    }
    private func inspectionRouteOverlay() {
        guard model?.layers.contains(.routes)==true,let preview=model?.visibleInspectionRoute else{return}
        let group=SCNNode();group.name="inspection-route-preview"
        let teal=NSColor(red:0.10,green:0.52,blue:0.47,alpha:1)
        func scenePoint(_ p:[Double],lift:Double=0)->[Double] { [p[0],-p[2],p[1]+lift] }
        for (a,b) in zip(preview.route,preview.route.dropFirst()) where a != b {
            let segment=line(scenePoint(a,lift:0.04),scenePoint(b,lift:0.04),color:teal,radius:0.028)
            segment.name="inspection-route-segment";group.addChildNode(segment)
        }
        let goal=scenePoint(preview.target,lift:0.04)
        let ring=SCNTorus(ringRadius:0.28,pipeRadius:0.025);ring.firstMaterial=flat(teal)
        let destination=SCNNode(geometry:ring);destination.name="inspection-destination"
        destination.position=vector(goal);destination.eulerAngles.x = .pi/2;group.addChildNode(destination)
        let original=scenePoint(preview.observed)
        let marker=sphere(original,radius:0.075,color:.systemOrange);marker.name="inspection-observed-location";group.addChildNode(marker)
        if zip(original,goal).map({pow($0-$1,2)}).reduce(0,+)>0.0001 {
            for i in stride(from:0,to:12,by:2) {
                func between(_ t:Double)->[Double] { zip(original,goal).map { $0*(1-t)+$1*t } }
                let dash=line(between(Double(i)/12),between(Double(i+1)/12),color:.systemOrange.withAlphaComponent(0.65),radius:0.012)
                group.addChildNode(dash)
            }
        }
        if model?.layers.contains(.labels)==true {
            group.addChildNode(label("Proposed inspection point",position:scenePoint(preview.target,lift:0.65),size:0.13))
        }
        groups[.routes]?.addChildNode(group)
    }
    private func trackOverlays(_ tracking:TrackingState?,at seconds:Double) {
        guard let tracking,model?.layers.contains(.tracks)==true else{return}
        #if canImport(WorldClient)
        if let model {
          for z in model.zones.activeOverlay {
            let a=[z.minXZ[0],-z.minXZ[1],0.07],b=[z.maxXZ[0],-z.minXZ[1],0.07],c=[z.maxXZ[0],-z.maxXZ[1],0.07],d=[z.minXZ[0],-z.maxXZ[1],0.07]
            let current = model.zones.activeOverlayCurrent
            for (p,q) in [(a,b),(b,c),(c,d),(d,a)] {
              let edge=line(p,q,color:current ? .systemTeal : .systemGray,radius:0.02)
              edge.name=(current ? "zone-active:" : "zone-historical:")+z.id
              groups[.tracks]?.addChildNode(edge)
            }
          }
          for z in model.zones.overlay {
            let a=[z.minXZ[0],-z.minXZ[1],0.055],b=[z.maxXZ[0],-z.minXZ[1],0.055],c=[z.maxXZ[0],-z.maxXZ[1],0.055],d=[z.minXZ[0],-z.maxXZ[1],0.055]
            for (p,q) in [(a,b),(b,c),(c,d),(d,a)]{let edge=line(p,q,color:.systemPurple,radius:0.012);edge.name="zone-draft:"+z.id;groups[.tracks]?.addChildNode(edge)}
        }}
        #endif
        for track in tracking.live(at:seconds) {
            let group=SCNNode();group.name="track:"+track.id
            let point=track.scenePosition;let tint=track.tint
            let selected=model?.selectedTrack==track.id && (model?.selectedOriginal == nil || model?.selectedOriginalEnvelopeSHA256==track.originalEnvelopeSHA256)
            let radius=selected ? 0.23:0.17
            let ring=SCNTorus(ringRadius:radius,pipeRadius:0.018);ring.firstMaterial=flat(tint)
            let marker=SCNNode(geometry:ring);marker.position=vector(point);marker.eulerAngles.x = .pi/2;group.addChildNode(marker)
            let a=2*sqrt(track.majorVariance),b=2*sqrt(track.minorVariance),angle=track.ellipseAngle
            func ellipse(_ step:Int)->[Double] {
                let phase=Double(step)/32*2*Double.pi,x=a*cos(phase),y=b*sin(phase)
                return [point[0]+x*cos(angle)-y*sin(angle),point[1]+x*sin(angle)+y*cos(angle),0.035]
            }
            for step in 0..<32 where track.state != "coasting" || step%2==0 {
                let edge=line(ellipse(step),ellipse(step+1),color:tint.withAlphaComponent(0.55),radius:0.009)
                edge.name="track-uncertainty";group.addChildNode(edge)
            }
            if track.motionKnown && track.speed>0.03 {
                let end=[point[0]+track.velocity_xz[0]*0.5,point[1]-track.velocity_xz[1]*0.5,0.07]
                let arrow=line(point,end,color:tint,radius:0.016);arrow.name="track-velocity";group.addChildNode(arrow)
                let direction=atan2(end[1]-point[1],end[0]-point[0])
                for offset in [-0.5,0.5] {
                    group.addChildNode(line(end,[end[0]-0.12*cos(direction+offset),end[1]-0.12*sin(direction+offset),end[2]],color:tint,radius:0.014))
                }
            }
            group.addChildNode(label(track.shortID+(track.ambiguous ? " ?":""),position:[point[0],point[1],0.30],size:0.14))
            groups[.tracks]?.addChildNode(group)
        }
    }
    private func fusionOverlays(_ fusion:FusionState?) {
        guard let fusion else{return}
        if model?.layers.contains(.observations) == true {
            for observation in fusion.observations {
                let tint=observation.tint.withAlphaComponent(observation.active ? 0.95:0.35)
                let marker=SCNNode();marker.name="observation:"+observation.id;marker.position=vector(observation.position)
                let dot=sphere([0,0,0],radius:0.07,color:tint);marker.addChildNode(dot)
                let icon=SCNPlane(width:0.25,height:0.25)
                let iconMaterial=flat(tint)
                let symbol=NSImage(systemSymbolName:observation.symbol,accessibilityDescription:observation.label)?.withSymbolConfiguration(.init(paletteColors:[tint]))
                iconMaterial.diffuse.contents=symbol
                iconMaterial.emission.contents=symbol;iconMaterial.isDoubleSided=true
                iconMaterial.readsFromDepthBuffer=false;iconMaterial.writesToDepthBuffer=false
                icon.firstMaterial=iconMaterial
                let badge=SCNNode(geometry:icon);badge.position=SCNVector3(0,0,0.24)
                badge.constraints=[SCNBillboardConstraint()];badge.renderingOrder=105;marker.addChildNode(badge)
                groups[.observations]?.addChildNode(marker)
                if let target=observation.target,observation.active {
                    // Every ray is a reported observation, never a ground-truth beam.
                    // Camera targets are inferred sightlines and use a dashed style.
                    let segments=observation.kind == "camera" ? 18:10
                    for i in stride(from:0,to:segments,by:2) {
                        func along(_ step:Int)->[Double] { let t=Double(step)/Double(segments);return zip(observation.position,target).map { $0*(1-t)+$1*t } }
                        let ray=line(along(i),along(i+1),color:tint.withAlphaComponent(0.65),radius:0.012)
                        ray.name="observation-ray:"+observation.id;groups[.observations]?.addChildNode(ray)
                    }
                }
            }
        }
        if model?.layers.contains(.hazards) == true,fusion.localizedHazard,let position=fusion.position {
            let tint=NSColor(red:0.86,green:0.36,blue:0.12,alpha:1)
            let anchor=SCNNode();anchor.name="hazard-estimate";anchor.position=vector(position)
            anchor.addChildNode(sphere([0,0,0.12],radius:0.1,color:tint))
            let selected=model?.selectedOriginal != nil && model?.selectedOriginalEnvelopeSHA256==fusion.original_envelope_sha256
            let marker=SCNTorus(ringRadius:selected ? 0.28:0.22,pipeRadius:0.016);marker.firstMaterial=flat(tint)
            let ring=SCNNode(geometry:marker);ring.eulerAngles.x = .pi/2;ring.position.z=0.045;anchor.addChildNode(ring)
            if let uncertainty=fusion.uncertainty_m,uncertainty>0 {
                let halo=SCNTorus(ringRadius:CGFloat(uncertainty),pipeRadius:0.014);halo.firstMaterial=flat(tint.withAlphaComponent(0.65))
                let boundary=SCNNode(geometry:halo);boundary.name="reported-uncertainty";boundary.eulerAngles.x = .pi/2
                boundary.position.z=0.035;anchor.addChildNode(boundary)
                let area=SCNCylinder(radius:CGFloat(uncertainty),height:0.008);area.radialSegmentCount=64
                let fill=flat(tint.withAlphaComponent(0.08));fill.writesToDepthBuffer=false;area.firstMaterial=fill
                let disc=SCNNode(geometry:area);disc.eulerAngles.x = .pi/2;disc.position.z=0.025;anchor.addChildNode(disc)
            }
            anchor.addChildNode(label("Hazard estimate",position:[0,0,0.65],size:0.17))
            groups[.hazards]?.addChildNode(anchor)
        }
    }
    private func label(_ text:String,position:[Double],size:CGFloat)->SCNNode {
        let geom=SCNText(string:text,extrusionDepth:0);geom.font=NSFont.systemFont(ofSize:1,weight:.medium);geom.flatness=0.15
        let textMaterial=flat(NSColor(red:0.16,green:0.25,blue:0.26,alpha:1))
        textMaterial.readsFromDepthBuffer=false;textMaterial.writesToDepthBuffer=false;geom.firstMaterial=textMaterial
        let textNode=SCNNode(geometry:geom);textNode.castsShadow=false
        let (min,max)=textNode.boundingBox
        textNode.pivot=SCNMatrix4MakeTranslation((min.x+max.x)/2,(min.y+max.y)/2,0)
        textNode.renderingOrder=101
        let plate=SCNPlane(width:CGFloat(max.x-min.x)+1.5,height:CGFloat(max.y-min.y)+0.9)
        plate.cornerRadius=0.45
        let background=flat(NSColor(red:0.98,green:0.98,blue:0.96,alpha:0.94))
        background.readsFromDepthBuffer=false;background.writesToDepthBuffer=false;plate.firstMaterial=background
        let plateNode=SCNNode(geometry:plate);plateNode.position.z = -0.01;plateNode.renderingOrder=100;plateNode.castsShadow=false
        let node=SCNNode();node.addChildNode(plateNode);node.addChildNode(textNode)
        node.scale=SCNVector3(Float(size),Float(size),Float(size));node.position=vector(position)
        node.constraints=[SCNBillboardConstraint()]
        return node
    }
    func preset(_ preset:CameraPreset,robot:Robot?) {
        view.allowsCameraControl = preset != .robot
        camera.camera?.usesOrthographicProjection = preset != .robot
        camera.camera?.orthographicScale = preset == .top ? 11.8 : 11.6
        let target=SCNVector3(0,0,0)
        if model?.trackingOnly == true || model?.snapshot?.fusion_selection != nil {
            camera.camera?.orthographicScale = preset == .top ? 4.7:4.2
            camera.position=preset == .top ? SCNVector3(0,12,0.01):SCNVector3(5.2,5.0,7.0)
            camera.look(at:SCNVector3(0,0.7,0));view.pointOfView=camera;return
        }
        switch preset {
        case .perspective: camera.position=SCNVector3(14,19,19)
        case .top: camera.position=SCNVector3(0,25,0.01)
        case .robot:
            if let p=robot?.position { followTarget=SIMD3(Float(p[0]),0.3,-Float(p[1])) }
            applyFollowPose();return
        }
        camera.look(at:target)
        view.pointOfView=camera
        view.defaultCameraController.target=target
    }
    private func moveFollowTarget(_ robot:Robot?) {
        guard let p=robot?.position else{return}
        let next=SIMD3<Float>(Float(p[0]),0.3,-Float(p[1]))
        camera.simdPosition += next-followTarget
        followTarget=next
        // Only translate on a new robot position. User yaw and distance never reset.
        camera.look(at:SCNVector3(followTarget),up:SCNVector3(0,1,0),localFront:SCNVector3(0,0,-1))
        view.defaultCameraController.target=SCNVector3(followTarget)
    }
    func followInput(horizontal:Double,zoom:Double) {
        guard following,horizontal.isFinite,zoom.isFinite else{return}
        followYaw += min(500,max(-500,horizontal))*0.008
        followYaw.formTruncatingRemainder(dividingBy:2 * .pi)
        followDistance=min(22,max(2.5,followDistance * exp(-min(500,max(-500,zoom))*0.02)))
        applyFollowPose()
    }
    private func applyFollowPose() {
        let horizontal=followDistance*cos(followElevation)
        camera.simdPosition=followTarget+SIMD3(Float(sin(followYaw)*horizontal),Float(followDistance*sin(followElevation)),Float(cos(followYaw)*horizontal))
        camera.look(at:SCNVector3(followTarget),up:SCNVector3(0,1,0),localFront:SCNVector3(0,0,-1))
        view.pointOfView=camera;view.defaultCameraController.target=SCNVector3(followTarget)
    }
    func cameraDiagnostics()->CameraDiagnostic {
        let p=camera.simdPosition
        return CameraDiagnostic(mode:model?.camera.rawValue ?? "Overview",yaw:followYaw,distance:followDistance,elevation:followElevation,
            position:[Double(p.x),Double(p.y),Double(p.z)],target:[Double(followTarget.x),Double(followTarget.y),Double(followTarget.z)])
    }
    @objc private func clicked(_ recognizer:NSClickGestureRecognizer) {
        let hit=view.hitTest(recognizer.location(in:view),options:[:])
        for result in hit {
            if select(node:result.node) { return }
        }
    }
    // Used by actual SceneKit hits and headless checks. Selection never changes
    // the camera preset, target, yaw or zoom.
    @discardableResult func select(node:SCNNode) -> Bool {
        var current:SCNNode?=node
        while let n=current {
            if let name=n.name {
                if name.hasPrefix("track:") { return model?.selectTrack(String(name.dropFirst(6))) ?? false }
                if name=="hazard-estimate" { return model?.selectHazard() ?? false }
                if name.hasPrefix("observation:") { return model?.selectHazard(observationID:String(name.dropFirst(12))) ?? false }
                if name.hasPrefix("observation-ray:") { return model?.selectHazard(observationID:String(name.dropFirst(16))) ?? false }
                if name.hasPrefix("robot:") { model?.selectedRobot=String(name.dropFirst(6));return true }
                if name.hasPrefix("edge:") { model?.command("toggle_edge",target:String(name.dropFirst(5)));return true }
            }
            current=n.parent
        }
        return false
    }
    func diagnostics()->[LayerDiagnostic] {
        DemoLayer.allCases.map { layer in
            let node = layer == .building ? building : (groups[layer] ?? SCNNode())
            return LayerDiagnostic(name:layer.rawValue,enabled:model?.layers.contains(layer) == true,
                                   hidden:node.isHidden,child_nodes:node.childNodes.count)
        }
    }
}

struct SceneView: NSViewRepresentable {
    @ObservedObject var model:DemoModel
    func makeNSView(context:Context)->SCNView { let renderer=SceneRenderer();model.renderer=renderer;renderer.update(model);return renderer.view }
    func updateNSView(_ view:SCNView,context:Context) { model.renderer?.update(model) }
}
