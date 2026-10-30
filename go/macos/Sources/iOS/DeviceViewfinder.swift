import SwiftUI

/// Visual matches are suggestions. Opening a card uses the normal authenticated
/// device connection; the card never silently attaches a model match to a device.
struct DeviceViewfinder: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var phase
    @State private var controller = DeviceViewfinderController()
    @State private var actions = AsyncActions()
    @State private var selected: DeviceSight?
    @State private var selectedDevice: Device?
    @State private var registering: DeviceRecognitionFrame?
    @State private var remembering = false
    @State private var library = false
    @State private var saved: [DeviceVisualReference] = []
    @State private var diagnostics = false
    @State private var error: String?

    var body: some View {
        GeometryReader { geometry in
            ZStack {
                Color.black
                if controller.allowed { DeviceViewfinderSurface(controller: controller) }
                if remembering { reticle(in: geometry.size) }
                TimelineView(.periodic(from: .now, by: 0.25)) { context in
                    ForEach(controller.visibleSights(at: context.date)) { sight in sightOutline(sight, size: geometry.size) }
                }
                VStack {
                    HStack {
                        Button("Close", systemImage: "xmark") { dismiss() }
                        Spacer()
                        Button("Recognition diagnostics", systemImage: "info.circle") { diagnostics = true }.labelStyle(.iconOnly)
                        Button("Saved appearances", systemImage: "square.stack") {
                            actions.submit {
                                saved = await controller.engine.registered()
                                library = true
                            }
                        }.labelStyle(.iconOnly)
                    }.padding().background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 20))
                    TimelineView(.periodic(from: .now, by: 0.25)) { context in
                        let sights = controller.visibleSights(at: context.date).sorted { $0.id < $1.id }
                        if !sights.isEmpty {
                            ScrollView {
                                VStack(spacing: 8) {
                                    ForEach(sights) { sight in sightCard(sight) }
                                }
                            }
                            .scrollBounceBehavior(.basedOnSize)
                            .frame(height: min(CGFloat(sights.count) * 80, min(240, geometry.size.height * 0.3)))
                            .frame(maxWidth: 420)
                        }
                    }
                    Spacer()
                    VStack(spacing: 10) {
                        if let message = error ?? controller.error { Text(message).foregroundStyle(.orange) }
                        Text(remembering ? "Fill the frame with the device. Include distinctive details and keep the background outside." : controller.tracking)
                            .font(.callout).multilineTextAlignment(.center)
                        if remembering {
                            HStack {
                                Button("Cancel") { remembering = false }
                                Spacer()
                                Button("Capture appearance", systemImage: "camera") {
                                    registering = controller.frame
                                }.disabled(controller.frame == nil)
                            }
                        } else {
                            Button("Remember a device’s appearance", systemImage: "viewfinder") { remembering = true }
                                .font(.subheadline)
                            Text("Appearance can be shared by identical devices. Tap a match to choose and connect.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }.padding().background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 20))
                }.padding()
            }
            .onAppear { controller.viewport = geometry.size }
            .onChange(of: geometry.size) { _, size in controller.viewport = size }
        }
        .preferredColorScheme(.dark)
        .task { await actions.run() }
        .task(id: phase) { if phase == .active { await controller.run() } }
        .task {
            while !Task.isCancelled {
                await model.refreshNearby()
                controller.devices = model.nearby
                do { try await Task.sleep(for: .seconds(10)) } catch { return }
            }
        }
        .onChange(of: model.nearby) { _, devices in controller.devices = devices }
        .sheet(isPresented: $diagnostics) {
            NavigationStack {
                Form {
                    Section("Recognition") {
                        Text(controller.tracking)
                        Text(controller.diagnostics).font(.caption.monospaced()).textSelection(.enabled)
                    }
                    Section {
                        Text("Save one camera frame on this iPhone so we can diagnose this miss. It is not uploaded. A new capture replaces the previous diagnostic frame.")
                        Button(controller.savingDiagnostic ? "Saving…" : "Save diagnostic frame", systemImage: "camera") {
                            actions.submit { await controller.saveDiagnosticFrame() }
                        }.disabled(controller.frame == nil || controller.savingDiagnostic)
                        if controller.savedDiagnostic { Label("Diagnostic frame saved on this iPhone", systemImage: "checkmark.circle") }
                    }
                }.navigationTitle("Recognition diagnostics").navigationBarTitleDisplayMode(.inline)
                    .toolbar { Button("Done") { diagnostics = false } }
            }.presentationDetents([.medium, .large])
        }
        .sheet(item: $selected) { sight in
            NavigationStack {
                List {
                    Section {
                        Text(sight.evidence).foregroundStyle(.secondary)
                        Text("Choose the device to open its state and cameras. Matching appearances are not unique identifiers.")
                    }
                    let candidates = model.nearby.filter { sight.deviceIDs.contains($0.id) }
                    if !candidates.isEmpty {
                        Section("Matching nearby devices") { ForEach(candidates) { device in deviceButton(device) } }
                    }
                    Section(candidates.isEmpty ? "Nearby devices" : "Other nearby devices") {
                        ForEach(model.nearby.filter { !sight.deviceIDs.contains($0.id) }) { device in deviceButton(device) }
                        if model.nearby.isEmpty { Text("No nearby devices found.") }
                    }
                    if let error = model.nearbyError { Text(error).foregroundStyle(.orange) }
                }
                .navigationTitle(sight.label).navigationBarTitleDisplayMode(.inline)
                .toolbar { Button("Done") { selected = nil } }
            }.presentationDetents([.medium, .large])
        }
        .sheet(item: $selectedDevice) { device in
            DeviceSightDetails(device: device, model: model)
        }
        .sheet(isPresented: Binding(get: { registering != nil }, set: { if !$0 { registering = nil } })) {
            NavigationStack {
                List {
                    Section {
                        Text("Choose which device you photographed. This saves an appearance on this phone, not a camera calibration. Identical devices can still match.")
                    }
                    ForEach(model.nearby) { device in
                        Button(device.title) {
                            guard let frame = registering else { return }
                            actions.submit {
                                do {
                                    await model.connect(device)
                                    guard model.connected?.id == device.id, model.detailError == nil else {
                                        throw WendyError(message: model.detailError ?? "Could not connect to the selected device.")
                                    }
                                    try await controller.engine.remember(controller.reference(for: device, frame: frame))
                                    registering = nil; remembering = false; error = nil
                                } catch { self.error = error.localizedDescription; registering = nil }
                            }
                        }.disabled(model.connectingDevice != nil)
                    }
                    if model.nearby.isEmpty { Text("No nearby devices found. Connect the device to the same network first.") }
                    if model.connectingDevice != nil { ProgressView("Connecting…") }
                }.navigationTitle("Remember appearance").navigationBarTitleDisplayMode(.inline)
                    .toolbar { Button("Cancel") { registering = nil } }
            }
        }
        .sheet(isPresented: $library) {
            NavigationStack {
                List {
                    if saved.isEmpty { Text("No saved device appearances.") }
                    ForEach(saved) { ref in
                        HStack {
                            Text(ref.label)
                            Spacer()
                            Button("Remove", systemImage: "trash", role: .destructive) {
                                actions.submit {
                                    do {
                                        try await controller.engine.forget(ref.id)
                                        saved = await controller.engine.registered()
                                    } catch { self.error = error.localizedDescription }
                                }
                            }.labelStyle(.iconOnly)
                        }
                    }
                }.navigationTitle("Saved appearances").navigationBarTitleDisplayMode(.inline)
                    .toolbar { Button("Done") { library = false } }
            }.presentationDetents([.medium])
        }
    }

    private func deviceButton(_ device: Device) -> some View {
        Button {
            selected = nil
            // A single sheet route avoids presenting a second sheet over the picker.
            actions.submit {
                do { try await Task.sleep(for: .milliseconds(400)) } catch { return }
                selectedDevice = device
            }
        } label: { Label(device.title, systemImage: device.lite ? "cpu" : "network") }
    }

    private func sightOutline(_ sight: DeviceSight, size: CGSize) -> some View {
        let points = sight.corners.map { CGPoint(x: $0.x * size.width, y: $0.y * size.height) }
        return Path { path in
            guard let first = points.first else { return }
            path.move(to: first)
            for point in points.dropFirst() { path.addLine(to: point) }
            path.closeSubpath()
        }.stroke(.mint, style: StrokeStyle(lineWidth: 2, dash: [6, 3])).allowsHitTesting(false)
    }

    private func sightCard(_ sight: DeviceSight) -> some View {
        Button { selected = sight } label: {
            HStack(spacing: 12) {
                Image(systemName: "cpu").font(.title2).foregroundStyle(.mint)
                VStack(alignment: .leading, spacing: 4) {
                    Text(sight.label).font(.subheadline.bold()).lineLimit(1)
                    Text(sight.evidence).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }.frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: "chevron.right").font(.caption.bold()).foregroundStyle(.secondary)
            }
            .padding(14)
            .frame(maxWidth: .infinity, minHeight: 68)
            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
            .contentShape(RoundedRectangle(cornerRadius: 16))
        }.buttonStyle(.plain)
    }
    private func reticle(in size: CGSize) -> some View {
        Rectangle().stroke(.white, style: StrokeStyle(lineWidth: 2, dash: [8,4]))
            .frame(width: size.width * 0.6, height: size.height * 0.4)
            .position(x: size.width * 0.5, y: size.height * 0.5).allowsHitTesting(false)
    }
}

private struct DeviceSightDetails: View {
    let device: Device
    @ObservedObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var info: DeviceInfoSnapshot?
    @State private var failure: String?
    @State private var checkedAt: Date?
    @State private var feedID: UUID?
    @State private var loading = true
    @State private var retry = 0

    var body: some View {
        NavigationStack {
            Group {
                if loading {
                    VStack(spacing: 18) {
                        Image(systemName: device.lite ? "cpu" : "network")
                            .font(.system(size: 30)).foregroundStyle(.secondary)
                        HStack(spacing: 10) {
                            ProgressView().controlSize(.small)
                            Text("Connecting to device…").font(.headline)
                        }
                        Text("Loading its status and cameras.")
                            .font(.subheadline).foregroundStyle(.secondary)
                    }.padding(24).frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if let failure {
                    FailureView(title: "Could not connect", error: failure) { retry += 1 }
                } else {
                    connectedContent
                }
            }
            .navigationTitle(device.title).navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
        }
        .presentationDetents(loading ? [.height(280)] : [.medium, .large])
        .presentationDragIndicator(.visible)
        .task(id: retry) { await refreshConnection() }
    }

    private var connectedContent: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                if model.connected?.id == device.id, let info {
                    Label("Connected", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                    Text([info.board, info.os, info.version].filter { !$0.isEmpty }.joined(separator: " · "))
                    if let checkedAt { Text("Updated \(checkedAt, style: .relative) ago").font(.caption).foregroundStyle(.secondary) }
                    if !model.connectedLite {
                        if let error = model.appsError { Text(error).foregroundStyle(.orange) }
                        else {
                            Text("\(model.apps.filter { $0.runningState == .running }.count) of \(model.apps.count) apps running")
                            if model.apps.contains(where: { $0.runningState == .crashLooping }) {
                                Label("An app is restarting repeatedly", systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                            }
                        }
                    }
                    if !model.cameraFeeds.isEmpty {
                        Picker("Camera", selection: $feedID) {
                            Text("Choose camera").tag(nil as UUID?)
                            ForEach(model.cameraFeeds) { feed in Text(feed.camera.name).tag(Optional(feed.id)) }
                        }
                        if let feed = model.cameraFeeds.first(where: { $0.id == feedID }) {
                            DeviceSightVideo(feed: feed, model: model).id(feed.id)
                        }
                    } else { Text("No cameras reported.").foregroundStyle(.secondary) }
                    if let error = model.camerasError { Text(error).foregroundStyle(.orange) }
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding()
        }
    }

    private func refreshConnection() async {
        loading = true; failure = nil; info = nil
        await model.connect(device)
        guard !Task.isCancelled else { return }
        guard model.connected?.id == device.id, model.detailError == nil else {
            failure = model.detailError ?? "The device could not be reached."
            loading = false
            return
        }
        let session = model.deviceSession
        while !Task.isCancelled, model.deviceSession == session {
            do {
                let snapshot = try await GoClient.shared.deviceInfo(session: session)
                try Task.checkCancellation()
                guard model.deviceSession == session else {
                    info = nil; loading = false; failure = "The active device connection changed."
                    return
                }
                info = snapshot; failure = nil; checkedAt = Date(); loading = false
                await model.refreshDetails()
                if feedID == nil { feedID = model.cameraFeeds.first?.id }
                try await Task.sleep(for: .seconds(5))
            } catch is CancellationError { return }
            catch {
                info = nil; checkedAt = nil; failure = error.localizedDescription; loading = false
                do { try await Task.sleep(for: .seconds(5)) } catch { return }
            }
        }
    }
}

private struct DeviceSightVideo: View {
    @ObservedObject var feed: CameraFeed
    @ObservedObject var model: AppModel
    @Environment(\.scenePhase) private var phase
    var body: some View {
        VStack {
            CameraFeedDisplay(feed: feed).aspectRatio(4/3, contentMode: .fit).background(.black)
            TimelineView(.periodic(from: .now, by: 1)) { context in
                if let error = feed.error { Text(error).foregroundStyle(.orange) }
                else if let last = feed.lastVideoReceivedAt, context.date.timeIntervalSince(last) < 3 { Text("Live").foregroundStyle(.green) }
                else { Text(feed.running ? "Waiting for video…" : "Paused").foregroundStyle(.secondary) }
            }.font(.caption)
        }
        .task(id: phase) {
            if phase == .active { await model.playCamera(feed, inline: true) }
            else { model.pauseCamera(feed) }
        }
        .onDisappear { model.pauseCamera(feed) }
    }
}
