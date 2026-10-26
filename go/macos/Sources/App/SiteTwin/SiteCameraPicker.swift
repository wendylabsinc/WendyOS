import SwiftUI

struct SiteCameraLabel {
    let title: String
    let detail: String

    init(name: String, sourceID: String) {
        if name.hasPrefix("ROS 2 ") {
            let parts = String(name.dropFirst(6)).components(separatedBy: " (")
            title = parts[0]
            let scope = parts.count > 1 ? String(parts[1].dropLast()) : ""
            detail = (["ROS 2", scope.replacingOccurrences(of: "host:lo", with: "Local host")
                .replacingOccurrences(of: "host:eth0", with: "Ethernet")].filter { !$0.isEmpty }).joined(separator: " · ")
        } else {
            title = name.hasSuffix(":camera") ? String(name.dropLast(7)) : name
            detail = sourceID.hasPrefix("sensorlink:") ? "SensorLink camera" : "Camera"
        }
    }
}

/// Preview a source before adding it. The workspace locates it automatically
/// without requiring a manually placed marker.
struct SiteCameraPicker<Preview: View>: View {
    let feeds: [SiteFeed]
    let preview: (String) -> Preview
    let choose: (SiteFeed) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var query = ""
    @State private var selectedID: String?

    private var selected: SiteFeed? { feeds.first { $0.id == selectedID } }
    private var filtered: [SiteFeed] {
        feeds.filter { query.isEmpty || $0.name.localizedCaseInsensitiveContains(query) }
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                TextField("Search cameras", text: $query)
                    .textFieldStyle(.roundedBorder).padding()
                ScrollView {
                    LazyVStack(spacing: 8) {
                        ForEach(filtered) { feed in
                            let label = SiteCameraLabel(name: feed.name, sourceID: feed.id)
                            Button { selectedID = feed.id } label: {
                                HStack(spacing: 12) {
                                    Image(systemName: "video.fill").font(.title3)
                                        .frame(width: 40, height: 40)
                                        .background(.quaternary, in: RoundedRectangle(cornerRadius: 10))
                                    VStack(alignment: .leading, spacing: 4) {
                                        Text(label.title).font(.headline).lineLimit(2)
                                        Text(label.detail).font(.caption).foregroundStyle(.secondary)
                                    }.frame(maxWidth: .infinity, alignment: .leading)
                                    Image(systemName: selectedID == feed.id ? "checkmark.circle.fill" : "circle")
                                        .foregroundStyle(selectedID == feed.id ? Color.accentColor : Color.secondary)
                                }.padding(12).contentShape(Rectangle())
                                    .background(selectedID == feed.id ? Color.accentColor.opacity(0.12) : Color.secondary.opacity(0.06),
                                                in: RoundedRectangle(cornerRadius: 12))
                            }.buttonStyle(.plain)
                        }
                        if filtered.isEmpty {
                            ContentUnavailableView("No matching cameras", systemImage: "video.slash")
                        }
                    }.padding(.horizontal).padding(.bottom)
                }
                Divider()
                if let selected {
                    preview(selected.id).id(selected.id)
                        .frame(height: 190).clipShape(RoundedRectangle(cornerRadius: 12)).padding()
                } else {
                    Label("Select a camera to preview its view.", systemImage: "video")
                        .foregroundStyle(.secondary).padding(24)
                }
                HStack {
                    Text("We’ll locate this camera using the room scan and its live view.")
                        .font(.caption).foregroundStyle(.secondary)
                    Spacer()
                    Button("Add Camera") {
                        if let selected { choose(selected); dismiss() }
                    }.buttonStyle(.borderedProminent).disabled(selected == nil)
                }.padding()
            }
            .navigationTitle("Add Camera")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(width: 560, height: 640)
        #endif
    }
}
