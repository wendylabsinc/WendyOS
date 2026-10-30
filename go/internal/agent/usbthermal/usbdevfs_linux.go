//go:build linux

package usbthermal

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// usbfs from Go without cgo: the ioctls in <linux/usbdevice_fs.h>, issued on
// an O_RDWR file descriptor of /dev/bus/usb/BBB/DDD.

// usbdevfsCtrlTransfer is struct usbdevfs_ctrltransfer. Go lays it out like C
// on both agent architectures: 24 bytes on 64-bit (four bytes of padding
// before the pointer), 16 on 32-bit. The ioctl numbers below are derived from
// the Go size so they cannot disagree with it.
type usbdevfsCtrlTransfer struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
	Timeout     uint32 // milliseconds
	Data        unsafe.Pointer
}

// usbdevfsBulkTransfer is struct usbdevfs_bulktransfer.
type usbdevfsBulkTransfer struct {
	Endpoint uint32
	Length   uint32
	Timeout  uint32 // milliseconds
	Data     unsafe.Pointer
}

// usbdevfsSetInterface is struct usbdevfs_setinterface.
type usbdevfsSetInterface struct {
	Interface  uint32
	AltSetting uint32
}

// Linux's generic ioctl encoding: direction in bits 30-31, size in 16-29,
// type in 8-15, number in 0-7.
const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | size<<16 | typ<<8 | nr }

var (
	usbdevfsControl          = ioc(iocRead|iocWrite, 'U', 0, unsafe.Sizeof(usbdevfsCtrlTransfer{}))
	usbdevfsBulk             = ioc(iocRead|iocWrite, 'U', 2, unsafe.Sizeof(usbdevfsBulkTransfer{}))
	usbdevfsSetinterface     = ioc(iocRead, 'U', 4, unsafe.Sizeof(usbdevfsSetInterface{}))
	usbdevfsClaiminterface   = ioc(iocRead, 'U', 15, unsafe.Sizeof(uint32(0)))
	usbdevfsReleaseinterface = ioc(iocRead, 'U', 16, unsafe.Sizeof(uint32(0)))
)

// usbfsDevice is a Device on a usbfs node.
type usbfsDevice struct {
	fd   int
	path string
}

// OpenUSBFS opens a usbfs device node for control and bulk transfers.
func OpenUSBFS(path string) (Device, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &usbfsDevice{fd: fd, path: path}, nil
}

func defaultOpen() OpenFunc { return OpenUSBFS }

func (d *usbfsDevice) ioctl(req uintptr, arg unsafe.Pointer) (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(d.fd), req, uintptr(arg))
	if errno != 0 {
		return 0, mapErrno(errno)
	}
	return int(r), nil
}

// mapErrno turns the errnos the driver acts on into this package's sentinels,
// keeping the errno in the chain for logs.
func mapErrno(errno unix.Errno) error {
	switch errno {
	case unix.ETIMEDOUT:
		return fmt.Errorf("%w: %w", ErrTimeout, errno)
	case unix.EBUSY:
		return fmt.Errorf("%w: %w", ErrBusy, errno)
	}
	return errno
}

func (d *usbfsDevice) Control(requestType, request uint8, value, index uint16, data []byte, timeout time.Duration) (int, error) {
	if len(data) > 0xFFFF {
		return 0, errors.New("control transfer longer than 65535 bytes")
	}
	ct := usbdevfsCtrlTransfer{
		RequestType: requestType,
		Request:     request,
		Value:       value,
		Index:       index,
		Length:      uint16(len(data)),
		Timeout:     uint32(timeout / time.Millisecond),
	}
	if len(data) > 0 {
		ct.Data = unsafe.Pointer(&data[0])
	}
	n, err := d.ioctl(usbdevfsControl, unsafe.Pointer(&ct))
	runtime.KeepAlive(data)
	return n, err
}

func (d *usbfsDevice) Bulk(endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	bt := usbdevfsBulkTransfer{
		Endpoint: uint32(endpoint),
		Length:   uint32(len(data)),
		Timeout:  uint32(timeout / time.Millisecond),
		Data:     unsafe.Pointer(&data[0]),
	}
	n, err := d.ioctl(usbdevfsBulk, unsafe.Pointer(&bt))
	runtime.KeepAlive(data)
	return n, err
}

func (d *usbfsDevice) ClaimInterface(iface uint32) error {
	_, err := d.ioctl(usbdevfsClaiminterface, unsafe.Pointer(&iface))
	return err
}

func (d *usbfsDevice) ReleaseInterface(iface uint32) error {
	_, err := d.ioctl(usbdevfsReleaseinterface, unsafe.Pointer(&iface))
	return err
}

func (d *usbfsDevice) SetInterface(iface, altSetting uint32) error {
	si := usbdevfsSetInterface{Interface: iface, AltSetting: altSetting}
	_, err := d.ioctl(usbdevfsSetinterface, unsafe.Pointer(&si))
	return err
}

func (d *usbfsDevice) Close() error { return unix.Close(d.fd) }
