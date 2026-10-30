#if os(macOS)
import SwiftUI
import AppKit

struct RobotIcon: View {
    var body: some View {
        VStack(spacing: 0) {
            Circle().frame(width: 3, height: 3)
            Rectangle().frame(width: 1, height: 2)
            HStack(spacing: 4) {
                Circle().frame(width: 3, height: 3)
                Circle().frame(width: 3, height: 3)
            }
            .frame(width: 15, height: 10)
            .overlay(RoundedRectangle(cornerRadius: 3).stroke(lineWidth: 1.3))
        }.frame(width: 18, height: 18).accessibilityHidden(true)
    }
}

struct SavedUnitreeDevice: Codable {
    let serial, name, address: String
    var robot: UnitreeRobot { UnitreeRobot(serial: serial, name: name, address: address, paired: false, connected: false) }
}

enum UnitreeSavedDevices {
    static let key = "savedUnitreeDevices"
    static func load(defaults: UserDefaults = .standard) -> [UnitreeRobot] {
        guard let data = defaults.data(forKey: key),
              let devices = try? JSONDecoder().decode([SavedUnitreeDevice].self, from: data) else { return [] }
        return devices.map(\.robot)
    }
    static func save(_ robots: [UnitreeRobot], defaults: UserDefaults = .standard) {
        let devices = robots.map { SavedUnitreeDevice(serial: $0.serial, name: $0.name, address: $0.address) }
        if let data = try? JSONEncoder().encode(devices) { defaults.set(data, forKey: key) }
    }
    static func merge(saved: [UnitreeRobot], discovered: [UnitreeRobot]) -> [UnitreeRobot] {
        var bySerial: [String: UnitreeRobot] = [:]
        for robot in saved + discovered { bySerial[robot.serial] = robot }
        return bySerial.values.sorted { $0.name == $1.name ? $0.serial < $1.serial : $0.name < $1.name }
    }
}

extension AppModel {
    func rememberUnitree(_ robot: UnitreeRobot) {
        let saved = UnitreeSavedDevices.merge(saved: UnitreeSavedDevices.load(), discovered: [robot])
        UnitreeSavedDevices.save(saved)
        nearbyUnitree = UnitreeSavedDevices.merge(saved: nearbyUnitree, discovered: saved)
    }
    func forgetSavedUnitree(_ robot: UnitreeRobot) {
        UnitreeSavedDevices.save(UnitreeSavedDevices.load().filter { $0.id != robot.id })
        nearbyUnitree.removeAll { $0.id == robot.id }
    }
}

struct UnitreeMotion: Equatable {
    var forward: Double = 0
    var lateral: Double = 0
    var yaw: Double = 0
    static func keys(_ keys: Set<UInt16>) -> Self {
        func held(_ codes: UInt16...) -> Double { codes.contains(where: keys.contains) ? 1 : 0 }
        return Self(forward: 0.6 * (held(13, 126) - held(1, 125)),
                    lateral: 0.3 * (held(0, 123) - held(2, 124)),
                    yaw: 0.5 * (held(12) - held(14)))
    }
}

struct UnitreeKeyboard: NSViewRepresentable {
    let enabled: Bool
    let changed: (UnitreeMotion) -> Void
    let stop: () -> Void
    var suspend: () -> Void = {}
    func makeNSView(context: Context) -> KeyView { KeyView() }
    func updateNSView(_ view: KeyView, context: Context) {
        view.changed = changed; view.stop = stop; view.suspend = suspend
        view.enabled = enabled
        if !enabled { view.keys.removeAll() }
    }
    static func dismantleNSView(_ view: KeyView, coordinator: ()) { view.removeMonitor() }
    final class KeyView: NSView {
        var enabled = false
        var keys = Set<UInt16>()
        var changed: (UnitreeMotion) -> Void = { _ in }
        var stop: () -> Void = {}
        var suspend: () -> Void = {}
        private var observers: [NSObjectProtocol] = []
        private var monitor: Any?
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            removeMonitor()
            guard let window else { return }
            let center = NotificationCenter.default
            observers = [
                center.addObserver(forName: NSWindow.didResignKeyNotification, object: window, queue: .main) { [weak self] _ in
                    MainActor.assumeIsolated { self?.keys.removeAll(); self?.suspend() }
                },
                center.addObserver(forName: NSApplication.didResignActiveNotification, object: nil, queue: .main) { [weak self] _ in
                    MainActor.assumeIsolated { self?.keys.removeAll(); self?.suspend() }
                }
            ]
            monitor = NSEvent.addLocalMonitorForEvents(matching: [.keyDown, .keyUp]) { [weak self] event in
                guard let self, self.enabled, self.window?.isKeyWindow == true, NSApp.isActive,
                      event.window === self.window else { return event }
                let codes: Set<UInt16> = [13, 1, 0, 2, 12, 14, 126, 125, 123, 124, 49]
                guard codes.contains(event.keyCode) else { return event }
                if event.type == .keyDown {
                    guard event.modifierFlags.intersection([.command, .option, .control]).isEmpty,
                          !(self.window?.firstResponder is NSTextView) else { return event }
                    if event.keyCode == 49 { self.keys.removeAll(); self.stop(); return nil }
                    guard !event.isARepeat else { return nil }
                    self.keys.insert(event.keyCode)
                } else { self.keys.remove(event.keyCode) }
                self.changed(.keys(self.keys))
                return nil
            }
        }
        func removeMonitor() {
            if let monitor { NSEvent.removeMonitor(monitor) }
            monitor = nil; keys.removeAll()
            observers.forEach(NotificationCenter.default.removeObserver)
            observers.removeAll()
        }
    }
}

struct UnitreeCameraView: View {
    let handle: UInt64
    @State private var decoder = H264AnnexBDecoder()
    @State private var lastFrame: Date?
    @State private var error: String?
    @State private var retry = 0
    var body: some View {
        ZStack {
            Color.black
            CameraDisplayView(decoder: decoder)
            TimelineView(.periodic(from: .now, by: 1)) { timeline in
                if let error {
                    VStack {
                        Text(error)
                        Button("Retry camera") { retry += 1 }
                    }.padding().background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8))
                } else if lastFrame == nil {
                    ProgressView("Opening camera…").padding().background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8))
                } else if timeline.date.timeIntervalSince(lastFrame!) > 3 {
                    Text("Camera feed interrupted").padding().background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8))
                }
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .accessibilityLabel("Go2 front camera")
        .task(id: retry) {
            decoder.reset(); lastFrame = nil; error = nil
            let started = Date()
            do {
                try await UnitreeClient.shared.startVideo(handle: handle)
                while !Task.isCancelled {
                    let bytes = try await UnitreeClient.shared.nextVideo(handle: handle)
                    try Task.checkCancellation()
                    if !bytes.isEmpty { decoder.feed(bytes); lastFrame = Date() }
                    if Date().timeIntervalSince(lastFrame ?? started) > 10 {
                        throw WendyError(message: "No camera frames received from Go2.")
                    }
                }
            } catch {
                if !Task.isCancelled { self.error = error.localizedDescription }
            }
            await UnitreeClient.shared.stopVideo(handle: handle)
        }
    }
}
#endif
