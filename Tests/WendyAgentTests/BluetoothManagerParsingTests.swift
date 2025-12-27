import Testing

@testable import wendy_agent

@Suite("Bluetooth Manager Parsing")
struct BluetoothManagerParsingTests {
    @Test("Parse device addresses from bluetoothctl devices output")
    func testParseDeviceAddresses() {
        let output = """
            Device AA:BB:CC:DD:EE:FF Keyboard
            Device 11:22:33:44:55:66 Headphones
            Random line
            """

        let addresses = BluetoothManager.parseDeviceAddresses(from: output)

        #expect(addresses == ["AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66"])
    }

    @Test("Parse device info preferring Name over Alias")
    func testParseDeviceInfoUsesName() {
        let output = """
            Name: RealName
            Alias: AliasName
            Paired: yes
            Connected: no
            Trusted: yes
            Icon: audio-headset
            RSSI: -45
            """

        let info = BluetoothManager.parseDeviceInfo(from: output, address: "AA:BB:CC:DD:EE:FF")

        #expect(info?.name == "RealName")
        #expect(info?.address == "AA:BB:CC:DD:EE:FF")
        #expect(info?.paired == true)
        #expect(info?.connected == false)
        #expect(info?.trusted == true)
        #expect(info?.deviceType == "audio-headset")
        #expect(info?.rssi == -45)
    }

    @Test("Parse device info falls back to Alias when Name is missing")
    func testParseDeviceInfoUsesAlias() {
        let output = """
            Alias: AliasName
            Paired: no
            Connected: yes
            Trusted: no
            Icon: input-keyboard
            RSSI: not-a-number
            """

        let info = BluetoothManager.parseDeviceInfo(from: output, address: "11:22:33:44:55:66")

        #expect(info?.name == "AliasName")
        #expect(info?.connected == true)
        #expect(info?.paired == false)
        #expect(info?.trusted == false)
        #expect(info?.deviceType == "input-keyboard")
        #expect(info?.rssi == nil)
    }
}
