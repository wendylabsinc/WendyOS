//go:build linux

package bluetooth

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// sysClassBluetooth lists the kernel's Bluetooth adapters. A var so tests
// can point it at a fake tree.
var sysClassBluetooth = "/sys/class/bluetooth"

// bluezStateDir is where bluetoothd keeps per-device state. A var so tests
// can point it at a fake tree.
var bluezStateDir = "/var/lib/bluetooth"

const (
	// hciFilterOpt is the HCI_FILTER socket option at SOL_HCI.
	hciFilterOpt = 2
	// HCI socket ioctls, _IOR('H', nr, int).
	hciGetDevInfo  = 0x800448D3
	hciGetConnList = 0x800448D4
	// hciMaxConns bounds one HCIGETCONNLIST call; a controller holds far
	// fewer links than this.
	hciMaxConns      = 32
	hciPollTimeoutMS = 1000
	// hciMaxPacket fits any HCI event: type byte, 2-byte header, 255
	// parameter bytes.
	hciMaxPacket = 258
)

func newLinkPlatform(logger *zap.Logger) linkPlatform {
	return linkPlatform{
		adapters: listHCIAdapters,
		open: func(index int) (linkTransport, error) {
			t, err := openHCITransport(logger, index)
			if err != nil {
				return nil, err // not a typed-nil *hciTransport
			}
			return t, nil
		},
		classify: bluezClassifier{},
	}
}

func listHCIAdapters() ([]int, error) {
	entries, err := os.ReadDir(sysClassBluetooth)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.Contains(e.Name(), ":") {
			names = append(names, e.Name())
		}
	}
	return parseAdapterNames(names), nil
}

// hciTransport is a raw HCI socket bound to one adapter, filtered to the
// events the watcher uses.
type hciTransport struct {
	logger    *zap.Logger
	index     int
	fd        int
	address   string // the adapter's own address, naming its bluetoothd state directory
	events    chan hciEvent
	stop      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

func openHCITransport(logger *zap.Logger, index int) (*hciTransport, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("hci socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: uint16(index), Channel: unix.HCI_CHANNEL_RAW}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", adapterName(index), err)
	}
	if err := unix.SetsockoptString(fd, unix.SOL_HCI, hciFilterOpt, string(encodeHCIFilter())); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("set HCI filter on %s: %w", adapterName(index), err)
	}
	info := make([]byte, hciDevInfoSize)
	binary.LittleEndian.PutUint16(info, uint16(index))
	if err := hciIoctl(fd, hciGetDevInfo, info); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("HCIGETDEVINFO %s: %w", adapterName(index), err)
	}
	address, err := devInfoAddress(info)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	t := &hciTransport{
		logger: logger, index: index, fd: fd, address: address,
		events:  make(chan hciEvent, 64),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go t.read()
	return t, nil
}

func hciIoctl(fd int, req uintptr, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// read polls with a timeout, like the L2CAP server, so Close is noticed
// within a second.
func (t *hciTransport) read() {
	defer close(t.stopped)
	defer close(t.events)
	buf := make([]byte, hciMaxPacket)
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		pfd := []unix.PollFd{{Fd: int32(t.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, hciPollTimeoutMS)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			t.logger.Debug("Bluetooth adapter socket poll failed", zap.Error(err))
			return
		}
		if n == 0 {
			continue
		}
		m, err := unix.Read(t.fd, buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			// EPIPE once the adapter is unregistered.
			t.logger.Debug("Bluetooth adapter socket closed", zap.Error(err))
			return
		}
		ev, ok, err := decodeHCIEvent(buf[:m])
		if err != nil {
			t.logger.Debug("Skipping a malformed HCI event", zap.Error(err))
			continue
		}
		if !ok {
			continue
		}
		select {
		case t.events <- ev:
		case <-t.stop:
			return
		}
	}
}

func (t *hciTransport) Events() <-chan hciEvent { return t.events }

func (t *hciTransport) UpdateConnection(handle uint16, u connUpdate) error {
	_, err := unix.Write(t.fd, encodeLEConnUpdate(handle, u))
	return err
}

func (t *hciTransport) Connections() ([]connInfo, error) {
	buf := make([]byte, hciConnListHdrSize+hciMaxConns*hciConnInfoSize)
	binary.LittleEndian.PutUint16(buf[0:], uint16(t.index))
	binary.LittleEndian.PutUint16(buf[2:], hciMaxConns)
	if err := hciIoctl(t.fd, hciGetConnList, buf); err != nil {
		return nil, fmt.Errorf("HCIGETCONNLIST %s: %w", adapterName(t.index), err)
	}
	return parseConnList(buf)
}

func (t *hciTransport) StoredParams(address string) (connUpdate, bool) {
	b, err := os.ReadFile(filepath.Join(bluezStateDir, t.address, strings.ToUpper(address), "info"))
	if err != nil {
		return connUpdate{}, false
	}
	return parseStoredConnParams(string(b))
}

func (t *hciTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.stop)
		<-t.stopped
		unix.Close(t.fd)
	})
	return nil
}

// bluezClassifier asks BlueZ whether a peer is a HID device, with the same
// test Connect uses (isHIDDevice).
type bluezClassifier struct{}

func (bluezClassifier) IsHID(ctx context.Context, adapterIndex int, address string) (bool, bool) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return false, false
	}
	defer conn.Close()
	managed, err := getManagedObjects(ctx, conn)
	if err != nil {
		return false, false
	}
	managed = restrictToAdapter(managed, "/org/bluez/"+adapterName(adapterIndex))
	_, _, props, found := findDeviceByAddress(managed, address)
	if !found {
		return false, false
	}
	return classifyHIDProps(props)
}

// classifyHIDProps is HID when the device looks like one; otherwise the
// answer is only final once BlueZ has resolved the device's services.
func classifyHIDProps(props map[string]dbus.Variant) (isHID, known bool) {
	if isHIDDevice(props) {
		return true, true
	}
	return false, boolProp(props, "ServicesResolved")
}
