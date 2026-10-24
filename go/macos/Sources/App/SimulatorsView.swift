#if os(macOS)
import SwiftUI
import AppKit

struct SimulatorsView: View {
    @State private var actions = AsyncActions()
    @ObservedObject var model: SimulatorModel
    let connect: (LocalSimulator) -> Void
    @State private var creating = false
    @State private var deleting: LocalSimulator?
    @State private var resetting: LocalSimulator?
    var body: some View {
        actionContent.task { await actions.run() }
    }

    @ViewBuilder private var actionContent: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Simulators").font(.title2.weight(.semibold))
                Spacer()
                Button("Create simulator", systemImage: "plus") { creating = true }.disabled(model.operation != nil)
            }.padding(20)
            if let operation = model.operation {
                HStack { ProgressView().controlSize(.small); Text(operation); Spacer(); Text("Setup can take several minutes.").foregroundStyle(.secondary) }
                    .font(.callout).padding(.horizontal, 20).padding(.bottom, 12)
            }
            if let error = model.error {
                HStack(alignment: .top) {
                    Image(systemName: "exclamationmark.triangle").foregroundStyle(.orange)
                    Text(error).textSelection(.enabled)
                    Spacer()
                    if let target = model.recoveryTarget, model.recovery != WDSimRecoveryNone {
                        Button(model.recovery == WDSimRecoveryRuntime ? "Update robot runtime" : "Update agent") {
                            let action = model.recovery == WDSimRecoveryRuntime ? WDSimUpdateRuntime : WDSimUpdateAgent
                            actions.submit { await model.act(target, action, label: "Updating") }
                        }.disabled(model.operation != nil)
                    }
                    Button { model.error = nil } label: { Image(systemName: "xmark") }.buttonStyle(.plain).accessibilityLabel("Dismiss error")
                }.font(.callout).padding(12).background(.orange.opacity(0.08)).padding(.horizontal, 20).padding(.bottom, 12)
            }
            Divider()
            HSplitView {
                List(selection: $model.selection) {
                    ForEach(model.items) { item in
                        VStack(alignment: .leading, spacing: 5) {
                            Text(item.name).fontWeight(.medium)
                            Text(item.title).font(.caption).foregroundStyle(.secondary)
                            Label(item.state.capitalized, systemImage: item.running ? "circle.fill" : "circle")
                                .font(.caption2).foregroundStyle(item.running ? .green : .secondary)
                        }.padding(.vertical, 5).tag(item.id)
                    }
                }.listStyle(.sidebar).frame(minWidth: 180, idealWidth: 220, maxWidth: 300)
                .contextMenu(forSelectionType: String.self) { ids in
                    if let item = model.items.first(where: { ids.contains($0.id) }) {
                        if item.running {
                            Button("Stop", systemImage: "stop.fill") {
                                actions.submit { await model.act(item, WDSimStop, label: "Stopping") }
                            }.disabled(model.operation != nil)
                        } else {
                            Button("Start", systemImage: "play.fill") {
                                actions.submit { await model.act(item, WDSimStart, label: "Starting") }
                            }.disabled(model.operation != nil)
                        }
                    }
                }
                if let item = model.selected {
                    let preparing = model.preparingSimulator == item.name
                    VStack(alignment: .leading, spacing: 0) {
                        HStack {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(item.name).font(.headline)
                                Text(item.title).font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if item.running {
                                if !item.lite { Button("Open device") { connect(item) } }
                                Button("Shut down") { actions.submit { await model.act(item, WDSimStop, label: "Stopping") } }
                            } else {
                                Button("Start", systemImage: "play.fill") { actions.submit { await model.act(item, WDSimStart, label: "Starting") } }
                            }
                            Menu {
                                if item.robot {
                                    Button("Prepare simulation") { actions.submit { await model.act(item, WDSimStart, label: "Preparing") } }
                                    Button("Update robot runtime") { actions.submit { await model.act(item, WDSimUpdateRuntime, label: "Updating runtime for") } }
                                    Button("Update agent") { actions.submit { await model.act(item, WDSimUpdateAgent, label: "Updating agent for") } }.disabled(!item.running)
                                    Divider()
                                }
                                Button("Copy device selector") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString("vm:\(item.name)", forType: .string) }
                                Button("Delete simulator…", role: .destructive) { deleting = item }.disabled(item.running)
                            } label: { Image(systemName: "ellipsis.circle") }
                                .menuStyle(.borderlessButton)
                                .menuIndicator(.hidden)
                                .fixedSize()
                                .frame(width: 24)
                                .help("Simulator options")
                                .accessibilityLabel("Simulator options")
                        }.padding(16).disabled(model.operation != nil)
                        if !preparing && !item.error.isEmpty { Text(item.error).font(.callout).foregroundStyle(.orange).padding(.horizontal, 16) }
                        if item.robot && (item.running || preparing) {
                            RobotSimulationView(item: item, busy: model.operation != nil, preparing: preparing, control: { action, label in
                                actions.submit { await model.act(item, action, label: label) }
                            }, reset: { resetting = item }).id(item.name)
                        } else if item.robot {
                            ContentUnavailableView("Simulator is stopped", systemImage: "cube.transparent", description: Text("Start this simulator to load its live 3D scene."))
                                .frame(maxWidth: .infinity, maxHeight: .infinity)
                        } else {
                            VStack(spacing: 0) {
                                SimulatorRenderer(scene: nil, pose: nil, schematic: item.lite ? "esp32" : "wendyos", follow: false, recenter: 0)
                                Text(item.lite ? "ESP32-C6 board schematic. Pin states are not exposed by the emulator." : "WendyOS device schematic. This VM has no physical scene.")
                                    .font(.caption).foregroundStyle(.secondary).padding(12)
                            }
                        }
                        Divider()
                        VStack(alignment: .leading, spacing: 5) {
                            Text("wendy run --device vm:\(item.name)").font(.system(.callout, design: .monospaced)).textSelection(.enabled)
                            HStack {
                                Text(item.address.isEmpty ? "No active device address" : item.address)
                                Spacer()
                                Text(item.version.isEmpty ? "Local image" : item.version)
                            }.font(.caption).foregroundStyle(.secondary)
                        }.padding(16)
                    }.frame(minWidth: 420, maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    ContentUnavailableView("Create a simulator", systemImage: "cube.transparent", description: Text("Run WendyOS, robot simulations or ESP32-C6 firmware on your Mac."))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .sheet(isPresented: $creating) { CreateSimulatorSheet { name, profile, image, disk in
            creating = false
            actions.submit { await model.create(name: name, profile: profile, image: image, disk: disk) }
        } }
        .alert("Delete simulator?", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }), presenting: deleting) { item in
            Button("Cancel", role: .cancel) {}
            Button("Delete", role: .destructive) { actions.submit { await model.act(item, WDSimDelete, label: "Deleting") } }
        } message: { item in Text("Delete \(item.name), its disk, snapshots and console logs? This cannot be undone.") }
        .alert("Reset simulation?", isPresented: Binding(get: { resetting != nil }, set: { if !$0 { resetting = nil } }), presenting: resetting) { item in
            Button("Cancel", role: .cancel) {}
            Button("Reset") { actions.submit { await model.act(item, WDSimReset, label: "Resetting") } }
        } message: { _ in Text("Reset the world and release its active controllers.") }
        .focusedSceneValue(\.simulatorRefresh, PaneRefreshAction(enabled: !model.refreshing, perform: { actions.submit { await model.refresh() } }))
        .task {
            while !Task.isCancelled { await model.refresh(); try? await Task.sleep(for: .seconds(3)) }
        }
    }
}

private struct RobotSimulationView: View {
    let item: LocalSimulator
    let busy: Bool
    let preparing: Bool
    let control: (WDSimulatorAction, String) -> Void
    let reset: () -> Void
    @StateObject private var render = SimulatorRenderModel()
    @State private var follow = true
    @State private var recenter = 0
    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Button(render.pose?.mode == "paused" ? "Resume" : "Pause", systemImage: render.pose?.mode == "paused" ? "play" : "pause") {
                    control(render.pose?.mode == "paused" ? WDSimResume : WDSimPause, render.pose?.mode == "paused" ? "Resuming" : "Pausing")
                }.disabled(busy || render.pose == nil || render.error != nil)
                Button("Reset", action: reset).disabled(busy || render.scene == nil)
                Spacer()
                Toggle("Follow", isOn: $follow).toggleStyle(.checkbox)
                Button("Recenter", systemImage: "scope") { recenter += 1 }
            }.padding(.horizontal, 16).padding(.bottom, 12)
            ZStack(alignment: preparing || render.error != nil || render.pose == nil ? .center : .topLeading) {
                SimulatorRenderer(scene: render.scene, pose: render.pose, schematic: nil, follow: follow, recenter: recenter)
                if preparing || (render.error == nil && render.pose == nil) {
                    SimulationLoadingView(preparing: preparing)
                        .padding(24)
                } else if let error = render.error {
                    SimulationErrorView(error: error, busy: busy) {
                        control(WDSimStart, "Preparing")
                    }
                    .padding(24)
                } else if let pose = render.pose {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(pose.valid ? (pose.mode == "paused" ? "Paused" : "Live simulation") : "Physics fault · last valid pose")
                        Text(String(format: "%.2f s", pose.time)).font(.caption.monospacedDigit())
                    }.padding(12).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8)).padding(12)
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
            Text("Drag to orbit · Scroll to zoom").font(.caption).foregroundStyle(.secondary).padding(8)
        }.task(id: preparing) { await render.stream(name: item.name) }
    }
}

private struct SimulationLoadingView: View {
    let preparing: Bool

    var body: some View {
        VStack(spacing: 20) {
            Image(systemName: "cube.transparent")
                .font(.system(size: 32, weight: .light))
                .foregroundStyle(.white.opacity(0.85))
                .frame(width: 72, height: 72)
                .background(.white.opacity(0.04), in: RoundedRectangle(cornerRadius: 20))
                .overlay(RoundedRectangle(cornerRadius: 20).strokeBorder(.white.opacity(0.08)))
                .accessibilityHidden(true)
            VStack(spacing: 8) {
                Text(preparing ? "Preparing simulation" : "Loading scene")
                    .font(.system(size: 18, weight: .semibold))
                    .foregroundStyle(.white.opacity(0.95))
                Text(preparing ? "Setting up the robot and its world.\nThis may take a few minutes." : "Connecting to the robot's live scene.")
                    .font(.callout)
                    .foregroundStyle(.white.opacity(0.6))
                    .fixedSize(horizontal: false, vertical: true)
            }
            ProgressView()
                .controlSize(.small)
                .accessibilityLabel(preparing ? "Preparing simulation" : "Loading scene")
        }
        .multilineTextAlignment(.center)
        .frame(maxWidth: 320)
        .environment(\.colorScheme, .dark)
    }
}

private struct SimulationErrorView: View {
    let error: String
    let busy: Bool
    let prepare: () -> Void

    var body: some View {
        VStack(spacing: 20) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 26, weight: .regular))
                .foregroundStyle(.orange.opacity(0.9))
                .frame(width: 64, height: 64)
                .background(.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 18))
                .accessibilityHidden(true)
            VStack(spacing: 8) {
                Text("Scene unavailable")
                    .font(.system(size: 18, weight: .semibold))
                    .foregroundStyle(.white.opacity(0.95))
                Text(error)
                    .font(.callout)
                    .foregroundStyle(.white.opacity(0.65))
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
            VStack(spacing: 12) {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.mini)
                    Text("Retrying connection…").font(.caption)
                }
                .foregroundStyle(.white.opacity(0.5))
                Button("Prepare simulation", action: prepare)
                    .controlSize(.large)
                    .disabled(busy)
            }
        }
        .multilineTextAlignment(.center)
        .frame(maxWidth: 280)
        .padding(28)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 20))
        .overlay(RoundedRectangle(cornerRadius: 20).strokeBorder(.white.opacity(0.08)))
        .environment(\.colorScheme, .dark)
    }
}

private struct CreateSimulatorSheet: View {
    @Environment(\.dismiss) private var dismiss
    let create: (String, String, String, Int) -> Void
    @State private var name = ""
    @State private var profile = "generic"
    @State private var image = ""
    @State private var disk = 16
    private var validName: Bool { name.range(of: "^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$", options: .regularExpression) != nil }
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Create simulator").font(.title2)
            Form {
                TextField("Name", text: $name)
                Picker("Hardware", selection: $profile) { ForEach(SimulatorProfile.all) { Text($0.title).tag($0.id) } }
                if !profile.hasPrefix("wendy-lite") { Stepper("Disk: \(disk) GiB", value: $disk, in: 8...256, step: 8) }
                HStack {
                    Text(image.isEmpty ? "Latest published image" : URL(fileURLWithPath: image).lastPathComponent).lineLimit(1)
                    Spacer()
                    Button("Choose image…") {
                        let panel = NSOpenPanel(); panel.canChooseDirectories = false
                        if panel.runModal() == .OK { image = panel.url?.path ?? "" }
                    }
                    if !image.isEmpty { Button("Clear") { image = "" } }
                }
            }
            Text("Use 1–32 lowercase letters, digits or dashes. Start and end with a letter or digit.").font(.caption).foregroundStyle(.secondary)
            Text(profile.hasPrefix("wendy-lite") ? "Runs ESP32-C6 firmware using esp-emu. A local image must be a merged flash binary. Install esp-emu before starting." : "Downloads WendyOS if no local image is selected. QEMU is required. Robot setup downloads its runtime on first start and may take several minutes.")
                .font(.callout).foregroundStyle(.secondary)
            HStack { Spacer(); Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Create") { create(name, profile, image, disk) }.keyboardShortcut(.defaultAction).disabled(!validName)
            }
        }.padding(24).frame(width: 520)
    }
}

private struct SimulatorRefreshKey: FocusedValueKey { typealias Value = PaneRefreshAction }
extension FocusedValues {
    var simulatorRefresh: PaneRefreshAction? {
        get { self[SimulatorRefreshKey.self] }
        set { self[SimulatorRefreshKey.self] = newValue }
    }
}
#endif
