import SwiftUI
import CryptoKit

// Namespace resource kinds so future Robot/Site widgets can share the same layout.
enum HomeWidgetKind: String, Codable, CaseIterable, Identifiable {
    case favorites = "devices.favorites", camera = "devices.camera", campaign = "devices.campaign", gallery = "data.gallery", portal = "devices.portal", attention = "home.attention", activity = "home.activity", actions = "home.actions"
    var id: String { rawValue }
    var title: String { switch self {
    case .favorites: "Favorite devices"; case .camera: "Camera feed"; case .campaign: "Campaign recording"
    case .gallery: "Data gallery"; case .portal: "App web portal"
    case .attention: "Needs you"; case .activity: "Happening now"; case .actions: "Quick actions"
    } }
    var symbol: String { switch self {
    case .favorites: "star"; case .camera: "camera"; case .campaign: "record.circle"
    case .gallery: "square.grid.2x2"; case .portal: "globe"
    case .attention: "exclamationmark.triangle"; case .activity: "waveform.path"; case .actions: "bolt"
    } }
    var builtIn: Bool { [.attention, .activity, .actions].contains(self) }
    var sizes: [HomeWidgetSize] { self == .activity || self == .actions ? [.small, .medium] : HomeWidgetSize.allCases }
    static var headerColor: Color {
        Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .lightGray : UIColor(red: 91/255, green: 90/255, blue: 86/255, alpha: 1) })
    }
    var color: Color {
        switch self {
        case .favorites, .portal: Color(red: 42/255, green: 112/255, blue: 80/255)
        case .camera: Color(red: 48/255, green: 176/255, blue: 199/255)
        case .campaign: Color(red: 1, green: 59/255, blue: 48/255)
        case .gallery: Self.headerColor
        case .attention: Color(red: 1, green: 149/255, blue: 0)
        case .activity: Color(red: 88/255, green: 86/255, blue: 214/255)
        case .actions: RecordingAppearance.ink
        }
    }
    var summary: String { switch self {
    case .favorites: "Devices you open most"
    case .camera: "Watch a camera from Home"
    case .campaign: "Record a campaign clip"
    case .gallery: "Latest recordings from one or all devices"
    case .portal: "Open an app’s web page"
    case .attention: "Device issues that need attention"
    case .activity: "Recent activity and current connections"
    case .actions: "Shortcuts to setup, devices, activity and data"
    } }
}
enum HomeWidgetSize: String, Codable, CaseIterable { case small = "Small", medium = "Medium", large = "Large"
    var columns: Int { self == .small ? 2 : 4 }
    var rows: Int { self == .large ? 4 : 2 }
    var short: String { String(rawValue.prefix(1)) }
    static var compact: Self { .small }
    static var expanded: Self { .medium }
    init(from decoder: Decoder) throws {
        let value = try decoder.singleValueContainer().decode(String.self)
        switch value {
        case "Compact", "Small", "S": self = .small
        case "Expanded", "Medium", "M": self = .medium
        case "Large", "L": self = .large
        default: throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "Unknown widget size"))
        }
    }
 }
struct HomeWidgetDevice: Codable, Equatable, Identifiable {
    let id: String
    let title: String
    init(_ device: Device) { id = recordingDeviceID(device); title = device.title }
}
struct HomeWidget: Codable, Identifiable, Equatable {
    var id = UUID()
    var kind: HomeWidgetKind
    var size = HomeWidgetSize.expanded
    var title = ""
    var devices: [HomeWidgetDevice] = []
    // Camera stable identity, campaign name, or app identity. Never a numeric camera node.
    var resource = ""
    var resourceTitle = ""
    var dataKind: String?
    var label = ""
    var displayTitle: String {
        if !title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return title }
        switch kind {
        case .camera:
            let name = resourceTitle.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !name.isEmpty else { return kind.title }
            return name.lowercased().hasSuffix("camera") ? name : name + " camera"
        case .campaign: return resourceTitle.isEmpty ? kind.title : "Record · " + resourceTitle
        case .gallery: return devices.first.map { "Data · " + $0.title } ?? "Data gallery"
        case .portal: return resourceTitle.isEmpty ? kind.title : resourceTitle
        default: return kind.title
        }
    }
    var valid: Bool {
        if kind.builtIn { return kind.sizes.contains(size) }
        if kind == .gallery && devices.isEmpty { return true }
        guard !devices.isEmpty, devices.count <= 6, Set(devices.map(\.id)).count == devices.count else { return false }
        if kind != .favorites && devices.count != 1 { return false }
        return kind == .favorites || kind == .gallery || !resource.isEmpty
    }
}
@MainActor final class HomeWidgetStore: ObservableObject {
    @Published private(set) var items: [HomeWidget] = []
    @Published private(set) var error: String?
    private let defaults: UserDefaults
    private var key: String?
    init(defaults: UserDefaults = .standard) { self.defaults = defaults }
    static func storageKey(account: String, organization: String) -> String {
        let data = try! JSONEncoder().encode([account, organization])
        return "wendy.home.widgets.v1." + SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
    func load(account: String, organization: String) {
        let next = Self.storageKey(account: account, organization: organization)
        guard key != next else { return }
        key = next; items = []; error = nil
        guard let data = defaults.data(forKey: next) else { return }
        do {
            items = try JSONDecoder().decode([HomeWidget].self, from: data)
            // Old Home sections become grid items once when an existing layout migrates.
            let old = String(data: data, encoding: .utf8) ?? ""
            if old.contains("\"Compact\"") || old.contains("\"Expanded\"") {
                let hidden = Set((defaults.string(forKey: "wendy.home.hidden") ?? "").split(separator: ",").map(String.init))
                let order = (defaults.string(forKey: "wendy.home.sections") ?? "Needs you,Happening now,Quick actions").split(separator: ",").map(String.init)
                for title in order where !hidden.contains(title) {
                    if let kind = HomeWidgetKind.allCases.first(where: { $0.builtIn && $0.title == title }), !items.contains(where: { $0.kind == kind }) {
                        items.append(HomeWidget(kind: kind))
                    }
                }
                persist()
            }
        }
        catch { self.error = "The saved Home layout could not be read. Your saved layout has not been replaced." }
    }
    func save(_ widget: HomeWidget) {
        guard widget.valid, error == nil else { return }
        if let i = items.firstIndex(where: { $0.id == widget.id }) { items[i] = widget }
        else { items.append(widget) }
        persist()
    }
    func remove(_ id: UUID) { items.removeAll { $0.id == id }; persist() }
    func move(from: IndexSet, to: Int) { items.move(fromOffsets: from, toOffset: to); persist() }
    func resize(_ id: UUID, to size: HomeWidgetSize) {
        guard let i = items.firstIndex(where: { $0.id == id }), items[i].kind.sizes.contains(size) else { return }
        items[i].size = size
        persist()
    }
    func resize(_ id: UUID) {
        guard let i = items.firstIndex(where: { $0.id == id }) else { return }
        let sizes = items[i].kind.sizes
        items[i].size = sizes[((sizes.firstIndex(of: items[i].size) ?? 0) + 1) % sizes.count]
        persist()
    }
    func move(_ id: UUID, before target: UUID) {
        guard id != target, let from = items.firstIndex(where: { $0.id == id }),
              let to = items.firstIndex(where: { $0.id == target }) else { return }
        items.move(fromOffsets: IndexSet(integer: from), toOffset: to > from ? to + 1 : to)
        persist()
    }
    private func persist() {
        guard let key, error == nil else { return }
        do { defaults.set(try JSONEncoder().encode(items), forKey: key) }
        catch { self.error = "The Home layout could not be saved." }
    }
}

@MainActor enum HomeWidgetInventory {
    static func devices(_ model: AppModel) -> [Device] {
        guard model.mobileScopeIsCurrent else { return [] }
        var seen = Set<String>()
        return (model.nearby + model.cloud).filter { seen.insert(recordingDeviceID($0)).inserted }
            .sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending }
    }
    static func resolve(_ reference: HomeWidgetDevice?, model: AppModel) -> Device? {
        guard let reference else { return nil }
        return devices(model).first { recordingDeviceID($0) == reference.id }
    }
    static func status(_ device: Device, model: AppModel) -> String {
        if model.selectedDeviceReady, model.connected?.id == device.id { return "Connected" }
        if device.transport != .cloud { return "Nearby" }
        guard model.cloudError == nil, let presence = model.mobilePresence,
              presence.0.presenceError.isEmpty, Date().timeIntervalSince(presence.1) < 60,
              let row = presence.0.devices.first(where: { $0.selector == device.selector }) else { return "Status unknown" }
        return row.online ? "Online" : "Offline"
    }
}


// Deterministic dense packing lets a later small widget fill a preceding half-row.
struct HomeWidgetGridPlacement: Equatable {
    let column, row, columns, rows: Int
    static func pack(_ sizes: [HomeWidgetSize], fullWidth: Bool = false) -> [Self] {
        var occupied = Set<Int>()
        return sizes.map { size in
            let width = fullWidth ? 4 : size.columns
            var row = 0
            while true {
                for column in 0...(4 - width) {
                    let cells = (row..<(row + size.rows)).flatMap { r in (column..<(column + width)).map { r * 4 + $0 } }
                    if cells.allSatisfy({ !occupied.contains($0) }) {
                        occupied.formUnion(cells)
                        return Self(column: column, row: row, columns: width, rows: size.rows)
                    }
                }
                row += 1
            }
        }
    }
}
struct HomeWidgetGrid: Layout {
    let sizes: [HomeWidgetSize]
    var accessible = false
    private func frames(width: CGFloat, subviews: Subviews) -> [CGRect] {
        let placements = HomeWidgetGridPlacement.pack(sizes, fullWidth: accessible)
        let unit = max(1, (width - 36) / 4)
        if accessible {
            var y: CGFloat = 0
            return subviews.map { view in
                let height = max(160, view.sizeThatFits(.init(width: width, height: nil)).height)
                defer { y += height + 12 }
                return CGRect(x: 0, y: y, width: width, height: height)
            }
        }
        return placements.map { p in CGRect(x: CGFloat(p.column) * (unit + 12), y: CGFloat(p.row) * 86,
            width: CGFloat(p.columns) * unit + CGFloat(p.columns - 1) * 12,
            height: p.rows == 4 ? 332 : 160) }
    }
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? 360
        return CGSize(width: width, height: frames(width: width, subviews: subviews).map(\.maxY).max() ?? 0)
    }
    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        for (view, frame) in zip(subviews, frames(width: bounds.width, subviews: subviews)) {
            view.place(at: CGPoint(x: bounds.minX + frame.minX, y: bounds.minY + frame.minY), anchor: .topLeading, proposal: ProposedViewSize(frame.size))
        }
    }
}
