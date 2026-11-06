package discovery

import "testing"

func TestSupportedESP32SerialUSBIDClassifiesTransport(t *testing.T) {
	native, ok := supportedESP32SerialUSBID(0x303a, 0x1001)
	if !ok || native.transport != SerialTransportNativeUSB {
		t.Fatalf("native USB classification = (%+v, %v), want supported native interface", native, ok)
	}
	cdc, ok := supportedESP32SerialUSBID(0x303a, 0x4002)
	if !ok || cdc.transport != SerialTransportNativeUSB {
		t.Fatalf("TinyUSB CDC classification = (%+v, %v)", cdc, ok)
	}
	bridge, ok := supportedESP32SerialUSBID(0x10c4, 0xea60)
	if !ok || bridge.transport != SerialTransportUARTBridge {
		t.Fatalf("CP210x classification = (%+v, %v), want supported UART bridge", bridge, ok)
	}
	for _, unsupported := range [][2]uint16{{0x10c4, 0xea61}, {0x0403, 0x6001}} {
		if _, ok := supportedESP32SerialUSBID(unsupported[0], unsupported[1]); ok {
			t.Fatalf("unsupported USB ID %#04x:%#04x was accepted", unsupported[0], unsupported[1])
		}
	}
}

func TestParseUSBID(t *testing.T) {
	for _, value := range []string{"0xEA60", "ea60", " EA60\n"} {
		got, err := parseUSBID(value)
		if err != nil {
			t.Fatalf("parseUSBID(%q): %v", value, err)
		}
		if got != 0xea60 {
			t.Fatalf("parseUSBID(%q) = %#04x, want %#04x", value, got, 0xea60)
		}
	}
}

func TestDualCDCManagementInterfaceOnly(t *testing.T) {
	cdc, _ := supportedESP32SerialUSBID(0x303a, 0x4002)
	for _, iface := range []int{-1, 0, 1, 2, 3} {
		if got := serialUSBInterfaceAllowed(cdc, iface); got != (iface == 0) {
			t.Fatalf("interface %d accepted=%v", iface, got)
		}
	}
	s31, ok := supportedESP32SerialUSBID(0x303a, 0x0020)
	if !ok || s31.transport != SerialTransportNativeUSB {
		t.Fatal("S31 ROM not recognized")
	}
	rom, _ := supportedESP32SerialUSBID(0x303a, 0x1001)
	if !serialUSBInterfaceAllowed(rom, -1) {
		t.Fatal("ROM interface rejected")
	}
}
