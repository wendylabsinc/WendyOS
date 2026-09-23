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
	got := leConnectionUpdateCommand(0x0800)
	want := []byte{0x01, 0x13, 0x20, 14, 0x00, 0x08, 12, 0, 12, 0, 0, 0, 0x20, 0x03, 0, 0, 16, 0}
	if string(got) != string(want) {
		t.Fatalf("command=%x, want %x", got, want)
	}
	// Pi 5's successful 15 ms / 10 ms event-length trial used handle 64.
	wantHandle64 := []byte{0x01, 0x13, 0x20, 14, 64, 0, 12, 0, 12, 0, 0, 0, 0x20, 0x03, 0, 0, 16, 0}
	if got := leConnectionUpdateCommand(64); string(got) != string(wantHandle64) {
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
	command := leConnectionUpdateCommand(0x0800)
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
	interval, err := waitForLEUpdate(context.Background(), 0x0800, eventReader(
		completePacket(0x0800, 12, 0, 800, 0), // stale completion before our command
		leConnectionUpdateCommand(0x0801),
		leConnectionUpdateCommand(0x0800),
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
		{"command rejected", [][]byte{leConnectionUpdateCommand(0x0800), statusPacket(0x0c)}, "command status"},
		{"update rejected", [][]byte{leConnectionUpdateCommand(0x0800), statusPacket(0), completePacket(0x0800, 12, 0, 800, 0x1f)}, "completed with status"},
		{"unexpected interval", [][]byte{leConnectionUpdateCommand(0x0800), statusPacket(0), completePacket(0x0800, 36, 0, 800, 0)}, "differs from request"},
		{"missing completion", [][]byte{leConnectionUpdateCommand(0x0800), statusPacket(0)}, "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := waitForLEUpdate(context.Background(), 0x0800, eventReader(tc.packets...))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v, want %q", err, tc.message)
			}
		})
	}
	if _, err := waitForLEUpdate(context.Background(), 0x0800, func(context.Context) ([]byte, error) {
		return nil, errors.New("socket closed")
	}); err == nil || err.Error() != "socket closed" {
		t.Fatalf("read error=%v", err)
	}
	interval, err = waitForLEUpdate(context.Background(), 0x0800, eventReader(
		leConnectionUpdateCommand(0x0801), leConnectionUpdateCommand(0x0800),
		statusPacket(0x0c), // another connection's rejected request
		statusPacket(0), completePacket(0x0800, 12, 0, 800, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("other ACL's failed status affected this link: interval=%s err=%v", interval, err)
	}
}

func TestLEUpdateDoesNotAcceptOldEventLengthCommand(t *testing.T) {
	old := leConnectionUpdateCommand(0x0800)
	binary.LittleEndian.PutUint16(old[14:16], 1)
	binary.LittleEndian.PutUint16(old[16:18], 1)
	_, err := waitForLEUpdate(context.Background(), 0x0800, eventReader(
		old, statusPacket(0), completePacket(0x0800, 12, 0, 800, 0),
	))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("old 0.625 ms event-length command accepted: %v", err)
	}
	interval, err := waitForLEUpdate(context.Background(), 0x0800, eventReader(
		old, statusPacket(0), leConnectionUpdateCommand(0x0800), statusPacket(0),
		completePacket(0x0800, 12, 0, 800, 0),
	))
	if err != nil || interval != 15*time.Millisecond {
		t.Fatalf("10 ms event-length command not matched: interval=%s err=%v", interval, err)
	}
}

func TestEarlyIntervalTuneRetriesOnlyAfterAuthentication(t *testing.T) {
	started := make(chan int, 2)
	reported := make(chan int, 2)
	calls := 0
	authenticated, finish, stop := startMeshIntervalTune(context.Background(), func(context.Context) (time.Duration, error) {
		calls++
		started <- calls
		if calls == 1 {
			return 0, errors.New("controller rejected early update")
		}
		return 15 * time.Millisecond, nil
	}, func(attempt int, interval time.Duration, err error) {
		if attempt == 1 && err == nil || attempt == 2 && (err != nil || interval != 15*time.Millisecond) {
			t.Errorf("attempt %d: interval=%s err=%v", attempt, interval, err)
		}
		reported <- attempt
	})
	if got := <-started; got != 1 {
		t.Fatalf("first attempt = %d", got)
	}
	<-reported
	select {
	case got := <-started:
		t.Fatalf("attempt %d started before TLS authentication", got)
	case <-time.After(20 * time.Millisecond):
	}
	authenticated()
	if got := <-started; got != 2 {
		t.Fatalf("retry = %d", got)
	}
	<-reported
	finish()
	stop()
	if calls != 2 {
		t.Fatalf("tune calls = %d, want 2", calls)
	}
}

func TestEarlyIntervalTuneSuccessNeedsNoRetry(t *testing.T) {
	calls := 0
	reported := make(chan struct{})
	authenticated, finish, stop := startMeshIntervalTune(context.Background(), func(context.Context) (time.Duration, error) {
		calls++
		return 15 * time.Millisecond, nil
	}, func(attempt int, interval time.Duration, err error) {
		if attempt != 1 || err != nil || interval != 15*time.Millisecond {
			t.Errorf("attempt %d: interval=%s err=%v", attempt, interval, err)
		}
		close(reported)
	})
	<-reported
	authenticated()
	finish()
	stop()
	if calls != 1 {
		t.Fatalf("tune calls = %d, want 1", calls)
	}
}

func TestEarlyIntervalTuneDoesNotDelayTLSAndJoinsBeforeClose(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	_, _, stop := startMeshIntervalTune(context.Background(), func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return 0, ctx.Err()
	}, func(int, time.Duration, error) { t.Fatal("cancelled update was reported") })
	<-started
	select {
	case <-finished:
		t.Fatal("controller update completed before TLS could start")
	default:
	}
	stop()
	select {
	case <-finished:
	default:
		t.Fatal("controller update survived CoC cleanup")
	}
}

func TestEarlyIntervalTuneYieldsBeforePeerStreamHelloDeadline(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	peerHelloDeadline := time.Now().Add(5 * time.Second)
	authenticated, finish, stop := startMeshIntervalTune(context.Background(), func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done() // controller never completes its interval update
		close(stopped)
		return 0, ctx.Err()
	}, func(int, time.Duration, error) { t.Fatal("cancelled update was reported") })
	defer stop()
	<-started
	authenticated() // both peers have completed TLS; inbound starts hello
	finish()        // outbound must finish tuning before AttachStream sends hello
	select {
	case <-stopped:
	default:
		t.Fatal("controller update survived stream admission")
	}
	if remaining := time.Until(peerHelloDeadline); remaining < 2*time.Second {
		t.Fatalf("only %s left for the peer's five-second stream hello", remaining)
	}
}

// A canceled context prevents hardware access while distinguishing the public
// request's identity gate from the subsequent I/O preparation.
func TestConnectionUpdateHandleBoundariesBeforeIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, handle := range []uint16{0, 0x0eff} {
		if _, err := requestMeshConnectionInterval(ctx, 0, handle); !errors.Is(err, context.Canceled) {
			t.Fatalf("valid handle %#x rejected before context check: %v", handle, err)
		}
		cmd := leConnectionUpdateCommand(handle)
		if got := binary.LittleEndian.Uint16(cmd[4:6]); got != handle {
			t.Fatalf("handle %#x encoded as %#x", handle, got)
		}
	}
	for _, handle := range []uint16{0x0f00, 0x0fff} {
		if _, err := requestMeshConnectionInterval(ctx, 0, handle); err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("invalid handle %#x passed identity gate: %v", handle, err)
		}
	}
}
