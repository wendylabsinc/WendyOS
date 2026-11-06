import SwiftUI

extension MobileRecordingLibrary {
    var labelCoverage: String? {
        let loaded = episodes.count(where: \.detailsLoaded)
        return loaded < episodes.count ? "Labels checked for \(loaded) of \(episodes.count) episodes" : nil
    }
    var sizeText: String {
        Self.sizeText(for: episodes)
    }

    static func sizeText(for episodes: [MobileRecordingEpisode]) -> String {
        let total = episodes.reduce(Int64(0)) { sum, episode in
            let (value, overflow) = sum.addingReportingOverflow(max(0, episode.bytes))
            return overflow ? Int64.max : value
        }
        return ByteCountFormatter.string(fromByteCount: total, countStyle: .file)
    }

    var labels: [(name: String, count: Int)] {
        Dictionary(grouping: episodes.compactMap(\.trigger), by: { $0 }).map { (name: $0.key, count: $0.value.count) }
            .sorted { $0.count == $1.count ? $0.name < $1.name : $0.count > $1.count }
    }

    func episodes(matching trigger: String?) -> [MobileRecordingEpisode] {
        guard let trigger else { return episodes }
        return episodes.filter { $0.trigger == trigger }
    }

    func exportTargets(trigger: String?, selectedID: String?) -> [MobileRecordingEpisode] {
        let visible = episodes(matching: trigger)
        if let selectedID { return visible.filter { $0.id == selectedID } }
        if trigger != nil && labelCoverage != nil { return [] }
        return visible
    }
}

enum RecordingAppearance {
    static let card = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .secondarySystemBackground : .white })
    static let ink = Color(red: 23/255, green: 28/255, blue: 35/255)
    static func dot(_ label: String?) -> Color {
        switch label?.lowercased() {
        case "person": Color(red: 1, green: 59/255, blue: 48/255)
        case "door opened": .orange
        case "motion": Color(red: 1, green: 214/255, blue: 10/255)
        case "schedule": Color(red: 100/255, green: 210/255, blue: 1)
        case "manual": Color(red: 159/255, green: 226/255, blue: 191/255)
        case "loud sound": .purple
        default: .gray
        }
    }

    static func time(_ date: Date) -> String {
        if Calendar.current.isDateInToday(date) { return date.formatted(date: .omitted, time: .shortened) }
        if Calendar.current.isDateInYesterday(date) { return "yesterday" }
        return date.formatted(date: .abbreviated, time: .omitted)
    }
}

// Keep the chips on natural rows, including when the user increases text size.
struct RecordingChipLayout: Layout {
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        arrange(width: proposal.width ?? .infinity, subviews: subviews).size
    }
    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let layout = arrange(width: bounds.width, subviews: subviews)
        for (view, frame) in zip(subviews, layout.frames) {
            view.place(at: CGPoint(x: bounds.minX + frame.minX, y: bounds.minY + frame.minY),
                       anchor: .topLeading, proposal: ProposedViewSize(frame.size))
        }
    }
    private func arrange(width: CGFloat, subviews: Subviews) -> (frames: [CGRect], size: CGSize) {
        var x: CGFloat = 0, y: CGFloat = 0, row: CGFloat = 0, used: CGFloat = 0
        var frames: [CGRect] = []
        for view in subviews {
            var size = view.sizeThatFits(.unspecified)
            if size.width > width { size = view.sizeThatFits(ProposedViewSize(width: width, height: nil)) }
            if x > 0 && x + size.width > width { y += row + 2; x = 0; row = 0 }
            frames.append(CGRect(origin: CGPoint(x: x, y: y), size: size))
            used = max(used, x + size.width)
            x += size.width + 6; row = max(row, size.height)
        }
        return (frames, CGSize(width: used, height: y + row))
    }
}

struct RecordingDeviceCard: View {
    let library: MobileRecordingLibrary
    let open: (String?) -> Void
    @Environment(\.dynamicTypeSize) private var textSize
    @ScaledMetric(relativeTo: .title2) private var countFont = 26.0

    var body: some View {
        let latest = library.episodes.first
        VStack(alignment: .leading, spacing: 0) {
            Button { open(nil) } label: {
                HStack(alignment: .center, spacing: 12) {
                    if let latest { RecordingThumbnail(device: library.device, episode: latest).frame(width: 60, height: 60).clipShape(.rect(cornerRadius: 14)) }
                    VStack(alignment: .leading, spacing: 3) {
                        Text(library.device.title).font(.callout.weight(.semibold)).fixedSize(horizontal: false, vertical: true)
                        Text([library.model, latest?.started.map { "latest " + RecordingAppearance.time($0) }, library.sizeText]
                            .compactMap { $0 }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                        if textSize.isAccessibilitySize { Text(countDescription).font(.subheadline.weight(.semibold)) }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                    if !textSize.isAccessibilitySize {
                        VStack(alignment: .trailing, spacing: 0) {
                            Text("\(library.episodes.count)").font(.system(size: countFont, weight: .bold)).monospacedDigit()
                            Text(library.episodes.count == 1 ? "episode" : "episodes").font(.caption2).foregroundStyle(.secondary)
                        }.fixedSize()
                    }
                }.contentShape(.rect)
            }.buttonStyle(.plain).accessibilityIdentifier("data.device." + library.device.id)
            if let coverage = library.labelCoverage {
                Text(coverage).font(.caption).foregroundStyle(.secondary).padding(.top, 6)
                    .padding(.leading, textSize.isAccessibilitySize ? 0 : 72)
            }
            if !library.labels.isEmpty {
                RecordingChipLayout {
                    ForEach(library.labels.prefix(3), id: \.name) { label in
                        Button { open(label.name) } label: {
                            HStack(spacing: 5) {
                                Circle().fill(RecordingAppearance.dot(label.name)).frame(width: 6, height: 6)
                                Text(label.name).fontWeight(.semibold)
                                Text("\(label.count)").foregroundStyle(.secondary)
                            }.font(.caption).padding(.horizontal, 9).padding(.vertical, 4)
                                .background(MobileLensStyle.background, in: .capsule).frame(minHeight: 44).contentShape(.rect)
                        }.buttonStyle(.plain).accessibilityLabel("\(label.name), \(label.count) episodes")
                    }
                }.padding(.leading, textSize.isAccessibilitySize ? 0 : 72)
            }
        }.padding(12).background(RecordingAppearance.card, in: .rect(cornerRadius: 22))
    }
    private var countDescription: String { "\(library.episodes.count) \(library.episodes.count == 1 ? "episode" : "episodes")" }
}

struct RecordingThumbnail: View {
    let device: Device
    let episode: MobileRecordingEpisode
    @Environment(\.recordingPosters) private var posters
    var body: some View {
        ZStack {
            if let poster = posters?.image(device: device, episode: episode.id) {
                GeometryReader { proxy in
                    Image(uiImage: poster).resizable().scaledToFill().frame(width: proxy.size.width, height: proxy.size.height)
                }
            } else { artwork }
        }.clipped().accessibilityHidden(true)
    }

    @ViewBuilder private var artwork: some View {
        ZStack {
            switch episode.kind {
            case .video:
                Color(red: 36/255, green: 43/255, blue: 53/255)
                Canvas { context, size in
                    for x in stride(from: -size.height, to: size.width, by: 16) {
                        var line = Path(); line.move(to: CGPoint(x: x, y: size.height)); line.addLine(to: CGPoint(x: x + size.height, y: 0))
                        context.stroke(line, with: .color(.white.opacity(0.045)), lineWidth: 7)
                    }
                }
            case .audio:
                Color(red: 0.92, green: 0.90, blue: 0.99)
                Canvas { context, size in
                    let bars = [0.22, 0.47, 0.31, 0.73, 0.91, 0.55, 0.69, 0.36, 0.20]
                    let step = (size.width - 12) / CGFloat(bars.count)
                    for (i, level) in bars.enumerated() {
                        let h = level * size.height * 0.80
                        let bar = CGRect(x: 6 + CGFloat(i) * step, y: (size.height - h) / 2, width: max(1, step - 2), height: h)
                        context.fill(Path(roundedRect: bar, cornerRadius: 2), with: .color(.purple))
                    }
                }
            case .rosbag:
                Color(red: 0.12, green: 0.16, blue: 0.20)
                Canvas { context, size in
                    let colors: [Color] = [.mint, .cyan, .orange]
                    for (index, color) in colors.enumerated() {
                        let y = (CGFloat(index) + 1) * size.height / 4
                        for n in 0..<4 {
                            let w = (size.width - 14) / 4, x = 7 + CGFloat(n) * w
                            let bar = CGRect(x: x, y: y - 2, width: max(1, w - 2), height: 4)
                            context.fill(Path(roundedRect: bar, cornerRadius: 2), with: .color(color.opacity(n == index ? 0.4 : 0.8)))
                        }
                    }
                }
            case .images:
                Color(red: 0.87, green: 0.85, blue: 0.80)
                Image(systemName: "photo.stack.fill").font(.title).foregroundStyle(Color(white: 0.55))
            case .dataset:
                Color(red: 0.82, green: 0.90, blue: 0.85)
                Canvas { context, size in
                    let gap: CGFloat = 3, w = (size.width - 20 - 3 * gap) / 4, h = (size.height - 16 - 2 * gap) / 3
                    for i in 0..<12 {
                        let rect = CGRect(x: 10 + CGFloat(i % 4) * (w + gap), y: 8 + CGFloat(i / 4) * (h + gap), width: w, height: h)
                        context.fill(Path(roundedRect: rect, cornerRadius: 2), with: .color(MobileLensStyle.green.opacity(0.24 + Double(i % 4) * 0.12)))
                    }
                }
            case .unknown:
                Color.secondary.opacity(0.12)
                Image(systemName: "cylinder.split.1x2").font(.title3).foregroundStyle(.secondary)
            }
        }
    }
}
