import SwiftUI

enum LogLevel: Int, CaseIterable {
    case unknown, trace, debug, info, warning, error, fatal
    init(_ value: WDLogLevel) { self = LogLevel(rawValue: Int(value.rawValue)) ?? .unknown }
    var title: String {
        switch self {
        case .unknown: return "Default"
        case .trace: return "Trace"
        case .debug: return "Debug"
        case .info: return "Info"
        case .warning: return "Warning"
        case .error: return "Error"
        case .fatal: return "Fault"
        }
    }
    var color: Color {
        switch self {
        case .trace, .debug, .unknown: return .secondary
        case .info: return .blue
        case .warning: return .orange
        case .error, .fatal: return .red
        }
    }
    var symbol: String {
        switch self {
        case .warning: return "exclamationmark.triangle.fill"
        case .error, .fatal: return "xmark.circle.fill"
        case .info: return "info.circle.fill"
        default: return "circle.fill"
        }
    }
}
struct LogEntry: Identifiable {
    let id = UUID()
    let timestampNanoseconds: UInt64
    let level: LogLevel
    let body: String
    let app: String
    let service: String
    private static let timeFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm:ss.SSS"
        return formatter
    }()
    var time: String {
        guard timestampNanoseconds != 0 else { return "—" }
        return Self.timeFormatter.string(from: Date(timeIntervalSince1970: Double(timestampNanoseconds) / 1_000_000_000))
    }
    var message: String {
        body.replacingOccurrences(of: "\u{001B}\\[[0-?]*[ -/]*[@-~]", with: "", options: .regularExpression)
            .trimmingCharacters(in: .newlines)
    }
    var source: String { [app, service].filter { !$0.isEmpty }.joined(separator: " / ") }
    var copyText: String { [time, level.title, source, message].filter { !$0.isEmpty }.joined(separator: "  ") }
}

struct LogsView: View {
    @ObservedObject var model: AppModel
    @State private var search = ""
    @State private var problemsOnly = false
    @State private var follow = true

    private var visibleLogs: [LogEntry] {
        model.logs.filter {
            (!problemsOnly || $0.level.rawValue >= LogLevel.warning.rawValue) &&
            (search.isEmpty || $0.body.localizedCaseInsensitiveContains(search) || $0.source.localizedCaseInsensitiveContains(search))
        }
    }
    var body: some View {
        VStack(spacing: 12) {
            HStack(spacing: 12) {
                Picker("App", selection: $model.logApp) {
                    Text("All apps").tag("")
                    ForEach(model.apps) { Text($0.appName).tag($0.appName) }
                }.frame(maxWidth: 300)
                TextField("Filter messages", text: $search).textFieldStyle(.roundedBorder)
                Toggle("Warnings & errors", isOn: $problemsOnly).toggleStyle(.button)
                Toggle("Follow", isOn: $follow).toggleStyle(.button)
                Button { model.logs = [] } label: { Image(systemName: "trash") }.help("Clear logs")
                Button {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(visibleLogs.map(\.copyText).joined(separator: "\n"), forType: .string)
                } label: { Image(systemName: "doc.on.doc") }.help("Copy visible logs")
            }
            if let error = model.logError {
                HStack { FailureNote(message: error); Button("Retry") { Task { await model.streamLogs() } } }
            }
            VStack(spacing: 0) {
                HStack(spacing: 14) {
                    Text("Time").frame(width: 96, alignment: .leading)
                    Text("Level").frame(width: 80, alignment: .leading)
                    Text("Message").frame(maxWidth: .infinity, alignment: .leading)
                }.font(.caption.weight(.medium)).foregroundStyle(.secondary)
                    .padding(.horizontal, 14).padding(.vertical, 10)
                    .background(.quaternary.opacity(0.4))
                Divider()
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(spacing: 0) {
                            ForEach(visibleLogs) { entry in
                                LogRow(entry: entry)
                                Divider().padding(.leading, 14)
                            }
                            Color.clear.frame(height: 1).id("bottom")
                        }.textSelection(.enabled)
                    }
                    .overlay {
                        if visibleLogs.isEmpty {
                            ContentUnavailableView(search.isEmpty && !problemsOnly ? "Waiting for logs" : "No matching messages",
                                                   systemImage: "text.alignleft",
                                                   description: Text("Messages from the selected app appear here."))
                        }
                    }
                    .onChange(of: model.logs.last?.id) { _, _ in if follow { proxy.scrollTo("bottom", anchor: .bottom) } }
                    .onChange(of: follow) { _, value in if value { proxy.scrollTo("bottom", anchor: .bottom) } }
                }
            }
            .background(Color(nsColor: .textBackgroundColor))
            .clipShape(RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.quaternary))
            HStack {
                Text("\(visibleLogs.count) messages")
                Spacer()
                Text("Latest 2,000 records")
            }.font(.caption).foregroundStyle(.secondary)
        }.padding(.horizontal, 28).padding(.bottom, 20)
        .onChange(of: model.logApp) { _, _ in Task { await model.streamLogs() } }
    }
}
private struct LogRow: View {
    let entry: LogEntry
    var body: some View {
        HStack(alignment: .top, spacing: 14) {
            Text(entry.time).font(.system(size: 11, design: .monospaced))
                .foregroundStyle(.secondary).frame(width: 96, alignment: .leading)
            Label(entry.level.title, systemImage: entry.level.symbol)
                .font(.system(size: 11, weight: .medium)).foregroundStyle(entry.level.color)
                .frame(width: 80, alignment: .leading)
            VStack(alignment: .leading, spacing: 5) {
                if !entry.source.isEmpty { Text(entry.source).font(.caption.weight(.medium)).foregroundStyle(.secondary) }
                Text(entry.message).font(.system(size: 12, design: .monospaced))
                    .foregroundStyle(entry.level.rawValue >= LogLevel.error.rawValue ? Color.red : Color.primary)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }.padding(.horizontal, 14).padding(.vertical, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(entry.level.rawValue >= LogLevel.warning.rawValue ? entry.level.color.opacity(0.045) : .clear)
            .contextMenu { Button("Copy message") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(entry.copyText, forType: .string)
            } }
    }
}
