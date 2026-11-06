//go:build darwin || linux || windows

package commands

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

func TestSelectESP32InstallDevice(t *testing.T) {
	native := discovery.SerialPortInfo{Port: "/dev/cu.usbmodem101", Transport: discovery.SerialTransportNativeUSB}
	second := discovery.SerialPortInfo{Port: "/dev/cu.usbmodem201", Transport: discovery.SerialTransportNativeUSB, ConnectionTime: time.Now()}
	uart := discovery.SerialPortInfo{Port: "/dev/cu.usbserial-301", Transport: discovery.SerialTransportUARTBridge}
	pickerErr := errors.New("picker failed")
	for _, tt := range []struct {
		name        string
		devices     []discovery.SerialPortInfo
		previous    discovery.SerialPortInfo
		interactive bool
		choice      string
		pickErr     error
		want        discovery.SerialPortInfo
		wantPicker  bool
		wantErr     string
	}{
		{name: "no devices", wantErr: "no ESP32 serial port found"},
		{name: "single native", devices: []discovery.SerialPortInfo{native}, want: native},
		{name: "single UART", devices: []discovery.SerialPortInfo{uart}, interactive: true, want: uart},
		{name: "choose older native", devices: []discovery.SerialPortInfo{native, second}, interactive: true, choice: native.Port, want: native, wantPicker: true},
		{name: "choose second native", devices: []discovery.SerialPortInfo{native, second}, interactive: true, choice: second.Port, want: second, wantPicker: true},
		{name: "choose UART over native", devices: []discovery.SerialPortInfo{native, uart}, interactive: true, choice: uart.Port, want: uart, wantPicker: true},
		{name: "ambiguous noninteractive", devices: []discovery.SerialPortInfo{native, second}, wantErr: "interactive terminal"},
		{name: "cancel", devices: []discovery.SerialPortInfo{native, second}, interactive: true, pickErr: ErrUserCancelled, wantPicker: true, wantErr: ErrUserCancelled.Error()},
		{name: "picker error", devices: []discovery.SerialPortInfo{native, second}, interactive: true, pickErr: pickerErr, wantPicker: true, wantErr: pickerErr.Error()},
		{name: "invalid selection", devices: []discovery.SerialPortInfo{native, second}, interactive: true, choice: "/dev/other", wantPicker: true, wantErr: "invalid ESP32 device selection"},
		{name: "retry keeps chosen native", devices: []discovery.SerialPortInfo{second, native}, previous: native, interactive: true, want: native},
		{name: "retry keeps chosen UART", devices: []discovery.SerialPortInfo{native, uart}, previous: uart, interactive: true, want: uart},
		{name: "missing selection requires picker even with one port", devices: []discovery.SerialPortInfo{second}, previous: native, interactive: true, choice: second.Port, want: second, wantPicker: true},
		{name: "missing selection with multiple ports", devices: []discovery.SerialPortInfo{second, uart}, previous: native, interactive: true, choice: uart.Port, want: uart, wantPicker: true},
		{name: "missing selection noninteractive", devices: []discovery.SerialPortInfo{second}, previous: native, wantErr: "selection required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			picked := false
			got, err := selectESP32InstallDevice(tt.devices, tt.previous, tt.interactive, func(title string, items []tui.PickerItem) (string, error) {
				picked = true
				if tt.previous.Port != "" && !strings.Contains(title, tt.previous.Port) {
					t.Errorf("picker did not identify missing port: %q", title)
				}
				if len(items) != len(tt.devices) {
					t.Fatalf("picker has %d items, want %d", len(items), len(tt.devices))
				}
				for i, device := range tt.devices {
					if items[i].Name != device.Port || items[i].Value != device.Port {
						t.Errorf("candidate port missing from picker: %+v", items[i])
					}
					wantTransport := "Native USB"
					if device.Transport == discovery.SerialTransportUARTBridge {
						wantTransport = "USB-UART bridge"
					}
					if items[i].Description != wantTransport {
						t.Errorf("transport = %q, want %q", items[i].Description, wantTransport)
					}
				}
				return tt.choice, tt.pickErr
			})
			if picked != tt.wantPicker {
				t.Errorf("picker called = %v, want %v", picked, tt.wantPicker)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if tt.pickErr != nil && !errors.Is(err, tt.pickErr) {
					t.Errorf("picker error was not preserved: %v", err)
				}
				if got.Port != "" {
					t.Errorf("selected a port despite error: %+v", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("selection = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}
