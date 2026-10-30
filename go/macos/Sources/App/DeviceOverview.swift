import SwiftUI

struct DeviceOverview: View {
    @ObservedObject var model: AppModel
    let device: Device
    let open: (String) -> Void
    @Environment(\.scenePhase) private var scenePhase
    @State private var info: DeviceInfoSnapshot?
    @State private var actions = AsyncActions()
    private var running: [DeviceApp] { model.visibleApps.filter { $0.runningState == .running } }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 28) {
                HStack(spacing: 20) {
                    DeviceEmblem(lite: device.lite, size: 76)
                    VStack(alignment: .leading, spacing: 6) {
                        Text(device.title).font(.largeTitle.weight(.semibold)).textSelection(.enabled)
                        if let info, !info.board.isEmpty { Text(info.board).font(.subheadline).foregroundStyle(.secondary) }
                        Label("Connected", systemImage: "checkmark.circle.fill").font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer(minLength: 0)
                }
                if let app = running.first(where: \.hasWebpage) {
                    Button { actions.submit { await model.openApp(app) } } label: {
                        Label("Open \(app.appName)", systemImage: "arrow.up.right")
                            .font(.headline).padding(.vertical, 8).frame(maxWidth: .infinity)
                    }.buttonStyle(.borderedProminent).controlSize(.large)
                }
                if !model.cameraFeeds.isEmpty {
                    HStack {
                        Text("Live view").font(.title2.weight(.semibold))
                        Spacer()
                        Button("All cameras") { open("cameras") }.font(.subheadline)
                    }
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 280), spacing: 16)], spacing: 16) {
                        ForEach(model.cameraFeeds.prefix(4)) { feed in
                            Button { open("cameras") } label: { OverviewCamera(feed: feed) }.buttonStyle(.plain)
                        }
                    }
                }
                VStack(alignment: .leading, spacing: 16) {
                    HStack {
                        Text("On this device").font(.title2.weight(.semibold))
                        Spacer()
                        Button("All apps") { open("apps") }.font(.subheadline)
                    }
                    if model.appsError != nil {
                        Label("Updating app activity…", systemImage: "clock").foregroundStyle(.secondary)
                    } else if model.visibleApps.isEmpty {
                        Text("Your device is ready for its first app.").foregroundStyle(.secondary)
                    } else {
                        ForEach(model.visibleApps.prefix(5)) { app in
                            HStack(spacing: 12) {
                                Image(systemName: "square.stack.3d.up").foregroundStyle(.secondary)
                                Text(app.appName).font(.headline)
                                Spacer()
                                Text(app.state.capitalized).font(.caption).foregroundStyle(.secondary)
                                if app.runningState == .running && app.hasWebpage {
                                    Button("Open") { actions.submit { await model.openApp(app) } }.buttonStyle(.bordered)
                                }
                            }.padding(.vertical, 6)
                        }
                    }
                }
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 140), spacing: 12)], spacing: 12) {
                    destination("Cameras", symbol: "video", page: "cameras")
                    destination("Explore site", symbol: "cube.transparent", page: "siteModel")
                    destination("Ask your device", symbol: "bubble.left.and.bubble.right", page: "chat")
                    #if os(macOS)
                    destination("Apps & activity", symbol: "waveform.path", page: "apps")
                    destination("Recorded data", symbol: "clock.arrow.circlepath", page: "data")
                    #endif
                }
            }.padding(24).frame(maxWidth: 1100).frame(maxWidth: .infinity)
        }
        .task { await actions.run() }
        .task(id: model.deviceSession) {
            info = nil
            info = try? await GoClient.shared.deviceInfo(session: model.deviceSession)
        }
        .task(id: scenePhase) {
            if scenePhase == .active { await model.showCameraWall(limit: 4) }
            else { model.hideCameraWall() }
        }
        .onDisappear { model.hideCameraWall() }
    }

    private func destination(_ title: String, symbol: String, page: String) -> some View {
        Button { open(page) } label: {
            VStack(alignment: .leading, spacing: 14) {
                Image(systemName: symbol).font(.title2).foregroundStyle(.tint)
                Text(title).font(.subheadline.weight(.medium))
            }.frame(maxWidth: .infinity, minHeight: 80, alignment: .leading)
                .padding(18).background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 22))
        }.buttonStyle(.plain)
    }
}

private struct OverviewCamera: View {
    @ObservedObject var feed: CameraFeed
    var body: some View {
        CameraFeedDisplay(feed: feed)
            .aspectRatio(4 / 3, contentMode: .fit).background(.black)
            .overlay(alignment: .bottomLeading) {
                VStack(alignment: .leading, spacing: 5) {
                    Text(feed.camera.name).font(.headline)
                    Text(feed.receivedVideo && feed.running ? "Live" : feed.manuallyPaused ? "Paused" : "Connecting…")
                        .font(.caption)
                }.foregroundStyle(.white).padding(16).frame(maxWidth: .infinity, alignment: .leading)
                    .background(LinearGradient(colors: [.clear, .black.opacity(0.8)], startPoint: .top, endPoint: .bottom))
            }
            .clipShape(RoundedRectangle(cornerRadius: 24))
            .accessibilityElement(children: .combine)
    }
}

struct ExperienceEvent: Identifiable {
    let id = UUID()
    let deviceID, deviceName, title, symbol: String
    let date = Date()
}

struct ExperienceActivityView: View {
    @ObservedObject var model: AppModel
    let select: (Device) -> Void
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 24) {
                Text("Activity").font(.largeTitle.weight(.semibold))
                Text("Connections and app changes observed by Wendy.").foregroundStyle(.secondary)
                if model.experienceEvents.isEmpty {
                    ContentUnavailableView("You’re up to date", systemImage: "checkmark.circle", description: Text("Activity will appear as you use your devices."))
                }
                ForEach(model.experienceEvents) { event in
                    Button {
                        if let device = (model.nearby + model.cloud + [model.connected].compactMap { $0 }).first(where: { $0.id == event.deviceID }) { select(device) }
                    } label: {
                        HStack(alignment: .top, spacing: 16) {
                            Image(systemName: event.symbol).font(.title3).foregroundStyle(.tint).frame(width: 28)
                            VStack(alignment: .leading, spacing: 5) {
                                Text(event.title).font(.headline)
                                Text(event.deviceName).font(.subheadline).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Text(event.date, style: .relative).font(.caption).foregroundStyle(.secondary)
                        }.padding(.vertical, 10).frame(maxWidth: .infinity, alignment: .leading)
                    }.buttonStyle(.plain)
                }
            }.padding(24).frame(maxWidth: 800).frame(maxWidth: .infinity)
        }
    }
}
