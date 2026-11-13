import SwiftUI

struct ChatDeviceCardsView: View {
    let cards: [ChatDeviceCard]
    let session: UInt64
    let deviceName: String
    private var cameras: [ChatDeviceCard] { cards.filter { $0.kind == "camera" } }
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if cameras.count == 1, let camera = cameras.first {
                ChatDeviceCardView(card: camera, session: session, deviceName: deviceName)
                    .frame(maxWidth: 480)
            } else if !cameras.isEmpty {
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 240, maximum: 400), spacing: 12)], alignment: .leading, spacing: 12) {
                    ForEach(cameras) { card in ChatDeviceCardView(card: card, session: session, deviceName: deviceName) }
                }.frame(maxWidth: 840, alignment: .leading)
            }
            ForEach(cards.filter { $0.kind != "camera" }) { card in
                ChatDeviceCardView(card: card, session: session, deviceName: deviceName)
            }
        }.frame(maxWidth: .infinity, alignment: .leading)
    }
}

struct ChatDeviceCardView: View {
    @EnvironmentObject private var model: AppModel
    let card: ChatDeviceCard
    let session: UInt64
    let deviceName: String
    @State private var resources: ResourceSnapshot?
    @State private var info: DeviceInfoSnapshot?
    @State private var error: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if card.kind == "camera" {
                if let feed = model.cameraFeeds.first(where: { $0.camera.id == card.camera && $0.camera.name == card.title }) {
                    ChatCameraCard(feed: feed, model: model)
                } else {
                    Label(card.title, systemImage: "video.slash")
                    Text(error ?? "Camera is no longer available.").font(.caption).foregroundStyle(.secondary)
                }
            } else if card.kind == "notifications" {
                Label(card.title, systemImage: "bell.badge").font(.headline)
                NotificationReceivingView()
            } else {
                Label(card.title, systemImage: card.kind == "resources" ? "gauge.with.dots.needle.50percent" : "info.circle").font(.headline)
                if let error { FailureNote(message: error) }
                else if card.kind == "resources", let resources {
                    if resources.hasHost {
                        LabeledContent("CPU", value: resources.cpu.map { String(format: "%.1f%%", $0) } ?? "Unavailable")
                        LabeledContent("Memory", value: bytes(resources.memoryUsed) + " / " + bytes(resources.memoryTotal))
                    }
                    if resources.hasDisk { LabeledContent("Storage", value: bytes(resources.diskUsed) + " / " + bytes(resources.diskTotal)) }
                    ForEach(resources.gpus) { gpu in LabeledContent(gpu.name, value: String(format: "%.1f%%", gpu.utilization)) }
                    Text("Live device readings").font(.caption).foregroundStyle(.secondary)
                } else if let info {
                    if !info.board.isEmpty { LabeledContent("Model", value: info.board) }
                    LabeledContent("Agent", value: info.version)
                    LabeledContent("OS", value: info.os + " " + info.osVersion)
                    LabeledContent("Architecture", value: info.architecture)
                    NavigationLink("Device info") { DeviceInfoView(session: session, deviceName: deviceName) }
                } else { ProgressView() }
            }
        }.padding(card.kind == "camera" ? 0 : 14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(PlatformColors.control, in: RoundedRectangle(cornerRadius: 14))
            .task(id: card.id) {
                guard model.deviceSession == session else { return }
                if card.kind == "camera" {
                    if !model.cameraFeeds.contains(where: { $0.camera.id == card.camera }) { await model.refreshDetails() }
                } else if card.kind == "resources" {
                    while !Task.isCancelled && model.deviceSession == session {
                        do { let result = try await model.resources(); if !Task.isCancelled { resources = result; error = nil } }
                        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
                        do { try await Task.sleep(for: .seconds(3)) } catch { return }
                    }
                } else if card.kind == "device_info" {
                    do { info = try await GoClient.shared.deviceInfo(session: session) }
                    catch { self.error = error.localizedDescription }
                }
            }
    }
    private func bytes(_ value: Int64) -> String { ByteCountFormatter.string(fromByteCount: value, countStyle: .memory) }
}
private struct ChatCameraCard: View {
    @State private var actions = AsyncActions()
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var manuallyPaused = false
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        Color.black
            .aspectRatio(4.0 / 3.0, contentMode: .fit)
            .overlay { CameraDisplayView(decoder: feed.decoder) }
            .overlay(alignment: .bottom) {
                HStack(alignment: .bottom) {
                    VStack(alignment: .leading, spacing: 5) {
                        Text(feed.camera.name).font(.subheadline.weight(.semibold)).lineLimit(2)
                        if let error = feed.error { Text(PresentedFailure(error).message).font(.caption) }
                        else if !feed.receivedVideo { Text(feed.running ? "Connecting…" : "Tap play for live video").font(.caption) }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                    Button {
                        if feed.running { manuallyPaused = true; model.pauseCamera(feed) }
                        else { manuallyPaused = false; actions.submit { await model.playCamera(feed, inline: true) } }
                    } label: {
                        Image(systemName: feed.running ? "pause.fill" : "play.fill").frame(width: 44, height: 44).background(.black.opacity(0.5), in: Circle())
                    }.buttonStyle(.plain).accessibilityLabel(feed.running ? "Pause camera" : "Play camera")
                }.foregroundStyle(.white).padding(12).padding(.top, 24)
                    .background(LinearGradient(colors: [.clear, .black.opacity(0.85)], startPoint: .top, endPoint: .bottom))
            }.clipShape(RoundedRectangle(cornerRadius: 14))
            .task {
                if !manuallyPaused && !feed.running && scenePhase == .active {
                    await model.playCamera(feed, inline: true)
                }
            }
            .onDisappear { model.pauseCamera(feed) }
            .onChange(of: scenePhase) { _, phase in
                if phase != .active { model.pauseCamera(feed) }
                else if !manuallyPaused && !feed.running {
                    actions.submit { await model.playCamera(feed, inline: true) }
                }
            }
    }
}
