//go:build windows

package winusb

// WinUSB transport: open a bound Jetson device by its device-interface GUID and
// do EP0 control transfers and bulk IN/OUT — the Windows equivalent of the
// gousb/libusb transport used on macOS/Linux. All via winusb.dll + setupapi,
// no cgo.

import (
	"fmt"
	"io"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// USBDevice is an open WinUSB handle to a Jetson device.
type USBDevice struct {
	handle         windows.Handle // CreateFile handle to the device path
	winusb         uintptr        // WINUSB_INTERFACE_HANDLE
	inPipe         uint8          // bulk IN endpoint address (0 if none)
	outPipe        uint8          // bulk OUT endpoint address (0 if none)
	outMaxPacket   uint16         // OUT pipe wMaxPacketSize (for end-of-transfer ZLP framing)
	curInTimeoutMs uint32         // last IN-pipe transfer timeout set, to avoid redundant policy calls
}

// Read implements adbproto.Transport: one bulk-IN transfer bounded by timeout.
// The IN pipe's transfer-timeout policy is (re)set only when the requested bound
// changes, so a steady stream of reads at one timeout doesn't re-arm it each time.
func (d *USBDevice) Read(p []byte, timeout time.Duration) (int, error) {
	ms := uint32(timeout.Milliseconds())
	if ms == 0 {
		ms = 1 // 0 would mean "wait forever" to WinUSB; keep a real bound
	}
	if ms != d.curInTimeoutMs {
		d.setPipeTimeout(d.inPipe, ms)
		d.curInTimeoutMs = ms
	}
	return d.ReadBulk(p)
}

// Write implements adbproto.Transport: a bulk-OUT transfer with no end-of-transfer
// ZLP (ADB is length-prefixed; writeBulkChunked never appends one).
func (d *USBDevice) Write(p []byte) error { return d.writeBulkChunked(p) }

// bulkChunk bounds a single WinUsb_WritePipe. WinUSB rejects/aborts large single
// transfers, so images are sent in chunks. 16 KiB matches the proven macOS/Linux
// bulk write; it is a multiple of any USB max packet size (512/1024), so no chunk
// except possibly the last is a short packet.
const bulkChunk = 0x4000 // 16 KiB

// responseReadTimeoutMs bounds the tolerant status reads between RCM downloads
// (the bootROM does not always answer), matching the macOS 2s readResponse.
const responseReadTimeoutMs = 2000

// longIOTimeoutMs bounds each stage-2 bulk transfer. Large because device-side
// flash steps (QSPI erase, dd/md5 of multi-GB partitions) hold the ADB stream
// open for minutes with no data; mirrors the macOS 30-minute ioTimeout.
const longIOTimeoutMs = 30 * 60 * 1000

// interfaceGUID is DeviceInterfaceGUID parsed once.
var interfaceGUID = mustGUID(DeviceInterfaceGUID)

// openDevicePaths returns the device-interface paths of all devices exposing
// wendy's Jetson WinUSB interface (i.e. bound by our driver package).
func openDevicePaths() ([]string, error) {
	set, _, e := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&interfaceGUID)),
		0,
		0,
		uintptr(digcfPresent|digcfDeviceInterface),
	)
	if set == invalidHandle {
		return nil, fmt.Errorf("SetupDiGetClassDevs: %w", e)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(set)

	var paths []string
	for i := 0; ; i++ {
		var ifd spDeviceInterfaceData
		ifd.cbSize = uint32(unsafe.Sizeof(ifd))
		r, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			set,
			0,
			uintptr(unsafe.Pointer(&interfaceGUID)),
			uintptr(i),
			uintptr(unsafe.Pointer(&ifd)),
		)
		if r == 0 {
			break // ERROR_NO_MORE_ITEMS
		}
		path, err := interfaceDetailPath(set, &ifd)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// interfaceDetailPath resolves the CreateFile device path for one interface.
func interfaceDetailPath(set uintptr, ifd *spDeviceInterfaceData) (string, error) {
	// First call to size the SP_DEVICE_INTERFACE_DETAIL_DATA_W buffer.
	var required uint32
	procSetupDiGetDeviceInterfaceDetailW.Call(
		set,
		uintptr(unsafe.Pointer(ifd)),
		0, 0,
		uintptr(unsafe.Pointer(&required)),
		0,
	)
	if required == 0 {
		return "", fmt.Errorf("interface detail size query failed")
	}
	buf := make([]byte, required)
	// SP_DEVICE_INTERFACE_DETAIL_DATA_W: DWORD cbSize; WCHAR DevicePath[ANYSIZE].
	// cbSize is the fixed header size: 4 (DWORD) + 2 (first WCHAR) with padding =
	// 8 on 64-bit (pointer alignment of the struct → 8).
	*(*uint32)(unsafe.Pointer(&buf[0])) = 8
	r, _, e := procSetupDiGetDeviceInterfaceDetailW.Call(
		set,
		uintptr(unsafe.Pointer(ifd)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(required),
		0,
		0,
	)
	if r == 0 {
		return "", fmt.Errorf("SetupDiGetDeviceInterfaceDetail: %w", e)
	}
	// DevicePath starts at offset 4 (after cbSize) as UTF-16.
	wpath := (*[1 << 20]uint16)(unsafe.Pointer(&buf[4]))[: (required-4)/2 : (required-4)/2]
	return windows.UTF16ToString(wpath), nil
}

// Open opens a Jetson exposing wendy's WinUSB interface. If locationPath is
// non-empty it pins the device at that physical port — the DEVPKEY location path
// reported by ListDevices, which is stable across the recovery→gadget
// re-enumeration. It deliberately does NOT match against the device-interface
// path (which encodes VID/PID/serial and so changes when the device
// re-enumerates and shares no substring with a location path). Empty requires
// exactly one device.
func Open(locationPath string) (*USBDevice, error) {
	return OpenExpected(locationPath, 0)
}

// OpenExpected pins both physical location and USB product. Recovery callers
// use this to ensure an Orin at the same host cannot satisfy a Thor re-open.
func OpenExpected(locationPath string, expectedProduct uint16) (*USBDevice, error) {
	paths, err := openDevicePaths()
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no Jetson WinUSB device found (is the driver installed and the device connected?)")
	}
	if locationPath == "" {
		if expectedProduct != 0 {
			return nil, fmt.Errorf("a physical USB location is required for product 0x%04x", expectedProduct)
		}
		if len(paths) != 1 {
			return nil, fmt.Errorf("%d WinUSB devices present; select a specific device", len(paths))
		}
		return openPath(paths[0])
	}
	// Pin by physical location: find the present NVIDIA device at locationPath
	// and open the interface path CM reports for exactly that devnode.
	devs, err := ListDevices()
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if d.LocationPath != locationPath || (expectedProduct != 0 && d.PID != expectedProduct) {
			continue
		}
		if p := devInterfacePath(d.InstanceID); p != "" {
			return openPath(p)
		}
	}
	return nil, fmt.Errorf("no Jetson WinUSB product 0x%04x at location %q among %d present (is our driver bound to it?)", expectedProduct, locationPath, len(paths))
}

// OpenInstance opens the exact devnode named by its PnP instance ID — stronger
// than location pinning, which hosts with redirected/virtualized USB (no
// SPDRP_LOCATION_PATHS) cannot provide. Recovery flows that already selected a
// specific device must not fall back to "first interface found".
func OpenInstance(instanceID string, expectedProduct uint16) (*USBDevice, error) {
	if expectedProduct != 0 {
		if vid, pid, ok := ParseVIDPID(instanceID); !ok || vid != VendorNVIDIA || pid != expectedProduct {
			return nil, fmt.Errorf("device %s is not USB product 0x%04x", instanceID, expectedProduct)
		}
	}
	p := devInterfacePath(instanceID)
	if p == "" {
		return nil, fmt.Errorf("device %s does not expose the Jetson WinUSB interface (is our driver bound to it?)", instanceID)
	}
	return openPath(p)
}

// devInterfacePath returns the wendy Jetson WinUSB interface path the devnode
// exposes, or "" when it is not bound to our driver. This is exact per-devnode
// correlation via CM_Get_Device_Interface_List — never substring matching of
// instance IDs against interface paths, whose rendering no API guarantees and
// where one device's ID can be a prefix of another's (Windows-generated
// instance IDs for serial-less devices differ only in their port digits).
func devInterfacePath(instanceID string) string {
	paths, err := windows.CM_Get_Device_Interface_List(instanceID, &interfaceGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil || len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func openPath(devPath string) (*USBDevice, error) {
	wpath, err := windows.UTF16PtrFromString(devPath)
	if err != nil {
		return nil, err
	}
	// WinUSB requires the handle be opened for overlapped I/O. We still pass a NULL
	// OVERLAPPED to the transfer calls, which WinUSB completes synchronously
	// (blocking) — sufficient for our sequential flow and long stage-2 writes.
	h, err := windows.CreateFile(
		wpath,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateFile(%s): %w", devPath, err)
	}
	d := &USBDevice{handle: h}
	if r, _, e := procWinUsbInitialize.Call(uintptr(h), uintptr(unsafe.Pointer(&d.winusb))); r == 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("WinUsb_Initialize: %w", e)
	}
	if err := d.discoverPipes(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// discoverPipes queries the interface's endpoints and records the bulk IN/OUT
// pipe addresses.
func (d *USBDevice) discoverPipes() error {
	// WinUsb_QueryInterfaceSettings fills USB_INTERFACE_DESCRIPTOR (bNumEndpoints).
	var desc [9]byte // USB_INTERFACE_DESCRIPTOR is 9 bytes
	if r, _, e := procWinUsbQueryInterfaceSettings.Call(
		d.winusb, 0, uintptr(unsafe.Pointer(&desc[0])),
	); r == 0 {
		return fmt.Errorf("WinUsb_QueryInterfaceSettings: %w", e)
	}
	numEndpoints := desc[4] // bNumEndpoints
	for i := uint8(0); i < numEndpoints; i++ {
		var pipe winusbPipeInfo
		if r, _, _ := procWinUsbQueryPipe.Call(
			d.winusb, 0, uintptr(i), uintptr(unsafe.Pointer(&pipe)),
		); r == 0 {
			continue
		}
		if pipe.PipeType != usbdPipeTypeBulk {
			continue
		}
		if pipe.PipeId&usbEndpointDirectionMask != 0 {
			d.inPipe = pipe.PipeId
		} else {
			d.outPipe = pipe.PipeId
			d.outMaxPacket = pipe.MaximumPacketSize
		}
	}
	if d.inPipe == 0 || d.outPipe == 0 {
		return fmt.Errorf("device missing bulk IN/OUT pipes (in=0x%02x out=0x%02x)", d.inPipe, d.outPipe)
	}

	// Disable the default 5s pipe timeout (0 = wait indefinitely) so long silent
	// stage-2 writes/reads don't abort; auto-clear stalls; terminate exact-
	// multiple OUT transfers with a ZLP (the bootROM/adbd expect end-of-transfer
	// framing, matching the macOS/Linux path's explicit ZLP).
	for _, pid := range []uint8{d.inPipe, d.outPipe} {
		d.setPipePolicyBool(pid, pipePolicyAutoClearStall, true)
	}
	// Both directions get a long (not infinite) timeout: stage-2 ADB reads can
	// block for minutes while nvdd erases/writes/hashes a partition, and OUT
	// writes are similarly long. A finite bound still lets a truly dead adbd fail
	// rather than hang forever. The RCM stage-1 path overrides the IN timeout to a
	// short value via SetReadTimeout, since the bootROM often sends no reply.
	d.setPipeTimeout(d.inPipe, longIOTimeoutMs)
	d.setPipeTimeout(d.outPipe, longIOTimeoutMs)
	// Do NOT enable SHORT_PACKET_TERMINATE: it would append a ZLP after every
	// exact-multiple chunk, which mid-image signals end-of-transfer to the
	// bootROM. WriteImage frames each image with a single trailing ZLP instead.
	return nil
}

// MaxTransferSize returns the OUT pipe's WinUSB MAXIMUM_TRANSFER_SIZE (0 if the
// query fails). Diagnostic only.
func (d *USBDevice) MaxTransferSize() uint32 {
	var v uint32
	sz := uint32(unsafe.Sizeof(v))
	procWinUsbGetPipePolicy.Call(
		d.winusb, uintptr(d.outPipe), uintptr(pipePolicyMaximumTransferSize),
		uintptr(unsafe.Pointer(&sz)), uintptr(unsafe.Pointer(&v)),
	)
	return v
}

// OutMaxPacket exposes the OUT pipe max packet size (diagnostic).
func (d *USBDevice) OutMaxPacket() uint16 { return d.outMaxPacket }

// SetReadTimeout overrides the IN-pipe transfer timeout (ms). Used by the RCM
// stage-1 path to bound its tolerant status reads.
func (d *USBDevice) SetReadTimeout(ms uint32) {
	d.setPipeTimeout(d.inPipe, ms)
	d.curInTimeoutMs = ms
}

// SetTransferTimeout bounds both bulk pipes for transports such as EDL whose
// per-call deadlines differ from RCM/ADB. Unlike legacy setters, errors are
// returned so callers cannot silently use an old (possibly lengthy) timeout.
func (d *USBDevice) SetTransferTimeout(timeout time.Duration, read bool) error {
	ms := uint32(max(1, min(timeout.Milliseconds(), int64(^uint32(0)))))
	pipe := d.outPipe
	if read {
		pipe = d.inPipe
	}
	if r, _, err := procWinUsbSetPipePolicy.Call(d.winusb, uintptr(pipe), uintptr(pipePolicyPipeTransferTimeout),
		uintptr(unsafe.Sizeof(ms)), uintptr(unsafe.Pointer(&ms))); r == 0 {
		return fmt.Errorf("setting USB transfer timeout: %w", err)
	}
	if read {
		d.curInTimeoutMs = ms
	}
	return nil
}

// setPipeTimeout sets PIPE_TRANSFER_TIMEOUT (ms; 0 = infinite) on a pipe.
func (d *USBDevice) setPipeTimeout(pipeID uint8, ms uint32) {
	procWinUsbSetPipePolicy.Call(
		d.winusb, uintptr(pipeID), uintptr(pipePolicyPipeTransferTimeout),
		uintptr(unsafe.Sizeof(ms)), uintptr(unsafe.Pointer(&ms)),
	)
}

// setPipePolicyBool sets a BOOL (UCHAR) pipe policy.
func (d *USBDevice) setPipePolicyBool(pipeID uint8, policy uint32, val bool) {
	var b uint8
	if val {
		b = 1
	}
	procWinUsbSetPipePolicy.Call(
		d.winusb, uintptr(pipeID), uintptr(policy),
		uintptr(unsafe.Sizeof(b)), uintptr(unsafe.Pointer(&b)),
	)
}

// WriteBulk sends buf on the bulk OUT pipe, returning the bytes transferred.
func (d *USBDevice) WriteBulk(buf []byte) (int, error) {
	var transferred uint32
	var p uintptr
	if len(buf) > 0 {
		p = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, e := procWinUsbWritePipe.Call(
		d.winusb, uintptr(d.outPipe), p, uintptr(len(buf)),
		uintptr(unsafe.Pointer(&transferred)), 0,
	)
	if r == 0 {
		return int(transferred), fmt.Errorf("WinUsb_WritePipe: %w", e)
	}
	return int(transferred), nil
}

// writeBulkChunked sends data on the bulk OUT pipe, split into WinUSB-sized
// chunks (WinUSB rejects one huge call). It appends NO end-of-transfer
// zero-length packet, so it is safe for length-prefixed protocols like ADB.
func (d *USBDevice) writeBulkChunked(data []byte) error {
	for off := 0; off < len(data); {
		end := off + bulkChunk
		if end > len(data) {
			end = len(data)
		}
		n, err := d.WriteBulk(data[off:end])
		if err != nil {
			return err
		}
		if n != end-off {
			return io.ErrShortWrite
		}
		off += n
	}
	return nil
}

// WriteImage sends one logical image on the bulk OUT pipe as a single USB
// transfer, split into WinUSB-sized chunks (WinUSB rejects one huge call). If the
// total length is an exact multiple of the pipe's max packet size, a trailing
// zero-length packet marks end-of-transfer (matching the macOS/Linux bulk write).
// This ZLP framing is for the RCM bootROM image chain; do NOT use it for ADB
// (length-prefixed) — writeMsg uses writeBulkChunked instead.
func (d *USBDevice) WriteImage(data []byte) error {
	if err := d.writeBulkChunked(data); err != nil {
		return err
	}
	if d.outMaxPacket > 0 && len(data) > 0 && len(data)%int(d.outMaxPacket) == 0 {
		if _, err := d.WriteBulk(nil); err != nil {
			return fmt.Errorf("sending zero-length packet: %w", err)
		}
	}
	return nil
}

// ReadBulk reads up to len(buf) bytes from the bulk IN pipe.
func (d *USBDevice) ReadBulk(buf []byte) (int, error) {
	var transferred uint32
	var p uintptr
	if len(buf) > 0 {
		p = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, e := procWinUsbReadPipe.Call(
		d.winusb, uintptr(d.inPipe), p, uintptr(len(buf)),
		uintptr(unsafe.Pointer(&transferred)), 0,
	)
	if r == 0 {
		return int(transferred), fmt.Errorf("WinUsb_ReadPipe: %w", e)
	}
	return int(transferred), nil
}

// Close releases the WinUSB handle and device handle.
func (d *USBDevice) Close() {
	if d.winusb != 0 {
		procWinUsbFree.Call(d.winusb)
		d.winusb = 0
	}
	if d.handle != 0 {
		windows.CloseHandle(d.handle)
		d.handle = 0
	}
}

// mustGUID parses a "{...}" GUID string at init.
func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic("winusb: bad DeviceInterfaceGUID: " + err.Error())
	}
	return g
}
