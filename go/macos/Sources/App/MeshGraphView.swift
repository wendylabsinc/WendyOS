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
}
struct DeviceMeshTopology {
    let available, runtime, truncated: Bool
    let captured: Date?
    let error, principal, address: String
    let links: [DeviceMeshPeer]
    let routes: [DeviceMeshRoute]
}

struct MeshGraphView: View {
    let topology: DeviceMeshTopology
    let deviceName: String
    @Environment(\.dismiss) private var dismiss
    @State private var selected: String?
    @State private var zoom: CGFloat = 1
    @GestureState private var magnification: CGFloat = 1
    @State private var showingDetails = false
    private var peers: [DeviceMeshPeer] {
        Dictionary(grouping: topology.links, by: \.principal).values.compactMap(\.first)
            .sorted { $0.principal < $1.principal }
    }
    private var diameter: CGFloat { max(340, CGFloat(peers.count) * 48) }
    private var canvasSize: CGFloat { diameter + 150 }
    private func point(_ index: Int) -> CGPoint {
        let angle = Double(index) / Double(max(1, peers.count)) * .pi * 2 - .pi / 2
        return CGPoint(x: canvasSize / 2 + cos(angle) * diameter / 2,
                       y: canvasSize / 2 + sin(angle) * diameter / 2)
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
                        let fit = min(geometry.size.width, geometry.size.height) / canvasSize
                        let scale = fit * min(8, max(0.5, zoom * magnification))
                        ScrollView([.horizontal, .vertical]) {
                            graph
                                .scaleEffect(scale, anchor: .topLeading)
                                .frame(width: canvasSize * scale, height: canvasSize * scale, alignment: .topLeading)
                                .frame(minWidth: geometry.size.width, minHeight: geometry.size.height)
                        }
                        .defaultScrollAnchor(.center)
                        .simultaneousGesture(MagnifyGesture()
                            .updating($magnification) { value, state, _ in state = value.magnification }
                            .onEnded { value in zoom = min(8, max(0.5, zoom * value.magnification)) })
                    }
                }
                VStack(alignment: .leading, spacing: 4) {
                    Text("Direct connections · \(deviceName)").fontWeight(.medium)
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
                    Button { dismiss() } label: { Image(systemName: "xmark") }.accessibilityLabel("Close mesh graph")
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
                        .toolbar { ToolbarItem(placement: .confirmationAction) { Button { showingDetails = false } label: { Image(systemName: "xmark") }.accessibilityLabel("Close details") } }
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
        ZStack(alignment: .topLeading) {
            Canvas { context, _ in
                for index in peers.indices {
                    var path = Path()
                    path.move(to: CGPoint(x: canvasSize / 2, y: canvasSize / 2))
                    path.addLine(to: point(index))
                    context.stroke(path, with: .color(.accentColor.opacity(0.45)), lineWidth: 2)
                }
            }.frame(width: canvasSize, height: canvasSize)
            node(deviceName, subtitle: "This device", symbol: "desktopcomputer", active: selected == nil) { selected = nil; showingDetails = true }
                .position(x: canvasSize / 2, y: canvasSize / 2)
            ForEach(Array(peers.enumerated()), id: \.element.principal) { index, peer in
                node(peer.title, subtitle: transports(peer.principal), symbol: "network", active: selected == peer.principal) { selected = peer.principal; showingDetails = true }
                    .position(point(index))
            }
            if peers.isEmpty {
                Text(topology.runtime ? "No direct peers in this snapshot" : "No runtime snapshot available").font(.callout).foregroundStyle(.secondary)
                    .position(x: canvasSize / 2, y: canvasSize / 2 + 90)
            }
        }.frame(width: canvasSize, height: canvasSize)
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
    private func transports(_ principal: String) -> String {
        Set(topology.links.filter { $0.principal == principal }.map { $0.transport.isEmpty ? "Unknown transport" : $0.transport }).sorted().joined(separator: ", ")
    }
    private var details: some View {
        List {
            if let selected {
                Section("Direct links") {
                    ForEach(topology.links.filter { $0.principal == selected }) { link in
                        VStack(alignment: .leading, spacing: 4) {
                            LabeledContent(link.transport.isEmpty ? "Unknown transport" : link.transport, value: "Cost \(link.cost)")
                            Text(link.address.isEmpty ? link.principal : link.address).font(.caption.monospaced()).textSelection(.enabled)
                        }
                    }
                }
            } else {
                Section(deviceName) {
                    Text(topology.address.isEmpty ? topology.principal : topology.address).font(.caption.monospaced()).textSelection(.enabled)
                    if topology.runtime { Text("\(peers.count) direct peers · \(topology.links.count) links") }
                    else { Text("Check Mesh status in Settings for the runtime error.").foregroundStyle(.secondary) }
                }
            }
            Section(selected == nil ? "Routes" : "Routes through this peer") {
                let routes = topology.routes.filter { selected == nil || $0.nextHop == selected }
                if routes.isEmpty { Text("No routes reported").foregroundStyle(.secondary) }
                ForEach(routes) { route in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(route.prefix).font(.callout.monospaced())
                        Text(route.unreachable ? "Unreachable" : route.local ? "Local route" : "Via \(route.nextHop) · Metric \(route.metric)")
                            .font(.caption).foregroundStyle(route.unreachable ? Color.orange : .secondary)
                        Text(route.authorized ? "Origin authorized at snapshot" : "Origin authorization unverified").font(.caption2).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .listStyle(.plain)
    }
}
