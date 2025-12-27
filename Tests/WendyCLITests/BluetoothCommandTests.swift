import Testing

@testable import wendy

@Suite("Bluetooth CLI Device Info")
struct BluetoothCommandTests {
    @Test("Display name uses address when name is unknown")
    func testDisplayNameUnknown() {
        let info = BluetoothDeviceInfo(
            name: "Unknown",
            address: "AA:BB:CC:DD:EE:FF"
        )

        #expect(info.displayName == "AA:BB:CC:DD:EE:FF")
    }

    @Test("Display name includes name and address when available")
    func testDisplayNameUsesName() {
        let info = BluetoothDeviceInfo(
            name: "My Device",
            address: "11:22:33:44:55:66"
        )

        #expect(info.displayName == "My Device (11:22:33:44:55:66)")
    }

    @Test("Display name uses address when name is empty")
    func testDisplayNameEmptyName() {
        let info = BluetoothDeviceInfo(
            name: "",
            address: "01:23:45:67:89:AB"
        )

        #expect(info.displayName == "01:23:45:67:89:AB")
    }
}
