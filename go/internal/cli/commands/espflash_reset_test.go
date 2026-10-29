//go:build darwin || linux || windows

package commands

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"go.bug.st/serial"
)

type modemLineChange struct {
	line  string
	value bool
}

type resetTestPort struct {
	changes           []modemLineChange
	inputBufferResets int
	readCalled        bool
}

type scriptedReadPort struct {
	resetTestPort
	input []byte
}

func (p *scriptedReadPort) Read(data []byte) (int, error) {
	if len(p.input) == 0 {
		return 0, nil
	}
	data[0] = p.input[0]
	p.input = p.input[1:]
	return 1, nil
}

func (p *resetTestPort) SetDTR(value bool) error {
	p.changes = append(p.changes, modemLineChange{line: "DTR", value: value})
	return nil
}

func (p *resetTestPort) SetRTS(value bool) error {
	p.changes = append(p.changes, modemLineChange{line: "RTS", value: value})
	return nil
}

func (p *resetTestPort) ResetInputBuffer() error {
	p.inputBufferResets++
	return nil
}

func (p *resetTestPort) Read([]byte) (int, error) {
	p.readCalled = true
	return 0, nil
}

func (p *resetTestPort) SetMode(*serial.Mode) error                           { return nil }
func (p *resetTestPort) Write(data []byte) (int, error)                       { return len(data), nil }
func (p *resetTestPort) Drain() error                                         { return nil }
func (p *resetTestPort) ResetOutputBuffer() error                             { return nil }
func (p *resetTestPort) GetModemStatusBits() (*serial.ModemStatusBits, error) { return nil, nil }
func (p *resetTestPort) SetReadTimeout(time.Duration) error                   { return nil }
func (p *resetTestPort) Close() error                                         { return nil }
func (p *resetTestPort) Break(time.Duration) error                            { return nil }

func TestResetESP32ViaUARTBridgeEntersBootloader(t *testing.T) {
	port := &resetTestPort{}
	if err := resetESP32(port, true, discovery.SerialTransportUARTBridge); err != nil {
		t.Fatal(err)
	}
	want := []modemLineChange{
		{line: "DTR", value: false},
		{line: "RTS", value: true},
		{line: "DTR", value: true},
		{line: "RTS", value: false},
		{line: "DTR", value: false},
	}
	assertModemLineChanges(t, port.changes, want)
}

func TestResetESP32ViaUARTBridgeHardResets(t *testing.T) {
	port := &resetTestPort{}
	if err := resetESP32(port, false, discovery.SerialTransportUARTBridge); err != nil {
		t.Fatal(err)
	}
	want := []modemLineChange{
		{line: "RTS", value: true},
		{line: "RTS", value: false},
	}
	assertModemLineChanges(t, port.changes, want)
}

func TestResetESP32ViaNativeUSBUsesUSBJTAGSequence(t *testing.T) {
	port := &resetTestPort{}
	if err := resetESP32(port, true, discovery.SerialTransportNativeUSB); err != nil {
		t.Fatal(err)
	}
	want := []modemLineChange{
		{line: "RTS", value: false},
		{line: "DTR", value: false},
		{line: "DTR", value: true},
		{line: "RTS", value: false},
		{line: "RTS", value: true},
		{line: "DTR", value: false},
		{line: "RTS", value: true},
		{line: "DTR", value: false},
		{line: "RTS", value: false},
	}
	assertModemLineChanges(t, port.changes, want)
}

func TestESPFlasherDrainPurgesInsteadOfReadingUntilQuiet(t *testing.T) {
	port := &resetTestPort{}
	flasher := &espFlasher{port: port}
	if err := flasher.drain(); err != nil {
		t.Fatal(err)
	}
	if port.inputBufferResets != 1 {
		t.Fatalf("input buffer reset count = %d, want 1", port.inputBufferResets)
	}
	if port.readCalled {
		t.Fatal("drain read from the serial stream; continuous application output could hang it")
	}
}

func TestSlipDecodePreservesFrameAfterConsecutiveDelimiters(t *testing.T) {
	port := &scriptedReadPort{input: []byte{
		0x55, slipEnd, slipEnd,
		0x01, espCmdChangeBaud, 0x04, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		slipEnd,
	}}
	flasher := &espFlasher{port: port, readTimeout: time.Second}
	got, err := flasher.slipDecode()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x01, espCmdChangeBaud, 0x04, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	if len(got) != len(want) {
		t.Fatalf("decoded frame = %x, want %x", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decoded frame = %x, want %x", got, want)
		}
	}
}

func TestChangeBaudCommandDataUsesROMBootloaderLayout(t *testing.T) {
	data := changeBaudCommandData(flashBaudRate)
	if got := binary.LittleEndian.Uint32(data[0:4]); got != flashBaudRate {
		t.Fatalf("new baud = %d, want %d", got, flashBaudRate)
	}
	if got := binary.LittleEndian.Uint32(data[4:8]); got != 0 {
		t.Fatalf("ROM bootloader old-baud argument = %d, want 0", got)
	}
}

func TestValidateDetectedChipRejectsWrongFirmwareTarget(t *testing.T) {
	if err := validateDetectedChip(chipESP32C6, chipESP32C6); err != nil {
		t.Fatalf("matching chip rejected: %v", err)
	}
	err := validateDetectedChip(chipESP32C6, chipESP32C5)
	if err == nil {
		t.Fatal("ESP32-C5 was accepted for ESP32-C6 firmware")
	}
}

func TestChipModelForEveryWendyLiteTarget(t *testing.T) {
	tests := []struct {
		target string
		chip   chipModel
		name   string
	}{
		{target: "esp32c5", chip: chipESP32C5, name: "ESP32-C5"},
		{target: "esp32c6", chip: chipESP32C6, name: "ESP32-C6"},
		{target: "esp32c61", chip: chipESP32C61, name: "ESP32-C61"},
		{target: "esp32p4", chip: chipESP32P4, name: "ESP32-P4"},
		{target: "esp32s3", chip: chipESP32S3, name: "ESP32-S3"},
		{target: "esp32s31", chip: chipESP32S31, name: "ESP32-S31"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			got, err := chipModelForTarget(tt.target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.chip {
				t.Fatalf("chipModelForTarget(%q) = %v, want %v", tt.target, got, tt.chip)
			}
			if gotName := chipModelName(got); gotName != tt.name {
				t.Fatalf("chipModelName(%v) = %q, want %q", got, gotName, tt.name)
			}
		})
	}
}

func TestFlashBeginUsesScaledTimeoutForDriverAndReader(t *testing.T) {
	port := &fakeSerialPort{}
	flasher := &espFlasher{port: port}
	const imageSize = uint32(15_000_000)
	_ = flasher.flashBegin(imageSize, 1, espFlashBlockSize, 0)
	if got, want := flasher.readTimeout, eraseTimeout(imageSize); got != want {
		t.Fatalf("read timeout = %s, want %s", got, want)
	}
}

func assertModemLineChanges(t *testing.T, got, want []modemLineChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("line changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line change %d = %+v, want %+v (all got: %+v)", i, got[i], want[i], got)
		}
	}
}
