//go:build linux

package bleprovider

import (
	"context"
	"encoding/binary"
	"errors"
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
	want := []byte{0x01, 0x13, 0x20, 14, 0x00, 0x08, 12, 0, 12, 0, 0, 0, 0x20, 0x03, 0, 0, 16, 0}
	if string(got) != string(want) {
		t.Fatalf("command=%x, want %x", got, want)
	}
	// Pi 5's mesh ACL handle 64 must encode the same 30 ms / 5 ms request.
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
		completePacket(0x0800, 12, 0, 800, 0),
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
		{"unexpected interval", [][]byte{leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0), completePacket(0x0800, 36, 0, 800, 0)}, "differs from request"},
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
		statusPacket(0), completePacket(0x0800, 12, 0, 800, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("other ACL's failed status affected this link: interval=%s err=%v", interval, err)
	}
}

func TestLEUpdateDoesNotAcceptOldEventLengthCommand(t *testing.T) {
	old := leConnectionUpdateCommand(0x0800, meshIntervalUnits)
	binary.LittleEndian.PutUint16(old[14:16], 1)
	binary.LittleEndian.PutUint16(old[16:18], 1)
	_, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		old, statusPacket(0), completePacket(0x0800, 12, 0, 800, 0),
	))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("old 0.625 ms event-length command accepted: %v", err)
	}
	interval, err := waitForLEUpdate(context.Background(), 0x0800, meshIntervalUnits, eventReader(
		old, statusPacket(0), leConnectionUpdateCommand(0x0800, meshIntervalUnits), statusPacket(0),
		completePacket(0x0800, 12, 0, 800, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("10 ms event-length command not matched: interval=%s err=%v", interval, err)
	}
}

func TestSteadyIntervalRelaxUses45ms(t *testing.T) {
	got := leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits)
	// interval min/max at bytes 6:10 must be 36 (45 ms); supervision stays 800.
	if binary.LittleEndian.Uint16(got[6:8]) != 36 || binary.LittleEndian.Uint16(got[8:10]) != 36 {
		t.Fatalf("steady command intervals=%x, want 36/36", got[6:10])
	}
	if binary.LittleEndian.Uint16(got[12:14]) != meshTimeoutUnits {
		t.Fatalf("steady command changed supervision timeout: %x", got[12:14])
	}
	interval, err := waitForLEUpdate(context.Background(), 0x0800, meshSteadyIntervalUnits, eventReader(
		leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits),
		statusPacket(0),
		completePacket(0x0800, 36, 0, 800, 0),
	))
	if err != nil || interval != 45*time.Millisecond {
		t.Fatalf("steady update interval=%s err=%v, want 45ms", interval, err)
	}
	// A 15 ms completion must NOT satisfy a steady request (and vice versa):
	// the verifier binds to the requested units.
	if _, err := waitForLEUpdate(context.Background(), 0x0800, meshSteadyIntervalUnits, eventReader(
		leConnectionUpdateCommand(0x0800, meshSteadyIntervalUnits),
		statusPacket(0),
		completePacket(0x0800, 12, 0, 800, 0),
	)); err == nil {
		t.Fatal("15 ms completion accepted for steady request")
	}
}
