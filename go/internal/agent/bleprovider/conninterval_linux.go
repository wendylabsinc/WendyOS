//go:build linux

package bleprovider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	// HCI LE Connection Update uses 1.25 ms interval, 10 ms timeout, and
	// 0.625 ms connection-event length units. Request short events to leave
	// headroom for concurrent CoCs, discovery and advertising. Event lengths
	// are controller scheduling hints, not guaranteed airtime allocations.
	// Use the established interval during authentication too: three centrals
	// can authenticate to one peripheral concurrently, and a 15 ms interval
	// would triple their anchor demand during the certificate transfer.
	meshIntervalUnits       = 36 // 45 ms
	meshSteadyIntervalUnits = 36 // 45 ms
	// A multi-peer adapter can miss several connection events while it scans,
	// advertises, and services another CoC. The former 2 s timeout caused
	// authenticated Wendy links to drop with HCI reason 0x08 under contention.
	meshTimeoutUnits        = 400 // 4 s supervision timeout
	meshMinEventLengthUnits = 0
	// Recommend shorter events than the former 10 ms hint so one bulk link
	// does not request most of the airtime available to other operations.
	meshMaxEventLengthUnits = 8 // 5 ms
	leUpdateOpcode          = 0x2013
	leUpdateSubevent        = 0x03
	hciEventPacket          = 0x04
	hciCommandPacket        = 0x01
	hciCommandStatus        = 0x0f
	hciLEMetaEvent          = 0x3e
	l2capConnInfo           = 0x02
	hciMonitorCommand       = 0x0002
	hciMonitorEvent         = 0x0003
)

func adapterHCIIndex(path dbus.ObjectPath) (int, error) {
	name := strings.TrimPrefix(string(path), "/org/bluez/hci")
	if name == string(path) || name == "" || strings.Contains(name, "/") {
		return -1, fmt.Errorf("invalid BlueZ adapter path %q", path)
	}
	index, err := strconv.ParseUint(name, 10, 16)
	if err != nil {
		return -1, fmt.Errorf("invalid BlueZ adapter path %q: %w", path, err)
	}
	return int(index), nil
}

// The connected CoC socket exposes its own ACL handle. Using its handle and
// the selected adapter avoids changing the controller defaults or unrelated
// Bluetooth connections. Linux l2cap_conninfo is 2-byte handle + 3-byte class
// (padded to six bytes); only the handle is relevant for LE.
func l2capACLHandle(fd int) (uint16, error) {
	var info [6]byte
	length := uint32(len(info))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.SOL_L2CAP), l2capConnInfo,
		uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 {
		return 0, errno
	}
	if length > uint32(len(info)) {
		return 0, fmt.Errorf("L2CAP_CONNINFO returned oversized length %d", length)
	}
	return decodeACLHandle(info[:length])
}

func decodeACLHandle(info []byte) (uint16, error) {
	if len(info) < 2 {
		return 0, fmt.Errorf("L2CAP_CONNINFO returned %d bytes", len(info))
	}
	handle := binary.LittleEndian.Uint16(info[:2])
	if handle > 0x0eff {
		return 0, fmt.Errorf("invalid ACL handle %#x", handle)
	}
	return handle, nil
}

func leConnectionUpdateCommand(handle uint16, intervalUnits uint16) []byte {
	command := []byte{hciCommandPacket, 0x13, 0x20, 14, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(command[4:6], handle)
	binary.LittleEndian.PutUint16(command[6:8], intervalUnits)
	binary.LittleEndian.PutUint16(command[8:10], intervalUnits)
	binary.LittleEndian.PutUint16(command[10:12], 0) // peripheral latency
	binary.LittleEndian.PutUint16(command[12:14], meshTimeoutUnits)
	binary.LittleEndian.PutUint16(command[14:16], meshMinEventLengthUnits)
	binary.LittleEndian.PutUint16(command[16:18], meshMaxEventLengthUnits)
	return command
}

type updateEvent struct {
	commandStatus bool
	complete      bool
	status        byte
	interval      uint16
	latency       uint16
	timeout       uint16
}

func leUpdateCommandHandle(packet []byte) (uint16, bool) {
	if len(packet) != 18 || packet[0] != hciCommandPacket || binary.LittleEndian.Uint16(packet[1:3]) != leUpdateOpcode || packet[3] != 14 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(packet[4:6]), true
}

func parseLEUpdateEvent(packet []byte, handle uint16) updateEvent {
	if len(packet) < 3 || packet[0] != hciEventPacket || int(packet[2]) > len(packet)-3 {
		return updateEvent{}
	}
	// Widen before adding: a byte sum wraps for 253-255 byte parameters.
	params := packet[3 : 3+int(packet[2])]
	switch packet[1] {
	case hciCommandStatus:
		if len(params) >= 4 && binary.LittleEndian.Uint16(params[2:4]) == leUpdateOpcode {
			return updateEvent{commandStatus: true, status: params[0]}
		}
	case hciLEMetaEvent:
		if len(params) >= 4 && params[0] == leUpdateSubevent && binary.LittleEndian.Uint16(params[2:4]) == handle {
			// Core Vol4 PartE 4.5 permits failed completion events to omit
			// optional values after the handle. Those values are not valid
			// connection parameters when Status is nonzero.
			if params[1] != 0 {
				return updateEvent{complete: true, status: params[1]}
			}
			if len(params) < 10 {
				return updateEvent{}
			}
			return updateEvent{complete: true, status: params[1], interval: binary.LittleEndian.Uint16(params[4:6]),
				latency: binary.LittleEndian.Uint16(params[6:8]), timeout: binary.LittleEndian.Uint16(params[8:10])}
		}
	}
	return updateEvent{}
}

func waitForLEUpdate(ctx context.Context, handle uint16, expectedUnits uint16, read func(context.Context) ([]byte, error)) (time.Duration, error) {
	accepted := false
	// Command Status carries an opcode but no ACL handle. HCI commands and
	// statuses are ordered on one controller, so pair each observed monitor
	// command with the next status for this opcode. Other mesh links (and
	// BlueZ) may request updates on the same adapter concurrently.
	type pendingCommand struct {
		handle   uint16
		expected bool
	}
	var pending []pendingCommand
	for {
		packet, err := read(ctx)
		if err != nil {
			return 0, err
		}
		if commandHandle, ok := leUpdateCommandHandle(packet); ok {
			if len(pending) >= 32 {
				return 0, errors.New("too many concurrent LE connection updates")
			}
			pending = append(pending, pendingCommand{commandHandle, bytes.Equal(packet, leConnectionUpdateCommand(handle, expectedUnits))})
			continue
		}
		event := parseLEUpdateEvent(packet, handle)
		if event.commandStatus {
			if len(pending) == 0 {
				continue // command preceded monitor subscription
			}
			command := pending[0]
			pending = pending[1:]
			if !command.expected {
				continue
			}
			if event.status != 0 {
				return 0, fmt.Errorf("LE connection update command status %#x", event.status)
			}
			accepted = true
		}
		if !event.complete || !accepted {
			continue
		}
		if event.status != 0 {
			return 0, &leUpdateCompletedError{status: event.status}
		}
		// Core Vol4 PartE 7.7.65.3 reports the parameters actually negotiated,
		// which may differ from the request. Validate the LE ranges and the
		// supervision constraint instead of requiring our preferred settings.
		// In 10ms/1.25ms units, timeout > 2*(latency+1)*interval becomes
		// 4*timeout > (latency+1)*interval; widen before multiplying.
		if event.interval < 6 || event.interval > 3200 || event.latency > 499 ||
			event.timeout < 10 || event.timeout > 3200 ||
			4*uint32(event.timeout) <= (uint32(event.latency)+1)*uint32(event.interval) {
			return 0, fmt.Errorf("LE connection update has invalid negotiated parameters: interval=%d latency=%d timeout=%d", event.interval, event.latency, event.timeout)
		}
		return time.Duration(event.interval) * 1250 * time.Microsecond, nil
	}
}

// A matched failed completion ends this submitted procedure. It differs from
// a missing completion or a rejected command that may leave another procedure
// active. The caller can retain the already established connection parameters.
type leUpdateCompletedError struct{ status byte }

func (e *leUpdateCompletedError) Error() string {
	return fmt.Sprintf("LE connection update completed with status %#x", e.status)
}

func monitorPacket(packet []byte, hciIndex int) []byte {
	// Linux hci_mon_hdr is opcode/index/len, each little-endian uint16.
	// Monitor payloads omit the H4 packet type byte. Keep both outgoing
	// commands and events so Command Status can be assigned to a handle.
	if len(packet) < 6 || int(binary.LittleEndian.Uint16(packet[2:4])) != hciIndex {
		return nil
	}
	length := int(binary.LittleEndian.Uint16(packet[4:6]))
	if length < 3 || length > len(packet)-6 {
		return nil
	}
	switch binary.LittleEndian.Uint16(packet[:2]) {
	case hciMonitorCommand:
		return append([]byte{hciCommandPacket}, packet[6:6+length]...)
	case hciMonitorEvent:
		return append([]byte{hciEventPacket}, packet[6:6+length]...)
	default:
		return nil
	}
}

func meshACLHandle(conn net.Conn) (uint16, error) {
	packet, ok := conn.(*packetConn)
	if !ok {
		return 0, errors.New("BLE link is not a CoC socket")
	}
	return l2capACLHandle(packet.fd)
}

// A failure before submission leaves no update from this request in flight.
// A matched failed completion has its own terminal error type; missing replies
// and command rejection are kept separate because their outcome may be unclear.
type leUpdateUnavailableError struct{ err error }

func (e *leUpdateUnavailableError) Error() string {
	return "LE update unavailable before submission: " + e.err.Error()
}
func (e *leUpdateUnavailableError) Unwrap() error { return e.err }

type leUpdateIO struct {
	send  func([]byte) (int, error)
	read  func(context.Context) ([]byte, error)
	close func()
}

func requestMeshConnectionInterval(ctx context.Context, hciIndex int, handle uint16) (time.Duration, error) {
	if hciIndex < 0 || hciIndex > 0xffff || handle > 0x0eff {
		return 0, &leUpdateUnavailableError{err: errors.New("BLE link has no controller/ACL identity")}
	}
	return submitMeshIntervalUpdate(ctx, handle, meshIntervalUnits, func(ctx context.Context) (*leUpdateIO, error) {
		return prepareMeshIntervalIO(ctx, hciIndex)
	})
}

// requestSteadyConnectionInterval relaxes an established link to the
// steady-state bulk interval. Failures are best-effort (the link works at
// the handshake interval); only submission-boundary errors are distinguished
// so callers can log accurately.
func requestSteadyConnectionInterval(ctx context.Context, hciIndex int, handle uint16) (time.Duration, error) {
	if hciIndex < 0 || hciIndex > 0xffff || handle > 0x0eff {
		return 0, &leUpdateUnavailableError{err: errors.New("BLE link has no controller/ACL identity")}
	}
	return submitMeshIntervalUpdate(ctx, handle, meshSteadyIntervalUnits, func(ctx context.Context) (*leUpdateIO, error) {
		return prepareMeshIntervalIO(ctx, hciIndex)
	})
}

func submitMeshIntervalUpdate(ctx context.Context, handle uint16, intervalUnits uint16, prepare func(context.Context) (*leUpdateIO, error)) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, &leUpdateUnavailableError{err: err}
	}
	io, err := prepare(ctx)
	if err != nil {
		return 0, &leUpdateUnavailableError{err: err}
	}
	defer io.close()
	if err := ctx.Err(); err != nil {
		return 0, &leUpdateUnavailableError{err: err}
	}
	command := leConnectionUpdateCommand(handle, intervalUnits)
	// Once send is attempted, even a write error is conservatively uncertain.
	// Never convert this or a monitor error into an untuned TLS fallback.
	if n, err := io.send(command); err != nil {
		return 0, fmt.Errorf("sending LE connection update: %w", err)
	} else if n != len(command) {
		return 0, fmt.Errorf("short LE connection update command: %d/%d", n, len(command))
	}
	return waitForLEUpdate(ctx, handle, intervalUnits, io.read)
}

func prepareMeshIntervalIO(ctx context.Context, hciIndex int) (*leUpdateIO, error) {
	monitorFD, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("HCI monitor socket: %w", err)
	}
	fd := -1
	ready := false
	closeIO := func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		_ = unix.Close(monitorFD)
	}
	defer func() {
		if !ready {
			closeIO()
		}
	}()
	if err := unix.Bind(monitorFD, &unix.SockaddrHCI{Dev: 0xffff, Channel: unix.HCI_CHANNEL_MONITOR}); err != nil {
		return nil, fmt.Errorf("binding HCI monitor: %w", err)
	}
	fd, err = unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("HCI raw socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: uint16(hciIndex), Channel: unix.HCI_CHANNEL_RAW}); err != nil {
		return nil, fmt.Errorf("binding HCI adapter %d: %w", hciIndex, err)
	}
	if err := pollFD(ctx, fd, unix.POLLOUT, 0); err != nil {
		return nil, err
	}
	ready = true
	return &leUpdateIO{
		close: closeIO,
		send:  func(command []byte) (int, error) { return unix.Write(fd, command) },
		read: func(ctx context.Context) ([]byte, error) {
			for {
				if err := pollFD(ctx, monitorFD, unix.POLLIN, 0); err != nil {
					return nil, err
				}
				var buffer [512]byte
				n, err := unix.Read(monitorFD, buffer[:])
				if err == unix.EAGAIN || err == unix.EINTR {
					continue
				}
				if err != nil {
					return nil, err
				}
				if event := monitorPacket(buffer[:n], hciIndex); event != nil {
					return event, nil
				}
			}
		},
	}, nil
}
