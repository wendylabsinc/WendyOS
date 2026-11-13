//go:build windows

package discovery

import (
	"strings"
	"testing"
)

func TestParseESP32SerialPortJSON_SingleEntry(t *testing.T) {
	in := `{"Name":"USB JTAG/serial debug unit (COM7)","PNPDeviceID":"USB\\VID_303A&PID_1001\\0123456789","Caption":"USB JTAG/serial debug unit (COM7)"}`
	got, err := parseESP32SerialPortJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "COM7" {
		t.Fatalf("got %q, want %q", got, "COM7")
	}
}

func TestParseESP32SerialPortJSON_ArrayPicksFirstWithCOM(t *testing.T) {
	in := `[{"Name":"USB JTAG/serial debug unit (COM3)","PNPDeviceID":"USB\\VID_303A&PID_1001\\A","Caption":"USB JTAG/serial debug unit (COM3)"},` +
		`{"Name":"Silicon Labs CP210x (COM12)","PNPDeviceID":"USB\\VID_10C4&PID_EA60\\B","Caption":"Silicon Labs CP210x (COM12)"}]`
	got, err := parseESP32SerialPortJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "COM3" {
		t.Fatalf("got %q, want %q", got, "COM3")
	}
}

func TestParseESP32SerialPortJSON_CP210x(t *testing.T) {
	in := `{"Name":"Silicon Labs CP210x (COM12)","PNPDeviceID":"USB\\VID_10C4&PID_EA60\\B","Caption":"Silicon Labs CP210x (COM12)"}`
	ports, err := parseESP32SerialPortsJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ports) != 1 || ports[0].Port != "COM12" || ports[0].Transport != SerialTransportUARTBridge {
		t.Fatalf("got %+v, want one CP210x UART bridge on COM12", ports)
	}
}

func TestParseESP32SerialPortJSON_IgnoresUnsupportedAdapter(t *testing.T) {
	in := `[{"Name":"Unrelated adapter (COM4)","PNPDeviceID":"USB\\VID_0403&PID_6001\\A","Caption":"Unrelated adapter (COM4)"},` +
		`{"Name":"USB JTAG/serial debug unit (COM7)","PNPDeviceID":"USB\\VID_303A&PID_1001\\B","Caption":"USB JTAG/serial debug unit (COM7)"}]`
	got, err := parseESP32SerialPortJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "COM7" {
		t.Fatalf("got %q, want %q", got, "COM7")
	}
}

func TestParseESP32SerialPortJSON_FallsBackToCaption(t *testing.T) {
	// Name field empty; COMN only present on Caption.
	in := `{"Name":"","PNPDeviceID":"USB\\VID_303A&PID_1001\\X","Caption":"USB JTAG/serial debug unit (COM21)"}`
	got, err := parseESP32SerialPortJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "COM21" {
		t.Fatalf("got %q, want %q", got, "COM21")
	}
}

func TestParseESP32SerialPortJSON_EmptyInput(t *testing.T) {
	_, err := parseESP32SerialPortJSON("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "no ESP32 serial port found") {
		t.Fatalf("got %v, want a 'no ESP32 serial port found' error", err)
	}
}

func TestParseESP32SerialPortJSON_NoCOMSuffix(t *testing.T) {
	in := `{"Name":"USB JTAG/serial debug unit","PNPDeviceID":"USB\\VID_303A&PID_1001\\Y","Caption":"USB JTAG/serial debug unit"}`
	_, err := parseESP32SerialPortJSON(in)
	if err == nil {
		t.Fatal("expected error when no COM port is present")
	}
}

func TestParseESP32SerialPortJSON_MalformedJSON(t *testing.T) {
	_, err := parseESP32SerialPortJSON(`{"Name":}`)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestDualCDCWindowsFiltersDiagnostics(t *testing.T) {
	input := `[{"Name":"Mosaico (COM3)","PNPDeviceID":"USB\\VID_303A&PID_4002&MI_00\\A"},{"Name":"Mosaico (COM4)","PNPDeviceID":"USB\\VID_303A&PID_4002&MI_02\\A"}]`
	ports, err := parseESP32SerialPortsJSON(input)
	if err != nil || len(ports) != 1 || ports[0].Port != "COM3" {
		t.Fatalf("got %+v, %v", ports, err)
	}
}
