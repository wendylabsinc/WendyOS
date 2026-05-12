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
		`{"Name":"USB-SERIAL CH340 (COM12)","PNPDeviceID":"USB\\VID_303A&PID_1001\\B","Caption":"USB-SERIAL CH340 (COM12)"}]`
	got, err := parseESP32SerialPortJSON(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "COM3" {
		t.Fatalf("got %q, want %q", got, "COM3")
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
