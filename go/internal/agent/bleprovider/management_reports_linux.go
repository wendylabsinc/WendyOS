//go:build linux

package bleprovider

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	mgmtDeviceFound          = 0x0012
	managementAllControllers = 0xffff
)

type managementAdvertisement struct {
	address, addressType string
	scanResponse         bool
	data                 []byte
}

func decodeManagementAdvertisement(packet []byte, index int) (managementAdvertisement, bool, error) {
	var out managementAdvertisement
	if len(packet) < 6 {
		return out, false, ErrInvalidAdvertisement
	}
	length := int(binary.LittleEndian.Uint16(packet[4:6]))
	if length != len(packet)-6 {
		return out, false, ErrInvalidAdvertisement
	}
	if int(binary.LittleEndian.Uint16(packet[2:4])) != index || binary.LittleEndian.Uint16(packet[:2]) != mgmtDeviceFound {
		return out, false, nil
	}
	// bdaddr (6), address type (1), RSSI (1), flags (4), EIR length (2).
	if length < 14 || int(binary.LittleEndian.Uint16(packet[18:20])) != length-14 {
		return out, false, ErrInvalidAdvertisement
	}
	switch packet[12] {
	case btAddrLEPublic:
		out.addressType = "public"
	case btAddrLERandom:
		out.addressType = "random"
	default:
		return out, false, nil // BR/EDR is not a BLE observation.
	}
	var address [6]byte
	for i := range address {
		address[i] = packet[11-i]
	}
	out.address = formatAddress(address)
	out.scanResponse = binary.LittleEndian.Uint32(packet[14:18])&(1<<5) != 0
	out.data = packet[20:]
	return out, true, nil
}

type managementReportReader interface {
	Read(context.Context) ([]byte, error)
	Close() error
}

type managementReportSocket struct {
	fd        int
	closeOnce sync.Once
	buffer    [65541]byte // management header + maximum uint16 event length
}

// Listen only: never issue management commands or take ownership of scanning.
// BlueZ continues to own discovery, scan scheduling and advertising instances.
// Opening this socket requires the Linux Bluetooth management capability
// (normally CAP_NET_ADMIN).
func openManagementReports() (managementReportReader, error) {
	// Binding an untrusted control socket can succeed, but Device Found is
	// broadcast only to trusted sockets. Refuse that otherwise silent failure.
	var capabilities [2]unix.CapUserData
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		return nil, fmt.Errorf("inspect BLE report capability: %w", err)
	}
	if capabilities[0].Effective&(uint32(1)<<unix.CAP_NET_ADMIN) == 0 {
		return nil, fmt.Errorf("BLE advertising reports require CAP_NET_ADMIN: %w", unix.EPERM)
	}
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("open BLE advertising report socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: managementAllControllers, Channel: unix.HCI_CHANNEL_CONTROL}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind BLE advertising report socket: %w", err)
	}
	return &managementReportSocket{fd: fd}, nil
}

func (s *managementReportSocket) Close() error {
	var err error
	s.closeOnce.Do(func() { err = unix.Close(s.fd) })
	return err
}

func (s *managementReportSocket) Read(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 250)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
			return nil, fmt.Errorf("BLE report socket poll failure: 0x%x", fds[0].Revents)
		}
		n, _, flags, _, err := unix.Recvmsg(s.fd, s.buffer[:], nil, unix.MSG_DONTWAIT)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if flags&unix.MSG_TRUNC != 0 {
			continue
		}
		return s.buffer[:n], nil
	}
}

// A report-source failure degrades discovery without dropping established
// streams or the binary advertisement path. Reopening clears partial/name
// observations so packets from different source epochs cannot be combined.
func runManagementReports(ctx context.Context, index int, f *advertisementFreshness, initial managementReportReader, open func() (managementReportReader, error), status chan<- error) error {
	reader := initial
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	report := func(err error) bool {
		select {
		case status <- err:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		if reader == nil {
			var err error
			reader, err = open()
			if err != nil {
				if !report(err) {
					return nil
				}
				if !waitManagementReportRetry(ctx) {
					return nil
				}
				continue
			}
			if !report(nil) {
				return nil
			}
		}
		packet, err := reader.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_ = reader.Close()
			reader = nil
			f.clearReports()
			if !report(fmt.Errorf("receiving BLE advertisements: %w", err)) {
				return nil
			}
			if !waitManagementReportRetry(ctx) {
				return nil
			}
			continue
		}
		advert, matched, err := decodeManagementAdvertisement(packet, index)
		if err != nil || !matched {
			continue
		}
		f.observeReport(advert.address, advert.addressType, advert.data, time.Now())
	}
	return nil
}

func waitManagementReportRetry(ctx context.Context) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
