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
    @State private var visible = false

    private struct Request: Equatable {
        let device, episode, file: String
    }
    private var request: Request? {
        guard visible, posters != nil, let file = episode.previewFile else { return nil }
        return Request(device: device.selector, episode: episode.id, file: file)
    }

    var body: some View {
        let poster = posters?.poster(device: device, episode: episode.id)
        ZStack {
            if case .image(let image) = poster {
                GeometryReader { proxy in
                    Image(uiImage: image).resizable().scaledToFill().frame(width: proxy.size.width, height: proxy.size.height)
                }
            } else {
                switch episode.kind {
                case .video:
                    Color(red: 36/255, green: 43/255, blue: 53/255)
                    Canvas { context, size in
                        for x in stride(from: -size.height, to: size.width, by: 20) {
                            var line = Path(); line.move(to: CGPoint(x: x, y: size.height)); line.addLine(to: CGPoint(x: x + size.height, y: 0))
                            context.stroke(line, with: .color(.white.opacity(0.035)), lineWidth: 8)
                        }
                    }
                case .audio:
                    Color(red: 232/255, green: 246/255, blue: 238/255)
                    if case .waveform(let peaks) = poster { waveform(peaks) }
                    else { Image(systemName: "speaker.wave.2").font(.title3).foregroundStyle(MobileLensStyle.green) }
                case .rosbag:
                    RecordingAppearance.ink
                    topicBars
                case .images:
                    Color(red: 230/255, green: 226/255, blue: 216/255)
                    GeometryReader { proxy in
                        ZStack {
                            photo(0xC9C4B8).rotationEffect(.degrees(6)).offset(x: 4, y: -3)
                            photo(0xB8B2A5).rotationEffect(.degrees(-4)).offset(x: -3)
                            photo(0x9A978F).offset(y: 3)
                        }.frame(width: proxy.size.width * 0.46, height: proxy.size.height * 0.52)
                            .position(x: proxy.size.width/2, y: proxy.size.height/2)
                    }
                case .dataset:
                    Color(white: 249/255)
                    datasetGrid
                case .unknown:
                    Color.secondary.opacity(0.12)
                    Image(systemName: "cylinder.split.1x2").font(.title3).foregroundStyle(.secondary)
                }
            }
        }.clipped().accessibilityHidden(true)
            .onScrollVisibilityChange(threshold: 0.05) { visible = $0 }
            .task(id: request) {
                if let request { await posters?.load(device: device, episode: request.episode, file: request.file) }
            }
    }

    private var mint: Color { Color(red: 159/255, green: 226/255, blue: 191/255) }
    private func waveform(_ peaks: [UInt8]) -> some View {
        Canvas { context, size in
            let step = (size.width - 12) / CGFloat(max(1, peaks.count))
            for (i, peak) in peaks.enumerated() {
                let h = max(2, CGFloat(peak)/255 * (size.height - 10))
                let bar = CGRect(x: 6 + CGFloat(i) * step, y: (size.height-h)/2, width: max(1, step-1), height: h)
                context.fill(Path(roundedRect: bar, cornerRadius: 2), with: .color(MobileLensStyle.green))
            }
        }
    }
    // One meter per recorded topic. Width encodes its message count relative
    // to the busiest topic; the manifest does not promise a message timeline.
    private var topicBars: some View {
        Canvas { context, size in
            let counts = episode.previewCounts.prefix(3)
            let maxCount = Double(max(1, counts.max() ?? 1))
            let colors = [MobileLensStyle.green, mint, Color(red: 1, green: 149/255, blue: 0)]
            for (i, count) in counts.enumerated() {
                let y = counts.count == 1 ? (size.height - 6)/2 : 8 + CGFloat(i) * (size.height - 22)/CGFloat(max(1, counts.count-1))
                let track = CGRect(x: 6, y: y, width: size.width-12, height: 6)
                context.fill(Path(roundedRect: track, cornerRadius: 2), with: .color(.white.opacity(0.10)))
                let bar = CGRect(x: track.minX, y: y, width: max(3, track.width * Double(count)/maxCount), height: 6)
                context.fill(Path(roundedRect: bar, cornerRadius: 2), with: .color(colors[i]))
            }
        }
    }

    private var datasetGrid: some View {
        Canvas { context, size in
            let counts = episode.previewCounts.prefix(12), peak = counts.max() ?? 0
            let gap: CGFloat = 3, w = (size.width-16-5*gap)/6, h = (size.height-16-gap)/2
            for i in 0..<12 {
                let count = i < counts.count ? counts[i] : 0
                let color = count == 0 ? Color(red: 230/255, green: 226/255, blue: 216/255) : count == peak ? MobileLensStyle.green : mint
                let rect = CGRect(x: 8+CGFloat(i%6)*(w+gap), y: 8+CGFloat(i/6)*(h+gap), width: w, height: h)
                context.fill(Path(roundedRect: rect, cornerRadius: 3), with: .color(color))
            }
        }
    }

    private func photo(_ rgb: Int) -> some View {
        RoundedRectangle(cornerRadius: 5)
            .fill(Color(red: Double(rgb >> 16)/255, green: Double((rgb >> 8) & 255)/255, blue: Double(rgb & 255)/255))
            .overlay { RoundedRectangle(cornerRadius: 5).strokeBorder(.white, lineWidth: 2) }
    }
}
