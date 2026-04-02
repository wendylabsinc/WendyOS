//go:build linux

package ble

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Connection holds a single L2CAP file descriptor.
type Connection struct {
	fd   int
	addr [6]byte
}

// Connect parses the Bluetooth address and creates an AF_BLUETOOTH socket.
// It does NOT connect yet — that happens in OpenL2CAP.
func Connect(peripheralAddress string, _ int) (*Connection, error) {
	addr, err := parseBTAddr(peripheralAddress)
	if err != nil {
		return nil, fmt.Errorf("parse BT address: %w", err)
	}

	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET, unix.BTPROTO_L2CAP)
	if err != nil {
		return nil, fmt.Errorf("create L2CAP socket: %w", err)
	}

	return &Connection{fd: fd, addr: addr}, nil
}

// OpenL2CAP connects the socket to the remote device on the given PSM.
// Uses non-blocking connect + Poll to respect the timeout.
func (c *Connection) OpenL2CAP(psm uint16, timeoutSeconds int) error {
	if err := unix.SetNonblock(c.fd, true); err != nil {
		return fmt.Errorf("set nonblock: %w", err)
	}

	sa := &unix.SockaddrL2{
		PSM:  psm,
		Addr: c.addr,
	}

	err := unix.Connect(c.fd, sa)
	if err != nil && err != unix.EINPROGRESS {
		return fmt.Errorf("connect: %w", err)
	}

	if err == unix.EINPROGRESS {
		// Wait for connection to complete.
		pfd := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLOUT}}
		timeoutMs := timeoutSeconds * 1000
		n, pollErr := unix.Poll(pfd, timeoutMs)
		if pollErr != nil {
			return fmt.Errorf("poll connect: %w", pollErr)
		}
		if n == 0 {
			return fmt.Errorf("connect timeout after %ds", timeoutSeconds)
		}
		// Check if the connection actually succeeded.
		errno, sockErr := unix.GetsockoptInt(c.fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if sockErr != nil {
			return fmt.Errorf("getsockopt: %w", sockErr)
		}
		if errno != 0 {
			return fmt.Errorf("connect failed: %w", unix.Errno(errno))
		}
	}

	// Switch back to blocking mode for subsequent reads/writes.
	if err := unix.SetNonblock(c.fd, false); err != nil {
		return fmt.Errorf("set blocking: %w", err)
	}

	return nil
}

// L2CAPSend sends raw bytes over the L2CAP channel.
// Framing (length prefix) is handled by the caller (agent_client.go).
func (c *Connection) L2CAPSend(data []byte) error {
	_, err := unix.Write(c.fd, data)
	return err
}

// L2CAPRecv receives one L2CAP SDU with a timeout.
// Returns the raw bytes (including any framing added by the caller).
func (c *Connection) L2CAPRecv(timeoutSeconds int) ([]byte, error) {
	// Poll with timeout.
	pfd := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}
	timeoutMs := timeoutSeconds * 1000
	n, err := unix.Poll(pfd, timeoutMs)
	if err != nil {
		return nil, fmt.Errorf("poll recv: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("recv timeout after %ds", timeoutSeconds)
	}

	buf := make([]byte, 65536)
	nRead, err := unix.Read(c.fd, buf)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	result := make([]byte, nRead)
	copy(result, buf[:nRead])
	return result, nil
}

// Close closes the L2CAP socket.
func (c *Connection) Close() {
	unix.Close(c.fd) //nolint:errcheck
}

// ── GATT methods — not implemented on Linux CLI ───────────────────────────────

// DiscoverServices discovers all services and characteristics on the peripheral.
func (c *Connection) DiscoverServices(_ int) error {
	return fmt.Errorf("GATT not implemented on Linux")
}

// WriteCharacteristic writes data to a GATT characteristic with response.
func (c *Connection) WriteCharacteristic(_, _ string, _ []byte) error {
	return fmt.Errorf("GATT not implemented on Linux")
}

// WriteCharacteristicNoResponse writes data to a GATT characteristic without response.
func (c *Connection) WriteCharacteristicNoResponse(_, _ string, _ []byte) error {
	return fmt.Errorf("GATT not implemented on Linux")
}

// ReadCharacteristic reads data from a GATT characteristic.
func (c *Connection) ReadCharacteristic(_, _ string) ([]byte, error) {
	return nil, fmt.Errorf("GATT not implemented on Linux")
}

// Subscribe enables notifications for a GATT characteristic.
func (c *Connection) Subscribe(_, _ string) error {
	return fmt.Errorf("GATT not implemented on Linux")
}

// WaitNotification waits for a notification on a subscribed characteristic.
func (c *Connection) WaitNotification(_, _ string, _ int) ([]byte, error) {
	return nil, fmt.Errorf("GATT not implemented on Linux")
}

// HasService checks whether a specific service UUID was discovered.
func (c *Connection) HasService(_ string) bool { return false }

// ListServices returns a comma-separated string of discovered service UUIDs.
func (c *Connection) ListServices() string { return "" }

// ── Helpers ───────────────────────────────────────────────────────────────────

// parseBTAddr parses "AA:BB:CC:DD:EE:FF" into a [6]byte in LSB-first order
// (Bluetooth byte order: the first byte in the array is the least significant).
func parseBTAddr(s string) ([6]byte, error) {
	var addr [6]byte
	if len(s) != 17 {
		return addr, fmt.Errorf("invalid BT address length: %q", s)
	}
	for i, offset := range []int{15, 12, 9, 6, 3, 0} {
		var b byte
		if _, err := fmt.Sscanf(s[offset:offset+2], "%02X", &b); err != nil {
			return addr, fmt.Errorf("invalid BT address byte at position %d: %w", offset, err)
		}
		addr[i] = b
	}
	return addr, nil
}
