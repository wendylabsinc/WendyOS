import SwiftUI

struct LocalPlan: View {
    let observations: [Observation]
    @Binding var selected: String?
    let now: Date
    let offline: Bool

    var body: some View {
        GeometryReader { reader in
            let plan = PlanProjection(observations: observations, size: reader.size)
            ZStack(alignment: .topLeading) {
                Color(red: 0.025, green: 0.045, blue: 0.060)
                Canvas { context, size in
                    for index in 0...8 {
                        let x = 38 + (size.width - 76) * CGFloat(index) / 8
                        let z = 32 + (size.height - 64) * CGFloat(index) / 8
                        var vertical = Path(); vertical.move(to: CGPoint(x: x, y: 0)); vertical.addLine(to: CGPoint(x: x, y: size.height))
                        var horizontal = Path(); horizontal.move(to: CGPoint(x: 0, y: z)); horizontal.addLine(to: CGPoint(x: size.width, y: z))
                        context.stroke(vertical, with: .color(.white.opacity(0.055)), lineWidth: 1)
                        context.stroke(horizontal, with: .color(.white.opacity(0.055)), lineWidth: 1)
                    }
                    let origin = plan.point([0, 0])
                    if origin.x >= 0 && origin.x <= size.width && origin.y >= 0 && origin.y <= size.height {
                        var axes = Path()
                        axes.move(to: CGPoint(x: 0, y: origin.y)); axes.addLine(to: CGPoint(x: size.width, y: origin.y))
                        axes.move(to: CGPoint(x: origin.x, y: 0)); axes.addLine(to: CGPoint(x: origin.x, y: size.height))
                        context.stroke(axes, with: .color(.white.opacity(0.16)), style: StrokeStyle(lineWidth: 1, dash: [4, 5]))
                    }
                }
                ForEach(clusters) { cluster in
                    let point = plan.point(cluster.position)
                    Group {
                        if cluster.members.count == 1, let observation = cluster.members.first {
                            Button { selected = observation.id } label: { marker(cluster) }
                                .accessibilityLabel("\(observation.name), \(observation.kind), \(observation.freshness(at: now, offline: offline))")
                        } else {
                            Menu {
                                ForEach(cluster.members) { observation in
                                    Button { selected = observation.id } label: { Label(observation.name + " · " + observation.kind, systemImage: WorldStyle.icon(observation.kind)) }
                                }
                            } label: { marker(cluster) }
                                .accessibilityLabel("\(cluster.members.count) sources at the same local plan position. Choose a source.")
                        }
                    }.buttonStyle(.plain).position(x: point.x, y: point.y + 11)
                }
                HStack {
                    Text(String(format: "X %.1f…%.1f m · Z %.1f…%.1f m", plan.minX, plan.maxX, plan.minZ, plan.maxZ))
                    Spacer()
                    Text("+X →  +Z ↓")
                }.font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary).padding(12)
                if observations.isEmpty {
                    VStack(spacing: 8) {
                        Image(systemName: "square.3.layers.3d.slash").font(.title2)
                        Text("No visible observations in this frame").font(.callout)
                    }.foregroundStyle(.secondary).frame(maxWidth: .infinity, maxHeight: .infinity)
                }
                if offline {
                    Text("SAVED SNAPSHOT · NOT LIVE").font(.system(size: 10, weight: .bold, design: .monospaced)).tracking(0.8)
                        .foregroundStyle(.orange).padding(10).background(WorldStyle.background.opacity(0.8), in: Capsule())
                        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .bottomLeading).padding(12)
                }
            }
        }
    }

    private var clusters: [PlanCluster] {
        var groups: [String: [Observation]] = [:]
        for observation in observations {
            guard let position = observation.planPositionXZ else { continue }
            // The plan projects X/Z. Group only exact projected coordinates;
            // the marker remains at that reported location, never nudged.
            let key = tupleID([String(position[0]), String(position[1])])
            groups[key, default: []].append(observation)
        }
        return groups.compactMap { key, members in
            guard let position = members.first?.planPositionXZ else { return nil }
            return PlanCluster(id: key, members: members.sorted { $0.id < $1.id }, position: position)
        }.sorted { $0.id < $1.id }
    }

    private func marker(_ cluster: PlanCluster) -> some View {
        let observation = cluster.members.first { $0.id == selected } ?? cluster.members[0]
        let selectedCluster = cluster.members.contains { $0.id == selected }
        let color = WorldStyle.color(observation.kind)
        return VStack(spacing: 5) {
            Image(systemName: cluster.members.count > 1 ? "square.stack.3d.up.fill" : WorldStyle.icon(observation.kind))
                .font(.system(size: 15, weight: .semibold)).foregroundStyle(color).frame(width: 36, height: 36)
                .background(color.opacity(0.18), in: Circle())
                .overlay(Circle().stroke(color.opacity(selectedCluster ? 1 : 0.35), lineWidth: selectedCluster ? 2 : 1))
                .overlay(alignment: .topTrailing) {
                    if cluster.members.count > 1 { Text("\(cluster.members.count)").font(.system(size: 9, weight: .bold)).foregroundStyle(.black).frame(width: 15, height: 15).background(WorldStyle.accent, in: Circle()).offset(x: 3, y: -3) }
                }
            Text(cluster.members.count > 1 ? "\(cluster.members.count) sources ▾" : observation.name)
                .font(.system(size: 10, weight: .medium, design: .monospaced)).lineLimit(1).frame(maxWidth: 130)
                .padding(.horizontal, 6).padding(.vertical, 3).background(WorldStyle.background.opacity(0.9), in: Capsule())
        }.opacity(observation.freshness(at: now, offline: offline) == "Fresh" ? 1 : 0.72)
    }
}

private struct PlanCluster: Identifiable {
    let id: String
    let members: [Observation]
    let position: [Double]
}

private struct PlanProjection {
    let minX: Double, maxX: Double, minZ: Double, maxZ: Double
    let size: CGSize
    let scale: Double
    init(observations: [Observation], size: CGSize) {
        let positions = observations.compactMap(\.planPositionXZ)
        let xs = positions.map { $0[0] }, zs = positions.map { $0[1] }
        let centerX = ((xs.min() ?? -2) + (xs.max() ?? 2)) / 2
        let centerZ = ((zs.min() ?? -2) + (zs.max() ?? 2)) / 2
        let spanX = max(6, (xs.max() ?? 2) - (xs.min() ?? -2) + 2)
        let spanZ = max(4, (zs.max() ?? 2) - (zs.min() ?? -2) + 2)
        self.size = size
        scale = min(max(1, size.width - 100) / spanX, max(1, size.height - 100) / spanZ)
        let extentX = max(1, size.width - 76) / scale
        let extentZ = max(1, size.height - 64) / scale
        minX = centerX - extentX / 2; maxX = centerX + extentX / 2
        minZ = centerZ - extentZ / 2; maxZ = centerZ + extentZ / 2
    }
    func point(_ position: [Double]) -> CGPoint {
        CGPoint(x: 38 + (position[0] - minX) * scale, y: 32 + (position[1] - minZ) * scale)
    }
}
