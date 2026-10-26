import SwiftUI

struct DeviceMeshPeer: Identifiable {
    let id: UInt64
    let principal, deviceID, address, transport, name: String
    let cost: UInt32
    var title: String { name.isEmpty ? (deviceID.isEmpty ? principal : deviceID) : name }
}
struct DeviceMeshRoute: Identifiable {
    let id: Int
    let prefix, origin, nextHop: String
    let metric: UInt32
    let local, unreachable, authorized: Bool
    var reachable: Bool { !local && !unreachable && metric < 65535 }
}
struct DeviceMeshAnnouncement {
    let principal, deviceID, name, prefix: String
}
struct DeviceMeshTopology {
    let available, runtime, truncated: Bool
    let captured: Date?
    let error, principal, address: String
    let links: [DeviceMeshPeer]
    let routes: [DeviceMeshRoute]
    let announcements: [DeviceMeshAnnouncement]
}

struct DeviceMeshGraphNode: Identifiable {
    let id, title, address: String
    let direct: Bool
    let nextHop: String?
    let subtitle: String
}

// A snapshot describes direct links and routes via a next hop. It cannot tell
// us which physical links exist beyond that hop, or how many hops remain.
struct DeviceMeshGraph {
    let nodes: [DeviceMeshGraphNode]
    var direct: [DeviceMeshGraphNode] { nodes.filter(\.direct) }
    var indirect: [DeviceMeshGraphNode] { nodes.filter { !$0.direct && $0.nextHop != nil } }
    var unrouted: [DeviceMeshGraphNode] { nodes.filter { !$0.direct && $0.nextHop == nil } }

    init(topology: DeviceMeshTopology) {
        let links = Dictionary(grouping: topology.links, by: \.principal)
        let announcements = Dictionary(grouping: topology.announcements, by: \.principal)
        let routes = Dictionary(grouping: topology.routes.filter { !$0.local }, by: \.origin)
        let principals = Set(links.keys).union(announcements.keys).union(routes.keys)
            .subtracting(["", topology.principal]).sorted()
        func title(_ principal: String) -> String {
            if let name = announcements[principal]?.first(where: { !$0.name.isEmpty })?.name { return name }
            if let link = links[principal]?.first { return link.title }
            if let id = announcements[principal]?.first(where: { !$0.deviceID.isEmpty })?.deviceID { return id }
            return principal.split(separator: "/").last.map(String.init) ?? principal
        }
        nodes = principals.map { principal in
            let directLinks = links[principal] ?? []
            let originRoutes = routes[principal] ?? []
            let route = originRoutes.filter { $0.reachable && links[$0.nextHop] != nil && $0.nextHop != principal }
                .sorted {
                    if $0.metric != $1.metric { return $0.metric < $1.metric }
                    if $0.nextHop != $1.nextHop { return $0.nextHop < $1.nextHop }
                    return $0.id < $1.id
                }.first
            let nextHop = directLinks.isEmpty ? route?.nextHop : nil
            let subtitle: String
            if !directLinks.isEmpty {
                subtitle = Set(directLinks.map { $0.transport.isEmpty ? "Unknown transport" : $0.transport }).sorted().joined(separator: ", ")
            } else if let nextHop {
                subtitle = "Via \(title(nextHop))"
            } else if !originRoutes.isEmpty && originRoutes.allSatisfy({ !$0.reachable }) {
                subtitle = "Unreachable"
            } else if !originRoutes.isEmpty {
                subtitle = "Next hop unavailable"
            } else {
                subtitle = "No route reported"
            }
            let address = directLinks.first(where: { !$0.address.isEmpty })?.address
                ?? announcements[principal]?.first(where: { !$0.prefix.isEmpty })?.prefix
                ?? originRoutes.first?.prefix ?? ""
            return DeviceMeshGraphNode(id: principal, title: title(principal), address: address,
                                       direct: !directLinks.isEmpty, nextHop: nextHop, subtitle: subtitle)
        }
    }
}

struct MeshGraphView: View {
    let topology: DeviceMeshTopology
    let deviceName: String
    private let mesh: DeviceMeshGraph
    @Environment(\.dismiss) private var dismiss
    @State private var selected: String?
    @State private var zoom: CGFloat = 1
    @GestureState private var magnification: CGFloat = 1
    @State private var showingDetails = false
    init(topology: DeviceMeshTopology, deviceName: String) {
        self.topology = topology
        self.deviceName = deviceName
        mesh = DeviceMeshGraph(topology: topology)
    }
    private var canvasSize: CGSize {
        let rows = mesh.direct.reduce(0) { count, peer in
            count + max(1, mesh.indirect.filter { $0.nextHop == peer.id }.count)
        }
        return CGSize(width: mesh.nodes.contains(where: { !$0.direct }) ? 800 : 510,
                      height: max(320, CGFloat(rows + mesh.unrouted.count) * 120 + 40))
    }
    private var positions: [String: CGPoint] {
        var result = [topology.principal: CGPoint(x: mesh.nodes.isEmpty ? canvasSize.width / 2 : 90, y: canvasSize.height / 2)]
        var row = 0
        for peer in mesh.direct {
            let children = mesh.indirect.filter { $0.nextHop == peer.id }
            let count = max(1, children.count)
            result[peer.id] = CGPoint(x: 400, y: 80 + CGFloat(row) * 120 + CGFloat(count - 1) * 60)
            for (index, child) in children.enumerated() {
                result[child.id] = CGPoint(x: 700, y: 80 + CGFloat(row + index) * 120)
            }
            row += count
        }
        for peer in mesh.unrouted {
            result[peer.id] = CGPoint(x: 700, y: 80 + CGFloat(row) * 120)
            row += 1
        }
        return result
    }
    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                if !topology.error.isEmpty {
                    ContentUnavailableView("Mesh graph unavailable", systemImage: "network.slash", description: Text(topology.error))
                } else if !topology.available {
                    ContentUnavailableView("Mesh graph unavailable", systemImage: "network.slash", description: Text("This device has not returned a topology snapshot."))
                } else {
                    GeometryReader { geometry in
                        let fit = min(geometry.size.width / canvasSize.width, geometry.size.height / canvasSize.height)
                        let scale = fit * min(8, max(0.5, zoom * magnification))
                        ScrollView([.horizontal, .vertical]) {
                            graph
                                .scaleEffect(scale, anchor: .topLeading)
                                .frame(width: canvasSize.width * scale, height: canvasSize.height * scale, alignment: .topLeading)
                                .frame(minWidth: geometry.size.width, minHeight: geometry.size.height)
                        }
                        .defaultScrollAnchor(.center)
                        .simultaneousGesture(MagnifyGesture()
                            .updating($magnification) { value, state, _ in state = value.magnification }
                            .onEnded { value in zoom = min(8, max(0.5, zoom * value.magnification)) })
                    }
                }
                VStack(alignment: .leading, spacing: 4) {
                    Text("\(mesh.direct.count) direct · \(mesh.indirect.count) indirect · \(deviceName)").fontWeight(.medium)
                    Text("Solid: direct link · Dashed: route via peer, possibly multiple hops")
                    if !mesh.unrouted.isEmpty { Text("Devices without a usable route: \(mesh.unrouted.count)") }
                    Text("Pinch to zoom · Tap a device for details")
                    if let captured = topology.captured { Text("Snapshot \(captured.formatted(date: .omitted, time: .standard))") }
                    if topology.truncated { Text("The device returned a partial topology.").foregroundStyle(.orange) }
                }.font(.caption).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading).padding()
            }
            .navigationTitle("Mesh graph")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button { dismiss() } label: { Image(systemName: "xmark") }.accessibilityLabel("Close mesh graph").keyboardShortcut(.cancelAction)
                }
                ToolbarItem(placement: .primaryAction) {
                    Button { zoom = 1 } label: { Image(systemName: "arrow.up.left.and.arrow.down.right") }.accessibilityLabel("Fit graph")
                }
            }
            .sheet(isPresented: $showingDetails) {
                NavigationStack {
                    details
                        .navigationTitle("Connection details")
                        #if os(iOS)
                        .navigationBarTitleDisplayMode(.inline)
                        #endif
                        .toolbar { ToolbarItem(placement: .confirmationAction) { Button { showingDetails = false } label: { Image(systemName: "xmark") }.accessibilityLabel("Close details").keyboardShortcut(.cancelAction) } }
                }.desktopFrame(width: 520, height: 500)
                #if os(iOS)
                .presentationDetents([.medium, .large])
                .presentationBackground(Color(uiColor: .systemGroupedBackground))
                #endif
            }
        }
        .desktopFrame(width: 820, height: 720)
    }
    private var graph: some View {
        let points = positions
        return ZStack(alignment: .topLeading) {
            Canvas { context, _ in
                for peer in mesh.nodes {
                    guard let source = peer.direct ? topology.principal : peer.nextHop,
                          let start = points[source], let end = points[peer.id] else { continue }
                    var path = Path()
                    path.move(to: start)
                    path.addLine(to: end)
                    context.stroke(path, with: .color(.accentColor.opacity(0.45)),
                                   style: StrokeStyle(lineWidth: 2, dash: peer.direct ? [] : [6, 5]))
                }
            }.frame(width: canvasSize.width, height: canvasSize.height)
            node(deviceName, subtitle: "This device", symbol: "desktopcomputer", active: selected == nil) { selected = nil; showingDetails = true }
                .position(points[topology.principal] ?? .zero)
            ForEach(mesh.nodes) { peer in
                node(peer.title, subtitle: peer.subtitle, symbol: "network", active: selected == peer.id) { selected = peer.id; showingDetails = true }
                    .position(points[peer.id] ?? .zero)
            }
            if mesh.nodes.isEmpty {
                Text(topology.runtime ? "No peers in this snapshot" : "No runtime snapshot available").font(.callout).foregroundStyle(.secondary)
                    .position(x: canvasSize.width / 2, y: canvasSize.height / 2 + 90)
            }
        }.frame(width: canvasSize.width, height: canvasSize.height)
    }
    private func node(_ title: String, subtitle: String, symbol: String, active: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            VStack(spacing: 5) {
                Image(systemName: symbol).font(.title2).foregroundStyle(active ? Color.accentColor : .secondary)
                Text(title).font(.caption.weight(.semibold)).lineLimit(2).truncationMode(.middle)
                Text(subtitle).font(.caption2).foregroundStyle(.secondary).lineLimit(2)
            }.padding(10).frame(width: 135)
                .background(.background, in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).stroke(active ? Color.accentColor : Color.secondary.opacity(0.3), lineWidth: active ? 2 : 1))
        }.buttonStyle(.plain).accessibilityLabel("\(title), \(subtitle)")
    }
    private var details: some View {
        List {
            if let selected {
                if let peer = mesh.nodes.first(where: { $0.id == selected }) {
                    Section(peer.title) {
                        Text(peer.subtitle)
                        Text(peer.address.isEmpty ? peer.id : peer.address).font(.caption.monospaced()).textSelection(.enabled)
                    }
                }
                if topology.links.contains(where: { $0.principal == selected }) {
                    Section("Direct links") {
                        ForEach(topology.links.filter { $0.principal == selected }) { link in
                            VStack(alignment: .leading, spacing: 4) {
                                LabeledContent(link.transport.isEmpty ? "Unknown transport" : link.transport, value: "Cost \(link.cost)")
                                Text(link.address.isEmpty ? link.principal : link.address).font(.caption.monospaced()).textSelection(.enabled)
                            }
                        }
                    }
                }
            } else {
                Section(deviceName) {
                    Text(topology.address.isEmpty ? topology.principal : topology.address).font(.caption.monospaced()).textSelection(.enabled)
                    if topology.runtime { Text("\(mesh.direct.count) direct peers · \(mesh.indirect.count) indirect peers · \(topology.links.count) links") }
                    else { Text("Check Mesh status in Settings for the runtime error.").foregroundStyle(.secondary) }
                }
            }
            Section(selected == nil ? "Routes" : "Routes to or through this peer") {
                let routes = topology.routes.filter { selected == nil || $0.nextHop == selected || $0.origin == selected }
                if routes.isEmpty { Text("No routes reported").foregroundStyle(.secondary) }
                ForEach(routes) { route in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(route.prefix).font(.callout.monospaced())
                        Text(route.unreachable || route.metric >= 65535 ? "Unreachable" : route.local ? "Local route" : "Via \(route.nextHop) · Metric \(route.metric)")
                            .font(.caption).foregroundStyle(route.unreachable || route.metric >= 65535 ? Color.orange : .secondary)
                        Text(route.authorized ? "Origin authorized at snapshot" : "Origin authorization unverified").font(.caption2).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .listStyle(.plain)
    }
}
