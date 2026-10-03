//go:build linux

package bleprovider

import (
	"context"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	mgmtDisconnectOpcode   = 0x0014
	mgmtCommandComplete    = 0x0001
	mgmtCommandStatus      = 0x0002
	mgmtStatusSuccess      = 0x00
	mgmtStatusNotConnected = 0x02
	mgmtStatusDisconnected = 0x0e
	mgmtIndexNone          = 0xffff
)

// disconnectLEPeer asks the Linux Bluetooth management layer to abort only
// the specified LE peer on the selected adapter. Unlike raw HCI LE Create
// Connection Cancel, the management command first resolves the peer's
// hci_conn and also clears a queued create command. The caller must use this
// only after its own timed-out CoC socket has closed: the management command
// will also terminate a late-established ACL to this same peer.
func disconnectLEPeer(ctx context.Context, hciIndex int, address, addressType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	packet, err := encodeMgmtDisconnect(hciIndex, address, addressType)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return fmt.Errorf("open Bluetooth management socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: mgmtIndexNone, Channel: unix.HCI_CHANNEL_CONTROL}); err != nil {
		return fmt.Errorf("bind Bluetooth management socket: %w", err)
	}
	for {
		if err := pollFD(ctx, fd, unix.POLLOUT, 0); err != nil {
			return fmt.Errorf("wait to send Bluetooth management disconnect: %w", err)
		}
		n, err := unix.Write(fd, packet)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return fmt.Errorf("send Bluetooth management disconnect: %w", err)
		}
		if n != len(packet) {
			return fmt.Errorf("short Bluetooth management disconnect write: %d/%d", n, len(packet))
		}
		break
	}
	var reply [4096]byte
	for {
		if err := pollFD(ctx, fd, unix.POLLIN, 0); err != nil {
			return fmt.Errorf("wait for Bluetooth management disconnect: %w", err)
		}
		n, err := unix.Read(fd, reply[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Bluetooth management disconnect: %w", err)
		}
		matched, status, err := decodeMgmtDisconnectReply(reply[:n], uint16(hciIndex))
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		// On Linux 6.8, DISCONNECTED can mean socket close has already
		// removed the pending hci_conn before this command runs. It is a
		// settled peer state, not proof this command performed the abort.
		// NOT_CONNECTED covers the same race on other kernel versions.
		if mgmtDisconnectSettled(status) {
			return nil
		}
		return fmt.Errorf("Bluetooth management disconnect status 0x%02x", status)
	}
}

func mgmtDisconnectSettled(status byte) bool {
	return status == mgmtStatusSuccess || status == mgmtStatusNotConnected || status == mgmtStatusDisconnected
}

func encodeMgmtDisconnect(hciIndex int, address, addressType string) ([]byte, error) {
	if hciIndex < 0 || hciIndex >= mgmtIndexNone {
		return nil, fmt.Errorf("invalid Bluetooth adapter index %d", hciIndex)
	}
	_, mac, err := parseAddress(address, addressType, 0x81)
	if err != nil {
		return nil, err
	}
	packet := make([]byte, 13) // management header (6) + address and type (7)
	binary.LittleEndian.PutUint16(packet[0:2], mgmtDisconnectOpcode)
	binary.LittleEndian.PutUint16(packet[2:4], uint16(hciIndex))
	binary.LittleEndian.PutUint16(packet[4:6], 7)
	for i := 0; i < len(mac); i++ {
		packet[6+i] = mac[len(mac)-1-i]
	}
	packet[12] = map[string]byte{"public": btAddrLEPublic, "random": btAddrLERandom}[addressType]
	return packet, nil
}

func decodeMgmtDisconnectReply(packet []byte, hciIndex uint16) (bool, byte, error) {
	if len(packet) < 6 {
		return false, 0, fmt.Errorf("short Bluetooth management reply: %d", len(packet))
	}
	event := binary.LittleEndian.Uint16(packet[0:2])
	index := binary.LittleEndian.Uint16(packet[2:4])
	length := int(binary.LittleEndian.Uint16(packet[4:6]))
	if length > len(packet)-6 {
		return false, 0, fmt.Errorf("truncated Bluetooth management reply: %d/%d", len(packet)-6, length)
	}
	if index != hciIndex || event != mgmtCommandComplete && event != mgmtCommandStatus {
		return false, 0, nil
	}
	if length < 3 {
		return false, 0, fmt.Errorf("short Bluetooth management command reply: %d", length)
	}
	if binary.LittleEndian.Uint16(packet[6:8]) != mgmtDisconnectOpcode {
		return false, 0, nil
	}
	return true, packet[8], nil
}
