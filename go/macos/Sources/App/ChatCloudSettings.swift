import SwiftUI
import Security

enum ChatInference: String, CaseIterable { case device, cloud
    var title: String { self == .device ? "On device" : "Cloud" }
}
enum ChatCloudProvider: String, CaseIterable, Identifiable {
    case openai, anthropic
    var id: String { rawValue }
    var title: String { self == .openai ? "OpenAI" : "Anthropic" }
}

/// Credentials stay in this app's non-synchronizing Keychain, never preferences
/// or the device. Read only when a user asks to discover models or send a turn.
@MainActor protocol ChatCredentialStore {
    func read(_ provider: ChatCloudProvider) throws -> String?
    func save(_ key: String, for provider: ChatCloudProvider) throws
    func remove(_ provider: ChatCloudProvider) throws
}
struct ChatKeychain: ChatCredentialStore {
    var service = "dev.wendy.ask-wendy.api-key"
    private func query(_ provider: ChatCloudProvider) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: provider.rawValue,
         kSecAttrSynchronizable as String: false]
    }
    func read(_ provider: ChatCloudProvider) throws -> String? {
        var request = query(provider)
        request[kSecReturnData as String] = true
        request[kSecMatchLimit as String] = kSecMatchLimitOne
        var value: CFTypeRef?
        let status = SecItemCopyMatching(request as CFDictionary, &value)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = value as? Data, let key = String(data: data, encoding: .utf8) else {
            throw WendyError(message: "Could not read the API key from Keychain (\(status)).")
        }
        return key
    }
    func save(_ key: String, for provider: ChatCloudProvider) throws {
        let value = key.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty, value.utf8.count <= 8192, !value.contains(where: { $0.isNewline }) else {
            throw WendyError(message: "Enter a valid API key.")
        }
        let attributes: [String: Any] = [kSecValueData as String: Data(value.utf8),
                                       kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly]
        var status = SecItemUpdate(query(provider) as CFDictionary, attributes as CFDictionary)
        if status == errSecItemNotFound {
            var item = query(provider); item.merge(attributes) { _, new in new }
            status = SecItemAdd(item as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw WendyError(message: "Could not save the API key in Keychain (\(status)).") }
    }
    func remove(_ provider: ChatCloudProvider) throws {
        let status = SecItemDelete(query(provider) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw WendyError(message: "Could not remove the API key from Keychain (\(status)).") }
    }
}

struct ChatCloudSettings: View {
    @ObservedObject var chat: DeviceChatModel
    @Environment(\.dismiss) private var dismiss
    @State private var apiKey = ""
    @State private var actions = AsyncActions()
    var body: some View {
        NavigationStack {
            Form {
                Section("Provider") {
                    Picker("Cloud provider", selection: Binding(get: { chat.cloudProvider }, set: { chat.selectProvider($0); apiKey = "" })) {
                        ForEach(ChatCloudProvider.allCases) { Text($0.title).tag($0) }
                    }.disabled(chat.loading || chat.streaming)
                }
                Section("Your API key") {
                    Text(chat.hasCloudKey ? "An API key is saved securely in Keychain." : "Add your own provider API key to find available models.")
                        .font(.callout).foregroundStyle(.secondary)
                    SecureField(chat.hasCloudKey ? "Replacement API key" : "API key", text: $apiKey)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                    Button(chat.hasCloudKey ? "Replace key & find models" : "Save key & find models") {
                        let key = apiKey; apiKey = ""
                        actions.submit { if chat.saveCloudKey(key) { await chat.discover() } }
                    }.disabled(apiKey.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || chat.loading || chat.streaming)
                    if chat.hasCloudKey {
                        Button("Remove API key", role: .destructive) { chat.removeCloudKey(); apiKey = "" }
                            .disabled(chat.loading || chat.streaming)
                    }
                }
                Section("Model") {
                    if chat.loading { ProgressView("Finding models…") }
                    Picker("Model", selection: $chat.model) {
                        Text("Choose a model").tag("")
                        ForEach(chat.models) { Text($0.displayName).tag($0.id) }
                    }.disabled(chat.loading || chat.streaming)
                    Button("Refresh models") { actions.submit { await chat.discover() } }
                        .disabled(!chat.hasCloudKey || chat.loading || chat.streaming)
                    if let error = chat.error { Text(error).font(.callout).foregroundStyle(.red) }
                }
                Section("Cloud inference") {
                    Text("Your messages and device information used to answer them—including requested logs and tool results—are sent to \(chat.cloudProvider.title). Device changes still require confirmation.")
                    Text("API usage is billed by your provider. A chat subscription is not automatically API access. Keys stay in this app’s Keychain and are not sent to your Wendy device. Switching inference source starts a new chat.")
                        .foregroundStyle(.secondary)
                }
            }
            #if os(iOS)
            .scrollContentBackground(.hidden).background(MobileLensStyle.background)
            #endif
            .navigationTitle("Cloud models")
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
        }
        #if os(iOS)
        .tint(MobileLensStyle.green)
        #endif
        .frame(idealWidth: 520, idealHeight: 620)
        .task { chat.refreshCloudKey(); await actions.run() }
        .onDisappear { apiKey = "" }
    }
}

extension GoClient {
    func hostedChatModels(provider: ChatCloudProvider, key: String) async throws -> [ChatModelOption] {
        try await performModelRequest {
            let strings = CStringStorage(); defer { withExtendedLifetime(strings) {} }
            guard let p = WDHostedChatModels(strings.add(provider.rawValue), strings.add(key)) else { throw WendyError(message: "Missing cloud model list.") }
            defer { WDChatModelsFree(p) }; try Self.check(p.pointee.status)
            return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { ChatModelOption(id: Self.string($0.id), name: Self.string($0.name), port: 0) }
        }
    }
    func startHostedChat(session: UInt64, turn: UInt64, provider: ChatCloudProvider, model: String, key: String, messages: [ChatMessage]) async throws {
        try await perform {
            let strings = CStringStorage(); defer { withExtendedLifetime(strings) {} }
            var input = messages.map { WDChatMessage(role: $0.isUser ? WD_CHAT_USER : WD_CHAT_ASSISTANT, text: strings.add($0.text)) }
            try input.withUnsafeMutableBufferPointer {
                try Self.finish(WDStartHostedChat(session, turn, strings.add(provider.rawValue), strings.add(model), strings.add(key), $0.baseAddress, $0.count))
            }
        }
    }
}
