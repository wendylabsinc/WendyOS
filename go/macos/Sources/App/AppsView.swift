import SwiftUI

extension AppState {
    var title: String {
        switch self {
        case .running: return "Running"
        case .crashLooping: return "Crashing"
        case .stopped: return "Stopped"
        case .unknown: return "Unknown"
        }
    }
    var color: Color {
        switch self {
        case .running: return .green
        case .crashLooping: return .red
        case .stopped, .unknown: return .secondary
        }
    }
}
struct AppsView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: AppModel
    let apps: [DeviceApp]
    let showLogs: (DeviceApp) -> Void
    @State private var selection: Set<String> = []

    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        Table(apps, selection: $selection) {
            TableColumn("Application") { app in
                HStack(spacing: 12) {
                    Image(systemName: "shippingbox").foregroundStyle(.secondary).font(.title3).frame(width: 24)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(app.appName).font(.body.weight(.medium)).lineLimit(1).help(app.appName)
                        Text(details(app)).font(.caption).foregroundStyle(.secondary).lineLimit(1).help(details(app))
                    }
                }.frame(height: 54)
            }.width(min: 240, ideal: 460)
            TableColumn("Version") { app in
                Text(app.appVersion ?? "—").foregroundStyle(.secondary).font(.callout.monospacedDigit())
            }.width(80)
            TableColumn("Status") { app in
                HStack(spacing: 8) {
                    Circle().fill(app.runningState.color).frame(width: 8, height: 8)
                    Text(app.runningState.title).font(.callout)
                }.accessibilityElement(children: .combine)
            }.width(120)
            TableColumn("") { app in
                if app.hasWebpage {
                    Button { actions.submit { await model.openApp(app) } } label: {
                        Label("Open", systemImage: "arrow.up.right.square")
                    }
                    .buttonStyle(.bordered).controlSize(.small)
                    .disabled(app.runningState != .running)
                    .help(app.runningState == .running ? "Open webpage on port \(app.httpPort)" : "Start the app to open its webpage")
                }
            }.width(88)
            TableColumn("") { app in
                Button { showLogs(app) } label: {
                    HStack(spacing: 6) {
                        Image(systemName: "doc.text").foregroundStyle(.tint)
                        Text("Logs").foregroundStyle(.primary)
                    }
                }.buttonStyle(.bordered).controlSize(.small)
            }.width(88)
        }
        .onChange(of: model.deviceSession) { _, _ in selection.removeAll() }
        .onChange(of: apps.map(\.id)) { _, ids in selection.formIntersection(ids) }
        .contextMenu(forSelectionType: String.self) { ids in
            let selectedApps = apps.filter { ids.contains($0.id) }
            if selectedApps.count > 1 {
                Button("Stop Selected Apps", systemImage: "stop.fill") {
                    let session = model.deviceSession
                    actions.submit { await model.stopApps(selectedApps, expectedSession: session) }
                }
                .disabled(selectedApps.allSatisfy { $0.runningState == .stopped } || selectedApps.contains { model.appActions.contains($0.appName) })
            } else if let app = selectedApps.first {
                AppControlMenu(actions: actions, model: model, app: app.appName, state: app.runningState)
                if app.hasWebpage {
                    Button("Open Webpage", systemImage: "arrow.up.right.square") { actions.submit { await model.openApp(app) } }
                        .disabled(app.runningState != .running)
                }
                Divider()
                Button("Show Logs", systemImage: "doc.text") { showLogs(app) }
                Divider()
                AppDeleteMenu(model: model, app: app.appName)
            }
        }
        #if os(macOS)
        .tableStyle(.bordered)
        #endif
    }
    private func details(_ app: DeviceApp) -> String {
        if !app.services.isEmpty {
            return app.services.map { $0.name + " · " + $0.state.title }.joined(separator: "    ")
        }
        return app.terminationReason?.replacingOccurrences(of: "_", with: " ").capitalized ?? "Single service"
    }
}

struct AppDeleteMenu: View {
    @ObservedObject var model: AppModel
    let app: String
    var body: some View {
        Button("Delete App", systemImage: "trash", role: .destructive) {
            model.requestAppDeletion(app)
        }.disabled(model.appActions.contains(app))
    }
}

enum AppStopBatch {
    @MainActor
    static func run(names: [String], isCurrent: () -> Bool,
                    stop: (String) async throws -> Void) async -> [String] {
        var failures: [String] = []
        for name in Set(names).sorted() {
            guard isCurrent(), !Task.isCancelled else { break }
            do { try await stop(name) }
            catch { failures.append("\(name): \(error.localizedDescription)") }
        }
        return failures
    }
}

struct AppControlMenu: View {
    let actions: AsyncActions
    @ObservedObject var model: AppModel
    let app: String
    let state: AppState
    var body: some View {
        if model.appActions.contains(app) {
            Text("Updating App…")
        } else {
            Button("Start", systemImage: "play.fill") { actions.submit { await model.setAppRunning(app, running: true) } }
                .disabled(state == .running)
            Button("Stop", systemImage: "stop.fill") { actions.submit { await model.setAppRunning(app, running: false) } }
                .disabled(state == .stopped)
        }
    }
}

// Arrow keys belong to the focused list. Selection changes from refreshes or
// other controls must not pull focus away from text fields and sheets.
struct KeyboardRowSelection: ViewModifier {
    let ids: [String]
    @Binding var selection: String?
    var focusRequest: Int = 0
    @FocusState private var focused: Bool

    nonisolated static func next(in ids: [String], selected: String?, down: Bool) -> String? {
        guard !ids.isEmpty else { return nil }
        guard let selected, let index = ids.firstIndex(of: selected) else {
            return down ? ids.first : ids.last
        }
        return ids[min(max(index + (down ? 1 : -1), 0), ids.count - 1)]
    }

    func body(content: Content) -> some View {
        content
            .focusable()
            .focused($focused)
            .simultaneousGesture(TapGesture().onEnded { focused = true })
            .onKeyPress(keys: [.upArrow, .downArrow], phases: [.down, .repeat]) { press in
                guard press.modifiers.isEmpty else { return .ignored }
                selection = Self.next(in: ids, selected: selection, down: press.key == .downArrow)
                return .handled
            }
            .onChange(of: focusRequest) { _, _ in focused = true }
    }
}
extension View {
    func keyboardRowSelection(_ ids: [String], selection: Binding<String?>, focusRequest: Int = 0) -> some View {
        modifier(KeyboardRowSelection(ids: ids, selection: selection, focusRequest: focusRequest))
    }
}
