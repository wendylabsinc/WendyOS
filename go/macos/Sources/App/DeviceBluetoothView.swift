import SwiftUI

struct DeviceBluetoothPeripheral: Identifiable {
    let name, address, type: String
    let paired, connected: Bool
    var id: String { address }
    var title: String { name.isEmpty ? address : name }
}
extension GoClient {
    func bluetoothDevices(session: UInt64) async throws -> [DeviceBluetoothPeripheral] {
        try await perform {
            guard let pointer = WDBluetoothScan(session) else { throw WendyError(message: "Missing Bluetooth scan.") }
            defer { WDBluetoothFree(pointer) }
            try Self.check(pointer.pointee.status)
            return UnsafeBufferPointer(start: pointer.pointee.devices, count: pointer.pointee.count).map {
                DeviceBluetoothPeripheral(name: Self.string($0.name), address: Self.string($0.address), type: Self.string($0.device_type), paired: $0.paired != 0, connected: $0.connected != 0)
            }
        }
    }
    func bluetoothAction(session: UInt64, address: String, action: Int32) async throws {
        try await perform { try Self.withCString(address) { try Self.finish(WDBluetoothAction(session, $0, action)) } }
    }
}
struct DeviceBluetoothView: View {
    let session: UInt64
    let deviceName: String
    @State private var devices: [DeviceBluetoothPeripheral] = []
    @State private var scanning = false
    @State private var busy: String?
    @State private var error: String?
    @State private var forgetting: DeviceBluetoothPeripheral?
    var body: some View {
        Form {
            if let error { Section { FailureNote(message: error) } }
            Section {
                ForEach(devices) { device in
                    HStack(spacing: 12) {
                        Image(systemName: "antenna.radiowaves.left.and.right").foregroundStyle(.secondary).frame(width: 24)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(device.title).fontWeight(.medium)
                            Text(device.connected ? "Connected" : device.paired ? "Paired" : "Not paired")
                                .font(.caption).foregroundStyle(device.connected ? Color.green : .secondary)
                            if !device.name.isEmpty { Text(device.address).font(.caption2).foregroundStyle(.secondary) }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                        if busy == device.id { ProgressView() }
                        else {
                            Menu {
                                if device.connected {
                                    Button("Disconnect") { Task { await act(device, action: 2) } }
                                } else {
                                    Button("Connect") { Task { await act(device, action: 0) } }
                                    if !device.paired { Button("Pair and connect") { Task { await act(device, action: 1) } } }
                                }
                                if device.paired { Button("Forget device", role: .destructive) { forgetting = device } }
                            } label: { Image(systemName: "ellipsis").frame(width: 44, height: 44) }
                                .accessibilityLabel("Options for \(device.title)").disabled(scanning || busy != nil)
                        }
                    }.padding(.vertical, 4)
                }
                if devices.isEmpty && !scanning && error == nil { Text("No Bluetooth devices found.").foregroundStyle(.secondary) }
                if scanning { HStack { ProgressView(); Text("Scanning…").foregroundStyle(.secondary) } }
                Button("Scan again") { Task { await scan() } }.disabled(scanning || busy != nil)
            } header: { Text("Nearby devices") } footer: { Text("Bluetooth devices near \(deviceName). Put your accessory in discovery mode to find it.") }
        }.formStyle(.grouped).navigationTitle("Bluetooth")
            .task(id: session) { await scan() }
            .mobileRefreshable { await scan() }
            .confirmationDialog("Forget \(forgetting?.title ?? "device")?", isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }), titleVisibility: .visible) {
                if let device = forgetting { Button("Forget device", role: .destructive) { forgetting = nil; Task { await act(device, action: 3) } } }
            }
    }
    private func scan() async {
        guard !scanning && busy == nil else { return }
        scanning = true; error = nil
        defer { scanning = false }
        do { let result = try await GoClient.shared.bluetoothDevices(session: session); if !Task.isCancelled { devices = result } }
        catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }
    private func act(_ device: DeviceBluetoothPeripheral, action: Int32) async {
        busy = device.id; error = nil
        do { try await GoClient.shared.bluetoothAction(session: session, address: device.address, action: action); busy = nil; await scan() }
        catch { self.error = error.localizedDescription; busy = nil }
    }
}
