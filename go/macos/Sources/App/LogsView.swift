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
        #if os(iOS)
        mobileContent
        #else
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
                    PlatformClipboard.copy(visibleLogs.map(\.copyText).joined(separator: "\n"))
                } label: { Image(systemName: "doc.on.doc") }.help("Copy visible logs")
            }
            if let error = model.logError {
                HStack { FailureNote(message: error); Button("Retry") { Task { await model.streamLogs() } } }
            }
            VStack(spacing: 0) {
                #if os(macOS)
                HStack(spacing: 14) {
                    Text("Time").frame(width: 96, alignment: .leading)
                    Text("Level").frame(width: 80, alignment: .leading)
                    Text("Message").frame(maxWidth: .infinity, alignment: .leading)
                }.font(.caption.weight(.medium)).foregroundStyle(.secondary)
                    .padding(.horizontal, 14).padding(.vertical, 10)
                    .background(.quaternary.opacity(0.4))
                Divider()
                #endif
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
            .background(PlatformColors.text)
            .clipShape(RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.quaternary))
            HStack {
                Text("\(visibleLogs.count) messages")
                Spacer()
                Text("Latest 2,000 records")
            }.font(.caption).foregroundStyle(.secondary)
        }
        #if os(iOS)
        .padding(.horizontal, 12).padding(.bottom, 8)
        #else
        .padding(.horizontal, 28).padding(.bottom, 20)
        #endif
        .task(id: model.logApp) { await model.streamLogs() }
        #endif
    }
    #if os(iOS)
    @State private var inspectedLog: LogEntry?
    private var mobileContent: some View {
        ScrollViewReader { proxy in
            List {
                if let error = model.logError {
                    Section {
                        FailureNote(message: error)
                        Button("Retry") { Task { await model.streamLogs() } }
                    }
                }
                ForEach(visibleLogs) { entry in
                    Button { inspectedLog = entry } label: {
                        VStack(alignment: .leading, spacing: 6) {
                            HStack(spacing: 6) {
                                Image(systemName: entry.level.symbol).foregroundStyle(entry.level.color)
                                Text(entry.time).monospacedDigit()
                                if model.logApp.isEmpty && !entry.source.isEmpty {
                                    Text("· " + entry.source).lineLimit(1).truncationMode(.middle)
                                }
                                Spacer(minLength: 0)
                            }.font(.caption2).foregroundStyle(.secondary)
                            Text(entry.message).font(.system(.callout, design: .monospaced))
                                .foregroundStyle(entry.level.rawValue >= LogLevel.error.rawValue ? Color.red : .primary)
                                .lineLimit(6).multilineTextAlignment(.leading)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }.padding(.vertical, 4).contentShape(Rectangle())
                    }.buttonStyle(.plain)
                        .contextMenu { Button("Copy message", systemImage: "doc.on.doc") { PlatformClipboard.copy(entry.copyText) } }
                        .listRowBackground(entry.level.rawValue >= LogLevel.warning.rawValue ? entry.level.color.opacity(0.06) : Color.clear)
                }
                Color.clear.frame(height: 1).id("bottom").listRowSeparator(.hidden)
            }
            .listStyle(.plain)
            .overlay {
                if visibleLogs.isEmpty && model.logError == nil {
                    ContentUnavailableView(search.isEmpty && !problemsOnly ? "Waiting for logs" : "No matching messages", systemImage: "text.alignleft")
                }
            }
            .safeAreaInset(edge: .top, spacing: 0) {
                VStack(spacing: 8) {
                    HStack(spacing: 12) {
                        Menu {
                            Picker("App", selection: $model.logApp) {
                                Text("All apps").tag("")
                                ForEach(model.apps) { Text($0.appName).tag($0.appName) }
                            }
                            Divider()
                            Button("Copy visible logs", systemImage: "doc.on.doc") { PlatformClipboard.copy(visibleLogs.map(\.copyText).joined(separator: "\n")) }
                            Button("Clear logs", systemImage: "trash") { model.logs = [] }
                            Text("\(visibleLogs.count) messages · Latest 2,000 records")
                        } label: {
                            HStack(spacing: 5) {
                                Text(model.logApp.isEmpty ? "All apps" : model.logApp).lineLimit(1).truncationMode(.middle)
                                Image(systemName: "chevron.down").font(.caption2)
                            }.frame(maxWidth: .infinity, alignment: .leading)
                        }
                        Toggle(isOn: $follow) { Label("Follow", systemImage: "arrow.down.to.line") }
                            .toggleStyle(.button).fixedSize().accessibilityHint("Automatically scroll to new log messages")
                    }
                    HStack(spacing: 10) {
                        HStack(spacing: 6) {
                            Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                            TextField("Filter messages", text: $search).textInputAutocapitalization(.never).autocorrectionDisabled()
                            if !search.isEmpty {
                                Button { search = "" } label: { Image(systemName: "xmark.circle.fill") }.accessibilityLabel("Clear filter")
                            }
                        }.padding(10).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 10))
                        Menu {
                            Toggle("Warnings & errors only", isOn: $problemsOnly)
                        } label: {
                            Label("Filters", systemImage: problemsOnly ? "line.3.horizontal.decrease.circle.fill" : "line.3.horizontal.decrease.circle")
                        }.fixedSize().accessibilityValue(problemsOnly ? "Warnings and errors" : "All levels")
                    }
                }.font(.subheadline).padding(.horizontal, 16).padding(.vertical, 8).background(.bar)
            }
            .onChange(of: model.logs.last?.id) { _, _ in if follow { proxy.scrollTo("bottom", anchor: .bottom) } }
            .onChange(of: follow) { _, value in if value { proxy.scrollTo("bottom", anchor: .bottom) } }
            .task(id: model.logApp) { await model.streamLogs() }
            .sheet(item: $inspectedLog) { entry in
                NavigationStack {
                    Form {
                        Section {
                            LabeledContent("Time", value: entry.time)
                            LabeledContent("Level", value: entry.level.title)
                            if !entry.source.isEmpty { LabeledContent("Source", value: entry.source) }
                        }
                        Section("Message") { Text(entry.message).font(.callout.monospaced()).textSelection(.enabled) }
                    }
                    .navigationTitle("Log message").navigationBarTitleDisplayMode(.inline)
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Close") { inspectedLog = nil } } }
                }.presentationBackground(Color(uiColor: .systemGroupedBackground))
            }
        }
    }
    #endif

}
private struct LogRow: View {
    let entry: LogEntry
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            #if os(iOS)
            HStack(spacing: 8) {
                Text(entry.time).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                Spacer()
                Label(entry.level.title, systemImage: entry.level.symbol).font(.caption).foregroundStyle(entry.level.color)
            }
            if !entry.source.isEmpty { Text(entry.source).font(.caption).foregroundStyle(.secondary) }
            Text(entry.message).font(.system(.caption, design: .monospaced))
                .foregroundStyle(entry.level.rawValue >= LogLevel.error.rawValue ? Color.red : Color.primary)
                .fixedSize(horizontal: false, vertical: true).frame(maxWidth: .infinity, alignment: .leading)
            #else
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
        }
            #endif
        }.padding(.horizontal, 14).padding(.vertical, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(entry.level.rawValue >= LogLevel.warning.rawValue ? entry.level.color.opacity(0.045) : .clear)
            .contextMenu { Button("Copy message") {
                PlatformClipboard.copy(entry.copyText)
            } }
    }
}


struct DeviceLogsPage: View {
    @ObservedObject var model: AppModel
    var body: some View {
        LogsView(model: model)
            .navigationTitle("Logs")
            .task(id: model.deviceSession) { await model.streamLogs() }
            .onDisappear { Task { try? await model.client.stopLogs() } }
    }
}
