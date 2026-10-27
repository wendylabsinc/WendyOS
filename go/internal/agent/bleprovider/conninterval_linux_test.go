//go:build linux

package bleprovider

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestAdapterAndACLIdentity(t *testing.T) {
	for _, tc := range []struct {
		path dbus.ObjectPath
		want int
	}{
		{"/org/bluez/hci0", 0}, {"/org/bluez/hci12", 12}, {"/org/bluez/hci0/dev_A", -1}, {"/org/bluez/hci-1", -1}, {"/other/hci0", -1},
	} {
		got, err := adapterHCIIndex(tc.path)
		if got != tc.want || (err != nil) != (tc.want < 0) {
			t.Errorf("adapterHCIIndex(%q)=(%d,%v), want %d", tc.path, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		info    []byte
		want    uint16
		invalid bool
	}{
		{[]byte{0x00, 0x08, 0, 0, 0, 0}, 0x0800, false},
		{[]byte{0x00}, 0, true}, {[]byte{0, 0}, 0, false},
		{[]byte{0xff, 0x0e}, 0x0eff, false}, {[]byte{0x00, 0x0f}, 0, true}, {[]byte{0xff, 0x0f}, 0, true},
	} {
		got, err := decodeACLHandle(tc.info)
		if got != tc.want || (err != nil) != tc.invalid {
			t.Errorf("decodeACLHandle(%x)=(%#x,%v), want %#x", tc.info, got, err, tc.want)
		}
	}
}

func TestLEConnectionUpdateEncoding(t *testing.T) {
	got := leConnectionUpdateCommand(0x0800, meshIntervalUnits)
	want := []byte{0x01, 0x13, 0x20, 14, 0x00, 0x08, 36, 0, 36, 0, 0, 0, 0x90, 0x01, 0, 0, 8, 0}
	if string(got) != string(want) {
		t.Fatalf("command=%x, want %x", got, want)
	}
	// Pi 5's ACL handle 64 must encode the same 45 ms / 5 ms request.
	wantHandle64 := append([]byte(nil), want...)
	wantHandle64[4], wantHandle64[5] = 64, 0
	if got := leConnectionUpdateCommand(64, meshIntervalUnits); string(got) != string(wantHandle64) {
		t.Fatalf("handle-64 command=%x, want %x", got, wantHandle64)
	}
	packet := statusPacket(0)
	monitor := make([]byte, 6+len(packet)-1)
	binary.LittleEndian.PutUint16(monitor[:2], hciMonitorEvent)
	binary.LittleEndian.PutUint16(monitor[2:4], 2)
	binary.LittleEndian.PutUint16(monitor[4:6], uint16(len(packet)-1))
	copy(monitor[6:], packet[1:])
	if got := monitorPacket(monitor, 2); string(got) != string(packet) {
		t.Fatalf("monitor event=%x, want %x", got, packet)
	}
	if got := monitorPacket(monitor, 1); got != nil {
		t.Fatalf("unrelated adapter accepted: %x", got)
	}
	monitor[4] = 0xff
	if got := monitorPacket(monitor, 2); got != nil {
		t.Fatalf("truncated monitor event accepted: %x", got)
	}
	command := leConnectionUpdateCommand(0x0800, meshIntervalUnits)
	monitor = make([]byte, 6+len(command)-1)
	binary.LittleEndian.PutUint16(monitor[:2], hciMonitorCommand)
	binary.LittleEndian.PutUint16(monitor[2:4], 2)
	binary.LittleEndian.PutUint16(monitor[4:6], uint16(len(command)-1))
	copy(monitor[6:], command[1:])
	if got := monitorPacket(monitor, 2); string(got) != string(command) {
		t.Fatalf("monitor command=%x, want %x", got, command)
	}
}

func statusPacket(status byte) []byte {
	return []byte{hciEventPacket, hciCommandStatus, 4, status, 1, 0x13, 0x20}
}

func completePacket(handle, interval, latency, timeout uint16, status byte) []byte {
	packet := []byte{hciEventPacket, hciLEMetaEvent, 10, leUpdateSubevent, status, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(packet[5:7], handle)
	binary.LittleEndian.PutUint16(packet[7:9], interval)
	binary.LittleEndian.PutUint16(packet[9:11], latency)
	binary.LittleEndian.PutUint16(packet[11:13], timeout)
	return packet
}

func eventReader(packets ...[]byte) func(context.Context) ([]byte, error) {
	index := 0
	return func(context.Context) ([]byte, error) {
		if index == len(packets) {
			return nil, context.DeadlineExceeded
		}
		packet := packets[index]
		index++
		return packet, nil
	}
}

func TestLEUpdateRequiresMatchingHandleAndCompletion(t *testing.T) {
	interval, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		completePacket(0x0800, 12, 0, 800, 0), // stale completion before our command
		leConnectionUpdateCommand(0x0801, meshIntervalUnits),
		leConnectionUpdateCommand(0x0800, meshIntervalUnits),
		statusPacket(0), // status for other ACL's command
		statusPacket(0),
		completePacket(0x0801, 12, 0, 800, 0), // unrelated ACL
		completePacket(0x0800, 12, 0, 400, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("interval=%s err=%v, want 15ms", interval, err)
	}
	for _, tc := range []struct {
		name    string
		packets [][]byte
		message string
	}{
		{"command rejected", [][]byte{leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0x0c)}, "command status"},
		{"update rejected", [][]byte{leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0), completePacket(0x0800, 12, 0, 800, 0x1f)}, "completed with status"},
		{"invalid interval", [][]byte{leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0), completePacket(0x0800, 0, 0, 800, 0)}, "invalid negotiated parameters"},
		{"missing completion", [][]byte{leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0)}, "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(tc.packets...))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v, want %q", err, tc.message)
			}
		})
	}
	if _, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, func(context.Context) ([]byte, error) {
		return nil, errors.New("socket closed")
	}); err == nil || err.Error() != "socket closed" {
		t.Fatalf("read error=%v", err)
	}
	interval, err = waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		leConnectionUpdateCommand(0x0801, meshIntervalUnits), leConnectionUpdateCommand(0x0800, meshIntervalUnits),
		statusPacket(0x0c), // another connection's rejected request
		statusPacket(0), completePacket(0x0800, 12, 0, 400, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("other ACL's failed status affected this link: interval=%s err=%v", interval, err)
	}
}

func TestLEUpdateUsesValidNegotiatedParameters(t *testing.T) {
	// The controller reports the parameters actually negotiated with the peer;
	// a successful procedure need not produce the exact requested values.
	for _, tc := range []struct {
		interval, latency, timeout uint16
		valid                      bool
	}{
		{36, 0, 400, true},
		{24, 4, 100, true},
		{6, 0, 10, true},
		{3200, 0, 3200, true},
		{5, 0, 800, false},
		{3201, 0, 800, false},
		{12, 500, 3200, false},
		{12, 0, 9, false},
		{12, 0, 3201, false},
		{40, 0, 10, false}, // timeout must exceed two connection intervals
		{80, 4, 100, false},
	} {
		t.Run(fmt.Sprintf("%d-%d-%d", tc.interval, tc.latency, tc.timeout), func(t *testing.T) {
			interval, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
				leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0),
				completePacket(0x0800, tc.interval, tc.latency, tc.timeout, 0),
			))
			if tc.valid {
				if err != nil || interval != time.Duration(tc.interval)*1250*time.Microsecond {
					t.Fatalf("negotiated interval=%s err=%v", interval, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "invalid negotiated parameters") {
				t.Fatalf("invalid success accepted: interval=%s err=%v", interval, err)
			}
		})
	}
}

func TestLEUpdateIgnoresMaximumLengthEvents(t *testing.T) {
	// The HCI monitor also delivers unrelated events with up to 255 parameter
	// bytes, such as advertising reports. They must be skipped, not crash.
	for _, length := range []int{252, 253, 254, 255} {
		packet := make([]byte, 3+length)
		packet[0], packet[1], packet[2] = hciEventPacket, hciLEMetaEvent, byte(length)
		if event := parseLEUpdateEvent(packet, 0x0800); event != (updateEvent{}) {
			t.Fatalf("%d-byte event parsed as %+v", length, event)
		}
		interval, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
			leConnectionUpdateCommand(0x0800, meshIntervalUnits), packet, statusPacket(0),
			completePacket(0x0800, 12, 0, 400, 0),
		))
		if err != nil || interval != 15*time.Millisecond {
			t.Fatalf("%d-byte event: interval=%s err=%v", length, interval, err)
		}
	}
}

func TestLEUpdateDoesNotAcceptOldEventLengthCommand(t *testing.T) {
	old := leConnectionUpdateCommand(0x0800, meshIntervalUnits)
	binary.LittleEndian.PutUint16(old[14:16], 1)
	binary.LittleEndian.PutUint16(old[16:18], 1)
	_, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		old, statusPacket(0), completePacket(0x0800, 12, 0, 400, 0),
	))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("old 0.625 ms event-length command accepted: %v", err)
	}
	interval, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		old, statusPacket(0), leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0),
		completePacket(0x0800, 12, 0, 400, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("5 ms event-length command not matched: interval=%s err=%v", interval, err)
	}
}

func TestSteadyIntervalRelaxUses45ms(t *testing.T) {
	got := leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits)
	// Interval min/max at bytes 6:10 must be 36 (45 ms); supervision stays 4 s.
	if binary.LittleEndian.Uint16(got[6:8]) != 36 || binary.LittleEndian.Uint16(got[8:10]) != 36 {
		t.Fatalf("steady command intervals=%x, want 36/36", got[6:10])
	}
	if binary.LittleEndian.Uint16(got[12:14]) != meshTimeoutUnits {
		t.Fatalf("steady command changed supervision timeout: %x", got[12:14])
	}
	interval, err := waitForLEUpdate(context.Background(), 0x0800, meshSteadyIntervalUnits, eventReader(
		leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits),
		statusPacket(0),
		completePacket(0x0800, 36, 0, 400, 0),
	))
	if err != nil || interval != 45*time.Millisecond {
		t.Fatalf("steady update interval=%s err=%v, want 45ms", interval, err)
	}
	// The submitted command must request the steady interval, but the
	// successful completion reports whatever the peer actually negotiated.
	interval, err = waitForLEUpdate(context.Background(), 0x0800, meshSteadyIntervalUnits, eventReader(
		leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits),
		statusPacket(0),
		completePacket(0x0800, 12, 0, 400, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("negotiated interval=%s err=%v, want 15ms", interval, err)
	}
	_, err = waitForLEUpdate(context.Background(), 0x0800, meshSteadyIntervalUnits, eventReader(
		// Keep the negative request distinct even when initial and steady
		// traffic use the same preferred interval.
		leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits+1),
		statusPacket(0),
		completePacket(0x0800, 36, 0, 400, 0),
	))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another request's completion accepted: %v", err)
	}
}
