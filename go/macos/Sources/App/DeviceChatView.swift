import SwiftUI

struct ChatModelOption: Identifiable {
    let id, name: String
    var displayName: String { Self.readableName(name.isEmpty ? id : name) }
    static func readableName(_ identifier: String) -> String {
        let parts = identifier.components(separatedBy: ":")
        var value = parts[0].components(separatedBy: "/").last ?? identifier
        if !identifier.contains("/"), parts.count > 1,
           parts[1].range(of: #"^\d+(?:\.\d+)?[bBmM]$"#, options: .regularExpression) != nil {
            value += "-" + parts[1].uppercased()
        }
        value = value.replacingOccurrences(of: #"(?i)\.gguf$|[-_]GGUF$"#, with: "", options: .regularExpression)
        value = value.replacingOccurrences(of: #"(?i)[-_.](?:Q[2-8](?:_[A-Z0-9]+)*|IQ[1-4](?:_[A-Z0-9]+)*|BF16|F16|F32)$"#, with: "", options: .regularExpression)
        value = value.replacingOccurrences(of: "-", with: " ").replacingOccurrences(of: "_", with: " ")
        for family in ["Qwen", "Llama", "Gemma", "Phi", "Nemotron"] {
            value = value.replacingOccurrences(of: "(?i)\\b" + family + "(?=[0-9]|\\b)", with: family + " ", options: .regularExpression)
        }
        value = value.replacingOccurrences(of: #"(?i)[\s·]+(\d+(?:\.\d+)?B)\b"#, with: " · $1", options: .regularExpression)
        value = value.replacingOccurrences(of: #"\s+"#, with: " ", options: .regularExpression).trimmingCharacters(in: .whitespaces)
        return value.isEmpty ? identifier : value
    }
}
struct ChatDeviceCard: Identifiable {
    let kind, title: String
    let camera: UInt32
    var id: String { kind + String(camera) }
}
struct ChatMessage: Identifiable {
    let id = UUID()
    let isUser: Bool
    var text: String
    var trace = ""
    var interrupted = false
    var cards: [ChatDeviceCard] = []
}
struct ChatApproval: Identifiable {
    let id: UInt64
    let action, app: String
    var isNotificationPermission: Bool { action == "enable_campaign_notifications" }
    var isCampaign: Bool { action == "create_person_detection_campaign" }
    var title: String { isNotificationPermission ? "Allow campaign notifications?" : isCampaign ? "Create person-detection campaign?" : "Allow device action?" }
    var confirmLabel: String { isNotificationPermission ? "Allow notifications" : isCampaign ? "Create campaign" : "Allow" }
    var arguments: [String: Any] {
        guard let data = app.data(using: .utf8), let args = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return [:] }
        return args
    }
    var campaignName: String { arguments[isCampaign ? "name" : "campaign"] as? String ?? "this campaign" }
    var cameraName: String {
        let name = arguments["camera"] as? String ?? "Unknown camera"
        return name == "*" ? "All current and future cameras" : name
    }
    var clipSummary: String { "\(arguments["before_seconds"] as? Int ?? 0)s before · \(arguments["after_seconds"] as? Int ?? 0)s after detection" }
}

private struct ChatApprovalView: View {
    let approval: ChatApproval
    let deviceName: String
    let resolve: (Bool) -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label(approval.title, systemImage: approval.isNotificationPermission ? "bell.badge" : approval.isCampaign ? "person.crop.rectangle" : "hand.raised")
                .font(.headline)
            Text(deviceName).font(.caption).foregroundStyle(.secondary)
            if approval.isNotificationPermission {
                Text("Allow “\(approval.campaignName)” to send notifications through Wendy Cloud?")
                Text("Applies across devices using this campaign in this device’s organization. This permission does not upload recordings.")
                    .font(.caption).foregroundStyle(.secondary)
                NotificationReceivingView()
            } else if approval.isCampaign {
                VStack(alignment: .leading, spacing: 10) {
                    detailRow("Name", value: approval.campaignName)
                    detailRow("Camera", value: approval.cameraName)
                    detailRow("Recording", value: approval.clipSummary)
                    detailRow("Notifications", value: approval.arguments["notify"] as? Bool == true ? "Ask for permission next" : "Off")
                    detailRow("Upload clips", value: approval.arguments["upload"] as? Bool == true ? "Automatically" : "Off")
                }.font(.callout).padding(.vertical, 4)
                Text("Starts person detection and records clips on the device. The device may download a detection model.")
                    .font(.caption).foregroundStyle(.secondary)
                DisclosureGroup("Detection settings") {
                    VStack(alignment: .leading, spacing: 10) {
                        detailRow("Confidence", value: String(format: "%.0f%%", (approval.arguments["threshold"] as? Double ?? 0) * 100))
                        detailRow("Alert interval", value: "\(approval.arguments["cooldown_seconds"] as? Int ?? 0) seconds")
                    }.padding(.top, 8)
                }.font(.caption)
            } else {
                Text(approval.action.replacingOccurrences(of: "_", with: " ").capitalized).font(.callout)
                DisclosureGroup("Details") {
                    ScrollView { Text(approval.app).font(.caption.monospaced()).textSelection(.enabled) }.frame(maxHeight: 120)
                }.font(.caption)
            }
            Divider().padding(.vertical, 4)
            HStack {
                Spacer()
                Button("Cancel") { resolve(false) }.buttonStyle(.bordered)
                Button(approval.confirmLabel) { resolve(true) }.buttonStyle(.borderedProminent)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(20).background(PlatformColors.control, in: RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(PlatformColors.separator.opacity(0.5)))
    }

    private func detailRow(_ label: String, value: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            Text(label).foregroundStyle(.secondary).frame(width: 100, alignment: .leading)
            Text(value).frame(maxWidth: .infinity, alignment: .leading)
        }.fixedSize(horizontal: false, vertical: true)
    }
}
struct ChatProgress { let cards: [ChatDeviceCard]; let text, error, activity, trace: String; let done: Bool; let approval: ChatApproval? }

extension GoClient {
    func chatModels(session: UInt64, port: UInt32) async throws -> [ChatModelOption] {
        try await performModelRequest {
            guard let p = WDChatModels(session, port) else { throw WendyError(message: "Missing model list.") }
            defer { WDChatModelsFree(p) }; try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { ChatModelOption(id: Self.string($0.id), name: Self.string($0.name)) }
        }
    }
    func startChat(session: UInt64, turn: UInt64, port: UInt32, model: String, messages: [ChatMessage]) async throws {
        try await perform {
            let strings = CStringStorage(); defer { withExtendedLifetime(strings) {} }
            var input = messages.map { WDChatMessage(role: $0.isUser ? WD_CHAT_USER : WD_CHAT_ASSISTANT, text: strings.add($0.text)) }
            try input.withUnsafeMutableBufferPointer { try Self.finish(WDStartChat(session, turn, port, strings.add(model), $0.baseAddress, $0.count)) }
        }
    }
    func pollChat(session: UInt64, turn: UInt64) async throws -> ChatProgress {
        try await perform {
            guard let p = WDPollChat(session, turn) else { throw WendyError(message: "Missing chat response.") }
            defer { WDChatFree(p) }; try Self.check(p.pointee.status)
            let value = p.pointee
            let approval = value.approval_id == 0 ? nil : ChatApproval(id: value.approval_id, action: Self.string(value.approval_action), app: Self.string(value.approval_app))
            let cards = UnsafeBufferPointer(start: value.cards, count: value.card_count).map { ChatDeviceCard(kind: Self.string($0.kind), title: Self.string($0.title), camera: $0.camera) }
            return ChatProgress(cards: cards, text: Self.string(value.text), error: Self.string(value.error), activity: Self.string(value.activity), trace: Self.string(value.trace), done: value.done != 0, approval: approval)
        }
    }
    func resolveChatApproval(session: UInt64, turn: UInt64, approval: UInt64, allow: Bool) async throws {
        try await perform { try Self.finish(WDResolveChatApproval(session, turn, approval, allow ? 1 : 0)) }
    }
    func stopChat(session: UInt64, turn: UInt64) async throws { try await perform { try Self.finish(WDStopChat(session, turn)) } }
}

@MainActor final class DeviceChatModel: ObservableObject {
    let session: UInt64
    @Published var port = "11434"
    @Published var models: [ChatModelOption] = []
    @Published var model = ""
    @Published var messages: [ChatMessage] = []
    @Published var prompt = ""
    @Published var loading = false
    @Published var streaming = false
    @Published private(set) var activeResponseID: UUID?
    @Published var error: String?
    @Published var activity = ""
    @Published var approval: ChatApproval?
    @Published var resolvingApproval = false
    private var task: Task<Void, Never>?
    private var turn: UInt64 = 0
    private var loadedPort: UInt32?
    init(session: UInt64) { self.session = session }
    var portNumber: UInt32? { guard let p = UInt32(port), (1...65535).contains(p) else { return nil }; return p }
    var canSend: Bool { !streaming && !loading && !model.isEmpty && portNumber == loadedPort && !prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
    func discoverIfNeeded() async {
        guard loadedPort == nil, !loading else { return }
        await discover()
    }
    func discover() async {
        guard !streaming, !loading, let port = portNumber else { return }
        let selectedModel = model
        loading = true; error = nil; models = []; model = ""; loadedPort = nil
        defer { loading = false }
        do {
            let result = try await GoClient.shared.chatModels(session: session, port: port)
            guard !Task.isCancelled, portNumber == port else { return }
            models = result; model = result.contains { $0.id == selectedModel } ? selectedModel : (result.first?.id ?? ""); loadedPort = port
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    func send() {
        guard canSend, let port = portNumber else { return }
        // Each request owns its history and cannot switch models mid-generation.
        let text = prompt.trimmingCharacters(in: .whitespacesAndNewlines)
        prompt = ""; error = nil; streaming = true
        messages.append(ChatMessage(isUser: true, text: text))
        let history = messages.filter { $0.isUser || !$0.text.isEmpty }
        messages.append(ChatMessage(isUser: false, text: ""))
        let index = messages.count - 1
        activeResponseID = messages[index].id
        turn = UInt64.random(in: 1...UInt64.max)
        let request = turn
        task = Task {
            defer { if turn == request { streaming = false; activeResponseID = nil; approval = nil; activity = "" } }
            do {
                guard !Task.isCancelled, turn == request else { return }
                try await GoClient.shared.startChat(session: session, turn: request, port: port, model: model, messages: history)
                // Stop may have been pressed while the start request was queued.
                guard !Task.isCancelled else { try? await GoClient.shared.stopChat(session: session, turn: request); return }
                while !Task.isCancelled {
                    let result = try await GoClient.shared.pollChat(session: session, turn: request)
                    guard turn == request, !Task.isCancelled else { return }
                    messages[index].text = result.text
                    messages[index].trace = result.trace
                    messages[index].cards = result.cards
                    activity = result.activity; approval = result.approval
                    if !result.error.isEmpty { error = result.error }
                    if result.done { return }
                    try await Task.sleep(for: .milliseconds(100))
                }
            } catch {
                if !Task.isCancelled && turn == request { self.error = error.localizedDescription }
            }
        }
    }
    func resolve(_ pending: ChatApproval, allow: Bool) async {
        guard approval?.id == pending.id, !resolvingApproval else { return }
        resolvingApproval = true
        defer { resolvingApproval = false }
        let request = turn
        do {
            try await GoClient.shared.resolveChatApproval(session: session, turn: request, approval: pending.id, allow: allow)
            if turn == request && approval?.id == pending.id { approval = nil }
        } catch { if turn == request { self.error = error.localizedDescription } }
    }
    func isWaiting(for message: ChatMessage) -> Bool {
        streaming && activeResponseID == message.id && message.text.isEmpty && message.cards.isEmpty && approval == nil
    }
    func stop() {
        task?.cancel(); task = nil
        let request = turn
        turn = 0
        if let index = messages.firstIndex(where: { $0.id == activeResponseID }) {
            messages[index].interrupted = true
        }
        streaming = false; activeResponseID = nil; approval = nil; activity = ""
        if request != 0 { Task { try? await GoClient.shared.stopChat(session: session, turn: request) } }
    }
    func clear() { stop(); messages = []; error = nil }
    func useModel(_ name: String, port: UInt32) {
        clear(); self.port = String(port); loadedPort = port
        if !models.contains(where: { $0.id == name }) { models.append(ChatModelOption(id: name, name: name)) }
        model = name
    }
}

struct DeviceChatView: View {
    let deviceName: String
    @ObservedObject var chat: DeviceChatModel
    @State private var showingLibrary = false
    @State private var showingErrorDetails = false
    @State private var showingConnection = false
    private var modelName: String { chat.models.first { $0.id == chat.model }?.displayName ?? ChatModelOption.readableName(chat.model) }
    var body: some View {
        VStack(spacing: 0) {
            #if os(iOS)
            HStack(spacing: 12) {
                Menu {
                    Picker("Model", selection: $chat.model) {
                        Text("Choose a model").tag("")
                        ForEach(chat.models) { Text($0.displayName).tag($0.id) }
                    }
                    Divider()
                    Button("Browse models", systemImage: "square.stack.3d.up") { showingLibrary = true }
                    Button("Connection settings", systemImage: "network") { showingConnection = true }
                    Button("Refresh models", systemImage: "arrow.clockwise") { Task { await chat.discover() } }
                } label: {
                    HStack(spacing: 6) {
                        Text(chat.model.isEmpty ? "Choose a model" : modelName).font(.subheadline.weight(.semibold)).lineLimit(1).truncationMode(.tail)
                        Image(systemName: "chevron.down").font(.caption2)
                    }.frame(maxWidth: .infinity, minHeight: 44, alignment: .leading).contentShape(Rectangle())
                }.disabled(chat.streaming || chat.loading)
                if chat.loading { ProgressView().controlSize(.small) }
                Button { chat.clear() } label: { Image(systemName: "square.and.pencil").frame(width: 44, height: 44) }
                    .accessibilityLabel("New chat")
            }.padding(.horizontal, 16)
            #else
            HStack(spacing: 12) {
                Label("Wendy Chat", systemImage: "bubble.left.and.bubble.right").font(.headline)
                Spacer()
                Picker("Model", selection: $chat.model) {
                    Text(chat.models.isEmpty ? "Find a model" : "Choose a model").tag("")
                    ForEach(chat.models) { Text($0.displayName).tag($0.id) }
                }.frame(maxWidth: 260).disabled(chat.streaming || chat.loading)
                Button("Browse models") { showingLibrary = true }.disabled(chat.streaming)
                Button { chat.clear() } label: { Label("New chat", systemImage: "square.and.pencil") }.help("New chat")
                Menu {
                    Button("Connection settings") { showingConnection = true }
                    Button("Refresh models") { Task { await chat.discover() } }
                } label: { Image(systemName: "ellipsis") }
                    .menuStyle(.borderlessButton).fixedSize().help("Chat options").disabled(chat.streaming)
            }.padding(.horizontal, 20).padding(.bottom, 8)
            #endif
            #if os(macOS)
            Text("Runs on \(deviceName) · Wendy skills and MCP tools")
                .font(.caption).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 28)
            if chat.loading { ProgressView("Finding models").padding() }
            #endif
            if let error = chat.error {
                HStack(spacing: 8) {
                    Image(systemName: "exclamationmark.circle").foregroundStyle(.orange)
                    Text(chat.messages.isEmpty ? "Could not load models." : "Chat could not complete the request.").font(.caption).foregroundStyle(.secondary)
                    Button("Details") { showingErrorDetails = true }.buttonStyle(.borderless).font(.caption)
                        .popover(isPresented: $showingErrorDetails) {
                            VStack(alignment: .leading, spacing: 12) {
                                HStack { Text("Chat error").font(.headline); Spacer(); Button("Done") { showingErrorDetails = false } }
                                ScrollView { Text(error).font(.callout).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading).fixedSize(horizontal: false, vertical: true) }
                            }.padding(16).desktopFrame(width: 500, height: 220)
                        }
                    Spacer()
                }.padding(.horizontal, 24).padding(.top, 8)
            }
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 22) {
                        ForEach(chat.messages) { message in
                            VStack(alignment: .leading, spacing: 8) {
                                Label(message.isUser ? "You" : modelName, systemImage: message.isUser ? "person.crop.circle" : "sparkles")
                                    .font(.caption.weight(.medium)).foregroundStyle(.secondary)
                                if !message.trace.isEmpty {
                                    DisclosureGroup("Device activity") { Text(message.trace).font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }
                                }
                                if chat.isWaiting(for: message) { ProgressView().controlSize(.small) }
                                else if message.isUser { Text(message.text).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading) }
                                else { ChatMarkdown(source: message.text) }
                                if message.interrupted {
                                    Text("Response stopped").font(.caption).foregroundStyle(.secondary)
                                }
                                ChatDeviceCardsView(cards: message.cards, session: chat.session, deviceName: deviceName)
                            }.padding(16).background(message.isUser ? Color.accentColor.opacity(0.06) : .clear, in: RoundedRectangle(cornerRadius: 12))
                        }
                        if let pending = chat.approval {
                            ChatApprovalView(approval: pending, deviceName: deviceName) { allow in
                                Task { await chat.resolve(pending, allow: allow) }
                            }
                            .disabled(chat.resolvingApproval)
                            .padding(.horizontal, 16)
                        }
                        Color.clear.frame(height: 1).id("bottom")
                    }.padding(.vertical, 20).padding(.horizontal, 12).frame(maxWidth: 900).frame(maxWidth: .infinity)
                }
                .onChange(of: chat.messages.last?.text) { _, _ in proxy.scrollTo("bottom", anchor: .bottom) }
                .onChange(of: chat.approval?.id) { _, id in
                    if id != nil { withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
                }
            }
            #if os(macOS)
            if chat.messages.isEmpty && !chat.loading {
                Text(chat.model.isEmpty ? "Choose a model to begin." : "Ask about this device, its apps, or recent logs.")
                    .font(.caption).foregroundStyle(.tertiary).padding(.horizontal, 26)
                    .frame(maxWidth: 900, alignment: .leading).padding(.bottom, 2)
            }
            #endif
            if chat.approval == nil && chat.streaming && !chat.activity.isEmpty {
                Text(chat.activity).font(.caption).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 28)
            }
            HStack(alignment: .bottom, spacing: 10) {
                TextField("Message", text: $chat.prompt, axis: .vertical)
                    .lineLimit(1...6).textFieldStyle(.plain)
                    .padding(.vertical, 9).padding(.leading, 6)
                    .frame(maxWidth: .infinity, minHeight: 36, alignment: .leading)
                    .onSubmit { chat.send() }
                Button {
                    if chat.streaming { chat.stop() } else { chat.send() }
                } label: {
                    Image(systemName: chat.streaming ? "stop.fill" : "arrow.up")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(chat.streaming || chat.canSend ? Color.white : Color.secondary)
                        .frame(width: 36, height: 36)
                        .background(chat.streaming || chat.canSend ? Color.accentColor : Color.secondary.opacity(0.12), in: Circle())
                        .contentShape(Circle())
                }
                .buttonStyle(.plain).disabled(!chat.streaming && !chat.canSend)
                .help(chat.streaming ? "Stop response" : "Send message")
                .accessibilityLabel(chat.streaming ? "Stop response" : "Send message")
            }
            .padding(8)
            .background(PlatformColors.control, in: RoundedRectangle(cornerRadius: 18))
            .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(PlatformColors.separator.opacity(0.5)))
            .frame(maxWidth: 900).padding(.horizontal, 20).padding(.bottom, 12).padding(.top, 6)
        }.task { await chat.discoverIfNeeded() }
            .sheet(isPresented: $showingLibrary) {
                ModelLibraryView(session: chat.session, deviceName: deviceName, port: chat.port) { model, port in chat.useModel(model, port: port) }
            }
            #if os(iOS)
            .sheet(isPresented: $showingConnection) {
                NavigationStack {
                    Form {
                        Section("Inference server") {
                            LabeledContent("Device", value: deviceName)
                            TextField("Port", text: $chat.port).keyboardType(.numberPad)
                            Button("Find models") { Task { await chat.discover(); if chat.error == nil { showingConnection = false } } }
                                .disabled(chat.streaming || chat.loading || chat.portNumber == nil)
                        }
                        if !chat.model.isEmpty { Section("Model ID") { Text(chat.model).font(.caption.monospaced()).textSelection(.enabled) } }
                        Section { Text("Wendy skills and MCP tools are available in this chat.").foregroundStyle(.secondary) }
                    }
                    .navigationTitle("Connection").navigationBarTitleDisplayMode(.inline)
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Close") { showingConnection = false } } }
                }.presentationDetents([.medium, .large]).presentationBackground(Color(uiColor: .systemGroupedBackground))
            }
            #else
            .popover(isPresented: $showingConnection) {
                VStack(alignment: .leading, spacing: 12) {
                    Text("Connection settings").font(.headline)
                    LabeledContent("Device", value: deviceName)
                    LabeledContent("Server port") { TextField("11434", text: $chat.port).frame(width: 90) }
                    Button("Find models") { Task { await chat.discover(); if chat.error == nil { showingConnection = false } } }
                        .disabled(chat.streaming || chat.loading || chat.portNumber == nil)
                }.padding(20).frame(width: 320)
            }
            #endif
            .focusedSceneValue(\.devicePaneRefresh, PaneRefreshAction(enabled: !chat.loading && !chat.streaming, perform: { Task { await chat.discover() } }))
    }
}

// Block layout stays native SwiftUI; Foundation parses inline emphasis, code,
// and links. Fenced code remains literal, including during partial streaming.
struct ChatMarkdownBlock: Equatable {
    enum Kind: Equatable { case paragraph, heading(Int), item(String, Int), code, quote, rule, table }
    let kind: Kind
    let text: String

    static func parse(_ source: String) -> [Self] {
        let lines = source.components(separatedBy: "\n")
        var blocks: [Self] = []
        var paragraph: [String] = []
        var index = 0
        func flush() {
            if !paragraph.isEmpty { blocks.append(Self(kind: .paragraph, text: paragraph.joined(separator: "\n"))); paragraph = [] }
        }
        while index < lines.count {
            let line = lines[index]
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            index += 1
            if trimmed.isEmpty { flush(); continue }
            if trimmed.hasPrefix("```") || trimmed.hasPrefix("~~~") {
                flush()
                let fence = String(trimmed.prefix(while: { $0 == trimmed.first! }))
                var code: [String] = []
                while index < lines.count {
                    let next = lines[index]; index += 1
                    if next.trimmingCharacters(in: .whitespaces).hasPrefix(fence) { break }
                    code.append(next)
                }
                blocks.append(Self(kind: .code, text: code.joined(separator: "\n")))
                continue
            }
            let heading = trimmed.prefix(while: { $0 == "#" }).count
            if (1...6).contains(heading), trimmed.dropFirst(heading).first == " " {
                flush(); blocks.append(Self(kind: .heading(heading), text: String(trimmed.dropFirst(heading + 1)))); continue
            }
            if ["---", "***", "___"].contains(trimmed) { flush(); blocks.append(Self(kind: .rule, text: "")); continue }
            if trimmed.hasPrefix(">") {
                flush(); blocks.append(Self(kind: .quote, text: String(trimmed.dropFirst()).trimmingCharacters(in: .whitespaces))); continue
            }
            if index < lines.count, trimmed.contains("|"), isTableDivider(lines[index]) {
                flush(); index += 1
                var rows = [trimmed]
                while index < lines.count, lines[index].contains("|"), !lines[index].trimmingCharacters(in: .whitespaces).isEmpty {
                    rows.append(lines[index]); index += 1
                }
                blocks.append(Self(kind: .table, text: rows.joined(separator: "\n"))); continue
            }
            var marker: String?
            var body = ""
            if ["- ", "* ", "+ "].contains(where: { trimmed.hasPrefix($0) }) {
                marker = "•"; body = String(trimmed.dropFirst(2))
                if body.hasPrefix("[ ] ") { marker = "☐"; body = String(body.dropFirst(4)) }
                else if body.lowercased().hasPrefix("[x] ") { marker = "☑"; body = String(body.dropFirst(4)) }
            } else {
                let digits = trimmed.prefix(while: { $0.isNumber })
                let rest = trimmed.dropFirst(digits.count)
                if !digits.isEmpty, rest.hasPrefix(". ") || rest.hasPrefix(") ") {
                    marker = String(digits) + "."; body = String(rest.dropFirst(2))
                }
            }
            if let marker {
                flush()
                blocks.append(Self(kind: .item(marker, min(6, (line.count - trimmed.count) / 2)), text: body))
            } else { paragraph.append(line) }
        }
        flush()
        return blocks
    }

    static func tableCells(_ row: String) -> [String] {
        var text = row.trimmingCharacters(in: .whitespaces)
        if text.hasPrefix("|") { text.removeFirst() }
        if text.hasSuffix("|") { text.removeLast() }
        return text.components(separatedBy: "|").map { $0.trimmingCharacters(in: .whitespaces) }
    }
    private static func isTableDivider(_ row: String) -> Bool {
        let cells = tableCells(row)
        return !cells.isEmpty && cells.allSatisfy { cell in
            let dashes = cell.trimmingCharacters(in: CharacterSet(charactersIn: ":"))
            return dashes.count >= 3 && dashes.allSatisfy { $0 == "-" }
        }
    }
}

struct ChatMarkdown: View {
    let source: String
    private func inline(_ text: String) -> Text {
        Text((try? AttributedString(markdown: text, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))) ?? AttributedString(text))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(ChatMarkdownBlock.parse(source).enumerated()), id: \.offset) { _, block in
                switch block.kind {
                case .paragraph:
                    inline(block.text).fixedSize(horizontal: false, vertical: true)
                case .heading(let level):
                    inline(block.text).font(level <= 2 ? .title3 : .headline).fontWeight(.semibold).padding(.top, 4)
                case .item(let marker, let depth):
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        Text(marker).monospacedDigit().frame(minWidth: 16, alignment: .trailing)
                        inline(block.text).fixedSize(horizontal: false, vertical: true)
                    }.padding(.leading, CGFloat(depth) * 16)
                case .code:
                    ScrollView(.horizontal) {
                        Text(block.text).font(.system(.callout, design: .monospaced)).fixedSize(horizontal: true, vertical: true).padding(10)
                    }.background(Color.primary.opacity(0.045), in: RoundedRectangle(cornerRadius: 6))
                case .quote:
                    HStack(alignment: .top, spacing: 10) {
                        Rectangle().fill(.tertiary).frame(width: 3)
                        inline(block.text).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }.fixedSize(horizontal: false, vertical: true)
                case .rule: Divider()
                case .table:
                    let rows = block.text.components(separatedBy: "\n").map(ChatMarkdownBlock.tableCells)
                    ScrollView(.horizontal) {
                        Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 8) {
                            ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                                GridRow {
                                    ForEach(Array(row.enumerated()), id: \.offset) { _, cell in
                                        inline(cell).fontWeight(index == 0 ? .semibold : .regular)
                                    }
                                }
                                if index == 0 { Divider().gridCellUnsizedAxes(.horizontal) }
                            }
                        }.padding(10)
                    }.background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 6))
                }
            }
        }
        .textSelection(.enabled)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
