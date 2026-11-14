import SwiftUI

struct MobileRecordingSection: View {
    @ObservedObject var model: AppModel
    var recent = false
    @State private var episodes: [DataVisualEpisode] = []
    @State private var loaded = false
    @State private var error: String?
    private var owner: UInt64? {
        guard model.mobileScopeIsCurrent, model.selectedDeviceReady, !model.connectedLite else { return nil }
        return model.deviceSession
    }

    var body: some View {
        LensSection(title: recent ? "Recent data" : "Recordings") {
            if let session = owner, let device = model.connected {
                if let error {
                    FailureNote(message: error).padding(16)
                } else if !loaded {
                    MobileLoadingPlaceholder(label: "Loading recordings…", style: .recordings, rows: 2).padding(16)
                } else if episodes.isEmpty {
                    Text("No recordings on \(device.title).").font(.subheadline).foregroundStyle(.secondary).lineLimit(3).truncationMode(.middle).padding(16)
                } else {
                    let visible = recent ? Array(episodes.prefix(3)) : episodes
                    ForEach(visible) { episode in
                        NavigationLink {
                            if owner == session {
                                DataVisualizerView(session: session, deviceName: device.title, initialEpisode: episode.id)
                                    .navigationTitle(episode.title).navigationBarTitleDisplayMode(.inline)
                            } else { ContentUnavailableView("Device changed", systemImage: "bolt.slash") }
                        } label: {
                            LensRow(title: episode.title, subtitle: device.title, symbol: "cylinder.split.1x2", color: .gray,
                                    trailing: episode.started.formatted(date: .abbreviated, time: .omitted))
                        }.buttonStyle(.plain)
                        if episode.id != visible.last?.id { Divider().padding(.leading, 64) }
                    }
                }
            } else {
                Text("Connect a device to see its recordings.").font(.subheadline).foregroundStyle(.secondary).padding(16)
            }
        }.task(id: owner) {
            episodes = []; loaded = false; error = nil
            guard let session = owner else { return }
            do {
                let rows = try await GoClient.shared.visualEpisodes(session: session)
                guard !Task.isCancelled, owner == session else { return }
                episodes = rows.sorted { $0.started > $1.started }; loaded = true
            } catch {
                if !Task.isCancelled, owner == session { self.error = error.localizedDescription }
            }
        }
    }
}

struct MobileHomeAttention: View {
    @ObservedObject var model: AppModel
    let openDevices: () -> Void
    var body: some View {
        LensSection(title: "Needs attention") {
            if let issue = model.nearbyErrorFailure ?? model.cloudErrorFailure ?? model.mobileCloudSelectionError.map({ WendyError(message: $0) }) {
                FailureNote(message: issue).padding(16)
            } else if model.mobileScopeIsCurrent, model.selectedDeviceReady,
                      let app = model.apps.first(where: { $0.runningState == .crashLooping }), let device = model.connected {
                NavigationLink(value: MobileDestination.device(device)) {
                    LensRow(title: app.appName, subtitle: "Crash looping · \(device.title)", symbol: "exclamationmark.triangle", color: .orange)
                }.buttonStyle(.plain)
            } else if let device = model.failedConnection, let error = model.detailError {
                NavigationLink(value: MobileDestination.device(device)) {
                    LensRow(title: device.title, subtitle: PresentedFailure(error).message, symbol: "wifi.exclamationmark", color: .orange)
                }.buttonStyle(.plain)
            } else {
                if let snapshot = model.mobilePresence, model.mobileScopeIsCurrent, model.cloudError == nil,
                   snapshot.0.presenceError.isEmpty, Date().timeIntervalSince(snapshot.1) < 60,
                   let offline = snapshot.0.devices.first(where: { !$0.online }) {
                    NavigationLink(value: MobileDestination.device(offline.device)) {
                        LensRow(title: offline.name, subtitle: "Offline in Wendy Cloud", symbol: "wifi.slash", color: .gray)
                    }.buttonStyle(.plain)
                } else {
                    Button(action: openDevices) {
                        LensRow(title: "No reported issues", subtitle: "View device activity", symbol: "checkmark.circle")
                    }.buttonStyle(.plain)
                }
            }
        }
    }
}

struct MobileMeshSection: View {
    @ObservedObject var model: AppModel
    @State private var topology: DeviceMeshTopology?
    @State private var error: String?
    private var session: UInt64? { model.mobileScopeIsCurrent && model.selectedDeviceReady && !model.connectedLite ? model.deviceSession : nil }
    var body: some View {
        Section("Mesh network") {
            if let topology, let device = model.connected { MeshGraphView(topology: topology, deviceName: device.title) }
            else if let error { FailureNote(message: error) }
            else if session != nil { ProgressView("Loading mesh…") }
            else { Text("Open a nearby device to see its mesh peers.").foregroundStyle(.secondary) }
        }.task(id: session) {
            topology = nil; error = nil
            guard let session else { return }
            do {
                let settings = try await GoClient.shared.networkSettings(session: session)
                if !Task.isCancelled, self.session == session { topology = settings.topology }
            } catch { if !Task.isCancelled, self.session == session { self.error = error.localizedDescription } }
        }
    }
}

struct MobileRobotInventory: View {
    @ObservedObject var model: AppModel
    let chooseDevice: () -> Void
    @State private var robots: [UnitreeRobot] = []
    @State private var status: String?
    var body: some View {
        List {
            if model.selectedDeviceReady && !model.connectedLite {
                if let status { Text(status).foregroundStyle(.secondary).lineLimit(3).truncationMode(.tail) }
                ForEach(robots) { robot in
                    NavigationLink {
                        if model.selectedDeviceReady { NativeSiteView(model: model) }
                    } label: { LensRow(title: robot.name.isEmpty ? robot.serial : robot.name,
                                       subtitle: robot.connected ? "Connected" : "Paired", symbol: "figure.walk", color: .orange) }
                }
                if robots.isEmpty && status == nil { Text("No robots found on this device.").foregroundStyle(.secondary) }
            } else {
                ContentUnavailableView {
                    Label("Choose an onboard device", systemImage: "cpu")
                } description: { Text("Connect to the device that manages your robot.") } actions: { Button("Open Devices", action: chooseDevice) }
            }
        }.scrollContentBackground(.hidden).background(MobileLensStyle.background).navigationTitle("Robots")
            .task(id: model.deviceSession) {
                robots = []; status = nil
                guard model.mobileScopeIsCurrent, model.selectedDeviceReady, !model.connectedLite else { return }
                do {
                    let result = try await UnitreeClient.shared.robots(session: model.deviceSession)
                    guard !Task.isCancelled else { return }
                    robots = result.robots; status = result.warning.isEmpty ? nil : result.warning
                } catch { if !Task.isCancelled { status = error.localizedDescription } }
            }
    }
}

private struct PresenceCount: Identifiable {
    let title: String
    let count: Int
    let color: Color
    var id: String { title }
}
@MainActor private func presenceCounts(_ model: AppModel) -> [PresenceCount] {
    guard model.mobileScopeIsCurrent else { return [] }
    if !model.mobileCloudOrganization.isEmpty {
        guard model.cloudError == nil, model.cloudLoaded, let snapshot = model.mobilePresence,
              Date().timeIntervalSince(snapshot.1) < 60 else { return [] }
        guard snapshot.0.presenceError.isEmpty else { return [PresenceCount(title: "unknown", count: model.cloud.count, color: .gray)] }
        let online = snapshot.0.devices.filter(\.online).count
        return [PresenceCount(title: "online", count: online, color: .green),
                PresenceCount(title: "offline", count: snapshot.0.devices.count - online, color: .gray)].filter { $0.count > 0 }
    }
    guard model.nearbyLoaded && model.nearbyError == nil else { return [] }
    return [PresenceCount(title: "nearby", count: model.nearby.count, color: MobileLensStyle.green)]
}
struct MobilePresenceLegend: View {
    @ObservedObject var model: AppModel
    var body: some View {
        VStack(alignment: .trailing, spacing: 4) {
            ForEach(presenceCounts(model)) { value in HStack(spacing: 5) {
                Circle().fill(value.color).frame(width: 7, height: 7)
                Text("\(value.count) \(value.title)").font(.caption.weight(.semibold))
            } }
        }
    }
}
struct MobilePresenceBar: View {
    @ObservedObject var model: AppModel
    var body: some View {
        let values = presenceCounts(model)
        let total = max(1, values.reduce(0) { $0 + $1.count })
        GeometryReader { g in
            HStack(spacing: 0) {
                ForEach(values) { value in
                    Rectangle().fill(value.color).frame(width: g.size.width * CGFloat(value.count) / CGFloat(total))
                }
            }
        }.frame(height: 7).background(.quaternary).clipShape(Capsule()).accessibilityHidden(true)
    }
}


struct MobileLensUnderConstruction: View {
    let title: String
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.scenePhase) private var phase
    var body: some View {
        VStack(spacing: 16) {
            Spacer()
            TimelineView(.animation(minimumInterval: 1.0 / 24, paused: reduceMotion || phase != .active)) { timeline in
                let time = reduceMotion || phase != .active ? 0.45 : timeline.date.timeIntervalSinceReferenceDate
                RobotForgeIllustration(time: time)
            }.frame(maxWidth: 340).frame(height: 260)
                .accessibilityLabel("A robot hammering an anvil")
            Text("Under construction").font(.title2.bold())
            Text("We’re building the \(title) lens.")
                .font(.body).foregroundStyle(.secondary).multilineTextAlignment(.center)
            Spacer()
            Spacer().frame(height: 32)
        }.padding(24).frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(MobileLensStyle.background).navigationTitle(title).navigationBarTitleDisplayMode(.inline)
    }
}

private struct RobotForgeIllustration: View {
    let time: Double
    var body: some View {
        Canvas { context, size in
            context.scaleBy(x: size.width / 320, y: size.height / 260)
            let green = MobileLensStyle.green
            let dark = Color(red: 0.12, green: 0.16, blue: 0.17)
            func rounded(_ rect: CGRect, _ radius: CGFloat, _ color: Color) {
                context.fill(Path(roundedRect: rect, cornerRadius: radius), with: .color(color))
            }
            func line(_ points: [CGPoint], _ color: Color, _ width: CGFloat) {
                var path = Path()
                path.addLines(points)
                context.stroke(path, with: .color(color), style: StrokeStyle(lineWidth: width, lineCap: .round, lineJoin: .round))
            }
            context.fill(Path(ellipseIn: CGRect(x: 25, y: 223, width: 272, height: 18)), with: .color(green.opacity(0.09)))
            // Robot boots, legs, body and friendly face.
            rounded(CGRect(x: 68, y: 182, width: 18, height: 40), 8, dark)
            rounded(CGRect(x: 109, y: 182, width: 18, height: 40), 8, dark)
            rounded(CGRect(x: 57, y: 212, width: 36, height: 17), 7, green)
            rounded(CGRect(x: 104, y: 212, width: 36, height: 17), 7, green)
            rounded(CGRect(x: 57, y: 113, width: 83, height: 78), 22, green)
            rounded(CGRect(x: 74, y: 130, width: 48, height: 40), 12, .white.opacity(0.18))
            rounded(CGRect(x: 88, y: 142, width: 21, height: 16), 4, .white.opacity(0.85))
            line([CGPoint(x: 98, y: 49), CGPoint(x: 98, y: 35)], dark, 4)
            context.fill(Path(ellipseIn: CGRect(x: 92, y: 27, width: 12, height: 12)), with: .color(green))
            rounded(CGRect(x: 54, y: 50, width: 88, height: 66), 22, green)
            rounded(CGRect(x: 65, y: 64, width: 66, height: 36), 13, dark)
            rounded(CGRect(x: 77, y: 76, width: 9, height: 12), 4, .white)
            rounded(CGRect(x: 109, y: 76, width: 9, height: 12), 4, .white)
            line([CGPoint(x: 59, y: 134), CGPoint(x: 42, y: 160), CGPoint(x: 53, y: 176)], dark, 13)
            context.fill(Path(ellipseIn: CGRect(x: 45, y: 168, width: 17, height: 17)), with: .color(green))
            // Heavy anvil and wooden stand.
            rounded(CGRect(x: 198, y: 189, width: 64, height: 38), 8, Color(red: 0.61, green: 0.46, blue: 0.31))
            var anvil = Path()
            anvil.move(to: CGPoint(x: 174, y: 151))
            anvil.addLines([CGPoint(x: 288, y: 151), CGPoint(x: 274, y: 169), CGPoint(x: 246, y: 173), CGPoint(x: 241, y: 183), CGPoint(x: 262, y: 190), CGPoint(x: 190, y: 190), CGPoint(x: 208, y: 180), CGPoint(x: 205, y: 171), CGPoint(x: 185, y: 168)])
            anvil.closeSubpath()
            context.fill(anvil, with: .color(dark))
            line([CGPoint(x: 178, y: 151), CGPoint(x: 284, y: 151)], .gray, 5)
            // Pivot the forearm and hammer through one gentle strike.
            let cycle = time.truncatingRemainder(dividingBy: 1.6) / 1.6
            let lift = (1 - cos(cycle * .pi * 2)) / 2
            var arm = context
            arm.translateBy(x: 136, y: 128)
            arm.rotate(by: .degrees(-58 * lift))
            var handle = Path()
            handle.move(to: .zero); handle.addLine(to: CGPoint(x: 77, y: 4))
            arm.stroke(handle, with: .color(dark), style: StrokeStyle(lineWidth: 12, lineCap: .round))
            arm.fill(Path(ellipseIn: CGRect(x: 25, y: -6, width: 20, height: 20)), with: .color(green))
            arm.fill(Path(roundedRect: CGRect(x: 65, y: -12, width: 34, height: 30), cornerRadius: 6), with: .color(.gray))
            if cycle < 0.12 {
                let glow = 1 - cycle / 0.12
                line([CGPoint(x: 236, y: 139), CGPoint(x: 247, y: 126)], .orange.opacity(glow), 3)
                line([CGPoint(x: 246, y: 147), CGPoint(x: 262, y: 143)], .orange.opacity(glow), 3)
            }
        }.accessibilityHidden(true)
    }
}
