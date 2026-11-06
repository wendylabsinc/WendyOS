import SwiftUI
import RealityKit
import UIKit

// Warm accents identify actions; cyan is reserved for measured spatial state.
enum SpatialStyle {
    static let amber = Color(red: 1, green: 0.66, blue: 0.34)
    static let cyan = Color(red: 0.38, green: 0.87, blue: 0.92)
    static let muted = Color.white.opacity(0.065)
}

struct SpatialCard<Content: View>: View {
    var accent: Color = .white
    @ViewBuilder let content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 18) { content }
            .padding(24)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(accent.opacity(0.035), in: RoundedRectangle(cornerRadius: 28))
            .overlay(RoundedRectangle(cornerRadius: 28).strokeBorder(.white.opacity(0.09), lineWidth: 1))
    }
}
struct SpatialEyebrow: View {
    let title: String
    var body: some View { Text(title.uppercased()).font(.system(size: 11, weight: .semibold, design: .rounded)).tracking(2.5).foregroundStyle(.secondary) }
}
struct SpatialStatus: View {
    let title: String
    var color: Color = .secondary
    var body: some View {
        HStack(spacing: 7) {
            Circle().fill(color).frame(width: 6, height: 6)
            Text(title).font(.system(size: 12, weight: .medium))
        }.padding(.horizontal, 12).padding(.vertical, 8)
            .background(color.opacity(0.09), in: Capsule())
            .accessibilityElement(children: .combine)
    }
}
struct SpatialSectionTitle: View {
    let title: String
    let subtitle: String
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.system(size: 28, weight: .semibold, design: .rounded))
            Text(subtitle).font(.subheadline).foregroundStyle(.secondary)
        }
    }
}
struct SpatialActionTile: View {
    let title: String
    let detail: String
    let symbol: String
    var color: Color = SpatialStyle.amber
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            HStack(spacing: 16) {
                Image(systemName: symbol).font(.system(size: 23, weight: .light))
                    .foregroundStyle(color).frame(width: 48, height: 48)
                    .background(color.opacity(0.12), in: RoundedRectangle(cornerRadius: 16))
                VStack(alignment: .leading, spacing: 4) {
                    Text(title).font(.headline)
                    Text(detail).font(.caption).foregroundStyle(.secondary)
                }
                Spacer(minLength: 0)
                Image(systemName: "arrow.up.right").font(.caption).foregroundStyle(.tertiary)
            }.padding(16).frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(RoundedRectangle(cornerRadius: 22))
        }.buttonStyle(.plain).hoverEffect(.highlight)
            .background(SpatialStyle.muted, in: RoundedRectangle(cornerRadius: 22))
    }
}

@MainActor enum SpatialAssets {
    private(set) static var robot: Entity?
    static func load() async {
        guard robot == nil, let url = Bundle.main.url(forResource: "Go2", withExtension: "usdz", subdirectory: "Spatial") else { return }
        robot = try? await Entity(contentsOf: url)
    }
}

struct SpatialRobotPreview: View {
    @State private var loaded = false
    @State private var root = Entity()
    @State private var world = Entity()
    @State private var aperture = ModelEntity()
    var body: some View {
        GeometryReader3D { geometry in
        RealityView { content in
            // A portal keeps stereo depth behind the window and clips the
            // miniature to its inset, including when viewed from the side.
            world.components.set(WorldComponent())
            world.addChild(root)
            let light = DirectionalLight()
            light.light.intensity = 1800
            light.orientation = simd_quatf(angle: -.pi / 4, axis: [1, 0, 0])
            world.addChild(light)
            let backdrop = ModelEntity(mesh: .generatePlane(width: 4, height: 4),
                materials: [UnlitMaterial(color: UIColor(red: 0.16, green: 0.15, blue: 0.14, alpha: 1))])
            backdrop.position.z = -0.6
            world.addChild(backdrop)
            aperture.components.set(PortalComponent(target: world,
                clippingMode: .plane(.positiveZ), crossingMode: .disabled))
            content.add(world)
            content.add(aperture)
        } update: { content in
            root.children.removeAll()
            let robot = SpatialDrawing.robot(at: .zero, yaw: -.pi / 5)
            root.addChild(robot)
            root.scale = .one
            root.position = .zero
            // The platform shows a scale model, never fabricated live positions.
            let platform = ModelEntity(mesh: .generateCylinder(height: 0.008, radius: 0.39),
                materials: [SimpleMaterial(color: UIColor.white.withAlphaComponent(0.08), isMetallic: true)])
            platform.position.y = -0.01; root.addChild(platform)
            for i in 0..<48 {
                let a = Float(i) / 48 * .pi * 2
                let b = a + .pi / 32
                let start = SIMD3<Float>(cos(a) * 0.41, 0, sin(a) * 0.41)
                let end = SIMD3<Float>(cos(b) * 0.41, 0, sin(b) * 0.41)
                root.addChild(SpatialDrawing.line(start, end, color: UIColor(red: 1, green: 0.66, blue: 0.34, alpha: 0.55), width: 0.003))
            }
            let available = content.convert(geometry.frame(in: .global), from: .global, to: content)
            let bounds = root.visualBounds(relativeTo: root)
            let fit = min(available.extents.x / max(bounds.extents.x, 0.01),
                          available.extents.y / max(bounds.extents.y, 0.01)) * 0.76
            root.scale = SIMD3(repeating: fit)
            root.position = -bounds.center * fit
            // The nearest point, not just the pivot, is 3 cm behind the pane.
            root.position.z = -bounds.max.z * fit - 0.03
            aperture.position = available.center
            world.position = available.center
            aperture.model = ModelComponent(
                mesh: .generatePlane(width: available.extents.x, height: available.extents.y,
                                     cornerRadius: min(available.extents.x, available.extents.y) * 0.09),
                materials: [PortalMaterial()])
            _ = loaded
        }
        }
        // Reserve no forward volume. The model's depth comes from the portal.
        .frame(depth: 0)
        .task { await SpatialAssets.load(); loaded = true }
        .overlay {
            RoundedRectangle(cornerRadius: 22)
                .strokeBorder(.white.opacity(0.12), lineWidth: 1)
                .allowsHitTesting(false)
        }
        // Keep the composited portal and its rim clear of the native glass
        // and card background. Coplanar surfaces flicker as the wearer moves.
        // This shifts only the aperture by six points; Go2 remains recessed
        // at least 3 cm inside it, behind the main window plane.
        .offset(z: 6)
        .accessibilityLabel("Recessed three-dimensional scale model of a Unitree Go2")
    }
}
