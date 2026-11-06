import SwiftUI

struct SpatialAssistantView: View {
    let model: SpatialModel
    @Bindable var assistant: SpatialAssistant
    @State private var actions = AsyncActions()
    @Environment(\.scenePhase) private var phase
    @FocusState private var composing: Bool

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 14) {
                Image(systemName: "waveform").font(.system(size: 24, weight: .light)).foregroundStyle(SpatialStyle.amber)
                    .frame(width: 48, height: 48).background(SpatialStyle.amber.opacity(0.12), in: RoundedRectangle(cornerRadius: 17))
                VStack(alignment: .leading, spacing: 3) {
                    Text("Ask Wendy").font(.system(size: 24, weight: .medium, design: .rounded))
                    Text(model.connected?.name ?? "Your spatial assistant").font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer()
                if assistant.streaming { ProgressView().controlSize(.small) }
                Menu {
                    if !assistant.models.isEmpty {
                        Picker("Model", selection: $assistant.selected) {
                            ForEach(assistant.models) { item in Text(item.name.isEmpty ? item.model : item.name).tag(item.id) }
                        }
                    }
                    Button("Refresh models", systemImage: "arrow.clockwise") { actions.submit { await assistant.discover() } }
                    Button("New conversation", systemImage: "square.and.pencil") { assistant.cancel(); assistant.messages = [] }
                } label: { Image(systemName: "ellipsis").frame(width: 44, height: 44) }.disabled(assistant.streaming)
            }.padding(24)
            Divider().opacity(0.4)
            ScrollViewReader { reader in
                ScrollView {
                    VStack(alignment: .leading, spacing: 24) {
                        if assistant.messages.isEmpty { introduction }
                        ForEach(assistant.messages) { message in
                            VStack(alignment: .leading, spacing: 10) {
                                HStack {
                                    SpatialEyebrow(title: message.user ? "You" : "Wendy")
                                    Spacer()
                                    if !message.context.isEmpty {
                                        Label("Snapshot attached", systemImage: "cube.transparent").font(.caption2).foregroundStyle(.secondary)
                                    }
                                }
                                if message.text.isEmpty, !message.user {
                                    if assistant.streaming {
                                        HStack(spacing: 10) { ProgressView().controlSize(.small); Text("Reading the context…").foregroundStyle(.secondary) }
                                    } else { Text("No response received.").foregroundStyle(.secondary) }
                                } else {
                                    Text(.init(message.text)).font(.system(size: 16)).lineSpacing(5).textSelection(.enabled)
                                }
                            }.padding(18)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .background(message.user ? SpatialStyle.amber.opacity(0.08) : .white.opacity(0.035), in: RoundedRectangle(cornerRadius: 22))
                                .id(message.id)
                        }
                        if assistant.declinedAction {
                            Label("Device changes stay in the workspace controls. No requested change was approved by this chat.", systemImage: "hand.raised")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        if let error = assistant.error {
                            VStack(alignment: .leading, spacing: 10) {
                                Label(error, systemImage: "exclamationmark.circle").font(.subheadline).foregroundStyle(.orange)
                                Button("Retry model discovery") { actions.submit { await assistant.discover() } }.disabled(assistant.loading || assistant.streaming)
                            }.padding(18).background(.orange.opacity(0.06), in: RoundedRectangle(cornerRadius: 20))
                        }
                        Color.clear.frame(height: 1).id("bottom")
                    }.padding(24)
                }
                .onChange(of: assistant.messages.count) { _, _ in reader.scrollTo("bottom", anchor: .bottom) }
            }
            composer
        }
        .tint(SpatialStyle.amber)
        .frame(minWidth: 440, minHeight: 560)
        .task { await actions.run() }
        .task(id: model.session) { await assistant.selectSession(model.session) }
        .task(id: assistant.turn) { await assistant.respond() }
        .onChange(of: phase) { _, new in if new == .background { assistant.cancel() } }
    }
    private var introduction: some View {
        VStack(alignment: .leading, spacing: 24) {
            VStack(alignment: .leading, spacing: 12) {
                Text("Ask about\nwhat's here.").font(.system(size: 36, weight: .medium, design: .rounded)).tracking(-1)
                Text("Wendy can explain device state, robot observations, alignment, and a moment in replay.")
                    .foregroundStyle(.secondary).lineSpacing(4)
            }
            VStack(spacing: 12) {
                suggestion("What's happening with Go2?", symbol: "pawprint", prompt: "Explain the robot's current status, what is known, and what is missing.")
                suggestion("Why can't it move?", symbol: "point.topleft.down.to.point.bottomright.curvepath", prompt: "What is blocking robot movement in this snapshot? Explain the evidence and the next step without changing anything.")
                suggestion("Where am I in the twin?", symbol: "location", prompt: "Describe my position in the digital twin if alignment and tracking are valid. If not, explain the missing alignment step.")
            }
            if model.session == 0 {
                Label("Connect a gateway to use a model running on that device.", systemImage: "server.rack").font(.caption).foregroundStyle(.secondary)
            } else if assistant.loading {
                HStack { ProgressView().controlSize(.small); Text("Finding models on your device…").font(.caption).foregroundStyle(.secondary) }
            } else if assistant.models.isEmpty, assistant.error == nil {
                Text("No model is available on this device. Start a model in Wendy on Mac or iPhone, then refresh here.").font(.caption).foregroundStyle(.secondary)
            }
        }
    }
    private var composer: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Toggle(isOn: $assistant.includeContext) {
                    Label(model.replay == nil ? "Include spatial snapshot" : "Include this replay frame", systemImage: "cube.transparent").font(.caption)
                }.toggleStyle(.switch).controlSize(.mini)
            }
            if assistant.includeContext {
                Text("Shares status, alignment and your position in the twin when available. No camera images or audio.")
                    .font(.system(size: 11)).foregroundStyle(.secondary)
            }
            HStack(alignment: .bottom, spacing: 12) {
                TextField("Ask about your space…", text: $assistant.prompt, axis: .vertical)
                    .lineLimit(1...4).focused($composing).submitLabel(.send)
                    .onSubmit { send() }.padding(15)
                    .background(.white.opacity(0.06), in: RoundedRectangle(cornerRadius: 18))
                if assistant.streaming {
                    Button { assistant.cancel() } label: { Image(systemName: "stop.fill").frame(width: 48, height: 48) }
                        .buttonStyle(.bordered).accessibilityLabel("Stop response")
                } else {
                    Button { send() } label: { Image(systemName: "arrow.up").font(.headline).frame(width: 48, height: 48) }
                        .buttonStyle(.borderedProminent).disabled(!assistant.canSend || assistant.session != model.session)
                        .accessibilityLabel("Send question")
                }
            }
            if let selected = assistant.selectedModel {
                Text(selected.unavailable.isEmpty ? "Runs on \(model.connected?.name ?? "your device") · \(selected.name.isEmpty ? selected.model : selected.name)" : selected.unavailable)
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2)
            }
        }.padding(22).background(.white.opacity(0.025))
    }
    private func suggestion(_ title: String, symbol: String, prompt: String) -> some View {
        Button { assistant.prompt = prompt; composing = true } label: {
            HStack(spacing: 13) {
                Image(systemName: symbol).foregroundStyle(SpatialStyle.amber).frame(width: 26)
                Text(title).font(.subheadline)
                Spacer()
                Image(systemName: "arrow.up.left").font(.caption).foregroundStyle(.tertiary)
            }.padding(16).frame(maxWidth: .infinity).contentShape(RoundedRectangle(cornerRadius: 18))
        }.buttonStyle(.plain).hoverEffect(.highlight).background(.white.opacity(0.045), in: RoundedRectangle(cornerRadius: 18))
    }
    private func send() {
        guard assistant.session == model.session else { return }
        assistant.send(context: SpatialAIContext.capture(model))
        composing = false
    }
}
