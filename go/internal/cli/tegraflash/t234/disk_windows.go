//go:build windows

package t234

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
)

// Windows discovery of the flashing initrd's USB mass-storage LUNs.
//
// The correlation chain mirrors the sysfs walk on Linux: the gadget's USB
// device node (VID_1D6B&PID_0104) carries the physical topology key
// (DEVPKEY LocationPaths, same namespace as winusb.Device.LocationPath, so
// PortPath comparisons against the recovery device work unchanged); its child
// USBSTOR devnode is the disk, found by matching DEVPKEY_Device_Parent; the
// disk's GUID_DEVINTERFACE_DISK path is then opened for the identity and
// geometry IOCTLs. SetupAPI reads identity from the hub, so enumeration
// needs no elevation; the volume lock/dismount and eject paths do.

// Storage IOCTLs and structs not covered by x/sys/windows (winioctl.h values).
const (
	ioctlStorageQueryProperty   = 0x002D1400 // IOCTL_STORAGE_QUERY_PROPERTY
	ioctlStorageGetDeviceNumber = 0x002D1080 // IOCTL_STORAGE_GET_DEVICE_NUMBER
	ioctlStorageMediaRemoval    = 0x002D4804 // IOCTL_STORAGE_MEDIA_REMOVAL
	ioctlStorageEjectMedia      = 0x002D4808 // IOCTL_STORAGE_EJECT_MEDIA
	ioctlDiskGetDriveGeometryEx = 0x000700A0 // IOCTL_DISK_GET_DRIVE_GEOMETRY_EX
	ioctlDiskGetLengthInfo      = 0x0007405C // IOCTL_DISK_GET_LENGTH_INFO
	ioctlVolumeGetDiskExtents   = 0x00560000 // IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS
	fsctlLockVolume             = 0x00090018 // FSCTL_LOCK_VOLUME
	fsctlDismountVolume         = 0x00090020 // FSCTL_DISMOUNT_VOLUME

	busTypeUsb = 7 // STORAGE_BUS_TYPE BusTypeUsb
)

// storagePropertyQuery is STORAGE_PROPERTY_QUERY for StorageDeviceProperty /
// PropertyStandardQuery (both zero).
type storagePropertyQuery struct {
	PropertyID           uint32
	QueryType            uint32
	AdditionalParameters [4]byte
}

// storageDeviceNumber is STORAGE_DEVICE_NUMBER.
type storageDeviceNumber struct {
	DeviceType      uint32
	DeviceNumber    uint32
	PartitionNumber uint32
}

// diskGeometryEx is DISK_GEOMETRY_EX up to DiskSize (the trailing variable
// Data is not read).
type diskGeometryEx struct {
	Cylinders         int64
	MediaType         uint32
	TracksPerCylinder uint32
	SectorsPerTrack   uint32
	BytesPerSector    uint32
	DiskSize          int64
}

// diskExtent / volumeDiskExtents are DISK_EXTENT / VOLUME_DISK_EXTENTS with
// room for a handful of extents (gadget LUNs are single-extent).
type diskExtent struct {
	DiskNumber     uint32
	_              uint32
	StartingOffset int64
	ExtentLength   int64
}

type volumeDiskExtents struct {
	NumberOfDiskExtents uint32
	_                   uint32
	Extents             [4]diskExtent
}

// devpkeyDeviceParent is DEVPKEY_Device_Parent — the PnP instance ID of a
// devnode's parent, which links a USBSTOR disk back to its USB gadget device.
var devpkeyDeviceParent = windows.DEVPROPKEY{
	FmtID: windows.DEVPROPGUID{Data1: 0x4340a6c5, Data2: 0x93fa, Data3: 0x4706, Data4: [8]byte{0x97, 0x2c, 0x7b, 0x64, 0x80, 0x08, 0xa5, 0xa7}},
	PID:   8,
}

// guidDevinterfaceDisk is GUID_DEVINTERFACE_DISK.
var guidDevinterfaceDisk = windows.GUID{Data1: 0x53f56307, Data2: 0xb6bf, Data3: 0x11d0, Data4: [8]byte{0x94, 0xf2, 0x00, 0xa0, 0xc9, 0x1e, 0xfb, 0x8b}}

// usbDeviceNode is one USB-bus devnode as seen by SetupAPI.
type usbDeviceNode struct {
	InstanceID       string
	VID, PID         uint16
	ParentInstanceID string
	LocationPath     string
}

// listUSBDeviceNodes enumerates present devnodes on the USB bus, keeping those
// whose VID/PID the match function accepts (nil accepts all). The location and
// parent properties are read only for accepted nodes — this runs on every
// 1-second poll tick of a stage-2 wait, so the filter keeps per-tick SetupAPI
// property reads off hubs, keyboards, and other bystander devices.
func listUSBDeviceNodes(match func(vid, pid uint16) bool) ([]usbDeviceNode, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return nil, fmt.Errorf("SetupDiGetClassDevsEx(USB): %w", err)
	}
	defer set.Close()

	var out []usbDeviceNode
	for i := 0; ; i++ {
		info, err := set.EnumDeviceInfo(i)
		if err != nil {
			break // ERROR_NO_MORE_ITEMS
		}
		instanceID, err := set.DeviceInstanceID(info)
		if err != nil {
			continue
		}
		vid, pid, ok := winusb.ParseVIDPID(instanceID)
		if !ok || (match != nil && !match(vid, pid)) {
			continue
		}
		n := usbDeviceNode{InstanceID: instanceID, VID: vid, PID: pid}
		if v, err := set.DeviceRegistryProperty(info, windows.SPDRP_LOCATION_PATHS); err == nil {
			n.LocationPath = winusb.FirstString(v)
		}
		if v, err := windows.SetupDiGetDeviceProperty(set, info, &devpkeyDeviceParent); err == nil {
			if s, ok := v.(string); ok {
				n.ParentInstanceID = s
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// usbstorDisk is one USBSTOR disk devnode with its opened-identity fields.
type usbstorDisk struct {
	ParentInstanceID string
	Vendor, Product  string // raw SCSI INQUIRY fields
	BusType          uint32
	DeviceNumber     uint32
	SizeBytes        int64
}

// listUSBStorDisks enumerates USBSTOR devnodes and opens each disk interface
// for identity/number/size. Disks that cannot be opened or queried are
// skipped (they are mid-arrival or not ours). The match function (nil accepts
// all) filters by parent instance ID before the disk is opened — the identity
// IOCTLs cost a handle plus three device round-trips per disk per poll tick,
// which bystander USB disks must not pay.
func listUSBStorDisks(match func(parentInstanceID string) bool) ([]usbstorDisk, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USBSTOR", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return nil, fmt.Errorf("SetupDiGetClassDevsEx(USBSTOR): %w", err)
	}
	defer set.Close()

	var out []usbstorDisk
	for i := 0; ; i++ {
		info, err := set.EnumDeviceInfo(i)
		if err != nil {
			break
		}
		instanceID, err := set.DeviceInstanceID(info)
		if err != nil {
			continue
		}
		d := usbstorDisk{}
		if v, err := windows.SetupDiGetDeviceProperty(set, info, &devpkeyDeviceParent); err == nil {
			if s, ok := v.(string); ok {
				d.ParentInstanceID = s
			}
		}
		if match != nil && !match(d.ParentInstanceID) {
			continue
		}
		paths, err := windows.CM_Get_Device_Interface_List(instanceID, &guidDevinterfaceDisk, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
		if err != nil || len(paths) == 0 || paths[0] == "" {
			continue
		}
		if err := queryDiskIdentity(paths[0], &d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// queryDiskIdentity opens a disk interface path with no data access (IOCTL
// queries only, so no elevation is needed) and fills in the INQUIRY strings,
// bus type, physical drive number, and size.
func queryDiskIdentity(ifacePath string, d *usbstorDisk) error {
	wpath, err := windows.UTF16PtrFromString(ifacePath)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(wpath, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("CreateFile(%s): %w", ifacePath, err)
	}
	defer windows.CloseHandle(h)

	var bytesReturned uint32
	query := storagePropertyQuery{}
	desc := make([]byte, 1024)
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty,
		(*byte)(unsafe.Pointer(&query)), uint32(unsafe.Sizeof(query)),
		&desc[0], uint32(len(desc)), &bytesReturned, nil); err != nil {
		return fmt.Errorf("IOCTL_STORAGE_QUERY_PROPERTY: %w", err)
	}
	vendor, product, busType, err := parseStorageDeviceDescriptor(desc[:bytesReturned])
	if err != nil {
		return err
	}
	d.Vendor, d.Product, d.BusType = vendor, product, busType

	var num storageDeviceNumber
	if err := windows.DeviceIoControl(h, ioctlStorageGetDeviceNumber, nil, 0,
		(*byte)(unsafe.Pointer(&num)), uint32(unsafe.Sizeof(num)), &bytesReturned, nil); err != nil {
		return fmt.Errorf("IOCTL_STORAGE_GET_DEVICE_NUMBER: %w", err)
	}
	d.DeviceNumber = num.DeviceNumber

	var geo diskGeometryEx
	if err := windows.DeviceIoControl(h, ioctlDiskGetDriveGeometryEx, nil, 0,
		(*byte)(unsafe.Pointer(&geo)), uint32(unsafe.Sizeof(geo)), &bytesReturned, nil); err == nil {
		d.SizeBytes = geo.DiskSize
	}
	return nil
}

// parseStorageDeviceDescriptor extracts the SCSI INQUIRY vendor/product
// strings and bus type from a STORAGE_DEVICE_DESCRIPTOR buffer.
func parseStorageDeviceDescriptor(buf []byte) (vendor, product string, busType uint32, err error) {
	// Layout: Version(4) Size(4) DeviceType(1) DeviceTypeModifier(1)
	// RemovableMedia(1) CommandQueueing(1) VendorIdOffset(4) ProductIdOffset(4)
	// ProductRevisionOffset(4) SerialNumberOffset(4) BusType(4) ...
	if len(buf) < 36 {
		return "", "", 0, fmt.Errorf("STORAGE_DEVICE_DESCRIPTOR too short: %d bytes", len(buf))
	}
	le := func(off int) uint32 {
		return uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24
	}
	str := func(off uint32) string {
		if off == 0 || int(off) >= len(buf) {
			return ""
		}
		end := int(off)
		for end < len(buf) && buf[end] != 0 {
			end++
		}
		return string(buf[off:end])
	}
	return str(le(12)), str(le(16)), le(28), nil
}

// listUMSDisks finds the flashing gadget's USB mass-storage whole disks: USB
// devnodes with the gadget VID/PID, joined to their USBSTOR disk children.
func listUMSDisks() ([]UMSDisk, error) {
	nodes, err := listUSBDeviceNodes(func(vid, pid uint16) bool {
		return vid == GadgetVendorID && pid == GadgetProductID
	})
	if err != nil {
		return nil, err
	}
	gadgetPorts := gadgetPortMap(nodes)
	if len(gadgetPorts) == 0 {
		return nil, nil
	}
	stor, err := listUSBStorDisks(func(parentInstanceID string) bool {
		_, ok := gadgetPorts[strings.ToUpper(parentInstanceID)]
		return ok
	})
	if err != nil {
		return nil, err
	}
	var disks []UMSDisk
	for _, s := range stor {
		port, isGadget := gadgetPorts[strings.ToUpper(s.ParentInstanceID)]
		// The geometry query fails, leaving size 0, on a LUN without a medium.
		if !isGadget || s.SizeBytes == 0 {
			continue
		}
		exportName, serial := splitInquiry(s.Vendor, s.Product)
		dev := fmt.Sprintf(`\\.\PhysicalDrive%d`, s.DeviceNumber)
		disks = append(disks, UMSDisk{
			DevPath:   dev,
			RawPath:   dev,
			SizeBytes: s.SizeBytes,
			Vendor:    exportName,
			Serial:    serial,
			PortPath:  port,
		})
	}
	return disks, nil
}

// gadgetPortMap maps every gadget devnode instance ID (upper-cased, the join
// key against a USBSTOR disk's parent) to the physical location path recorded
// for its LUNs. When the gadget enumerates as a composite device, usbccgp
// splits it into MI_xx function devnodes: the USBSTOR disk then parents to
// the MI child, whose own instance trailer is a synthesized ID and whose
// location path carries a #USBMI(n) suffix — but the recovery-port
// correlation works on the composite root, where the physical location and
// the USB serial (the flash session id) live. A function devnode therefore
// resolves to its root's location path.
func gadgetPortMap(nodes []usbDeviceNode) map[string]string {
	byID := make(map[string]usbDeviceNode, len(nodes))
	for _, n := range nodes {
		byID[strings.ToUpper(n.InstanceID)] = n
	}
	ports := make(map[string]string, len(nodes))
	for _, n := range nodes {
		port := n.LocationPath
		if isCompositeFunction(n.InstanceID) {
			if root, ok := byID[strings.ToUpper(n.ParentInstanceID)]; ok && root.LocationPath != "" {
				port = root.LocationPath
			}
		}
		ports[strings.ToUpper(n.InstanceID)] = port
	}
	return ports
}

// isCompositeFunction reports whether a PnP instance ID names a usbccgp
// function devnode (one interface of a composite device) rather than a whole
// USB device.
func isCompositeFunction(instanceID string) bool {
	return strings.Contains(strings.ToUpper(instanceID), "&MI_")
}

// rawUMSInquiry lists every USB-attached disk's raw SCSI vendor/product — a
// diagnostic for a wait that timed out, showing what the device actually
// advertised before splitInquiry rejoins the fields. Runs once on timeout,
// so it deliberately queries every disk, unfiltered.
func rawUMSInquiry() string {
	stor, err := listUSBStorDisks(nil)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, s := range stor {
		if s.BusType != busTypeUsb {
			continue
		}
		fmt.Fprintf(&b, "  - vendor=%q model=%q dev=%q size=%d\n",
			s.Vendor, s.Product, fmt.Sprintf("PhysicalDrive%d", s.DeviceNumber), s.SizeBytes)
	}
	return b.String()
}

// tegraUSBHint reports which Tegra-relevant USB devices are present, so a
// timed-out stage-2 wait can distinguish a board that rebooted into recovery
// from one still exposing the flashing gadget or gone from USB.
func tegraUSBHint() string {
	nodes, _ := listUSBDeviceNodes(func(vid, pid uint16) bool {
		return tegraUSBLabel(vid, pid) != ""
	})
	var found []string
	for _, n := range nodes {
		found = append(found, tegraUSBLabel(n.VID, n.PID))
	}
	if len(found) == 0 {
		return "No NVIDIA recovery (0955:*) or flashing-gadget (1d6b:0104) USB device is present — the board has left USB."
	}
	return "Tegra USB devices present: " + strings.Join(found, ", ")
}

// lockedVolumes holds the open lock handles of dismounted volumes, keyed by
// the disk device path. Holding the handle keeps the volume locked (so the OS
// cannot re-mount a freshly written filesystem mid-flash); handles are
// released when the disk is ejected.
var lockedVolumes = struct {
	sync.Mutex
	byDisk map[string][]windows.Handle
}{byDisk: map[string][]windows.Handle{}}

// unmountUMSDisk best-effort locks and dismounts every mounted volume backed by
// the LUN, so the host stops holding regions the flash is about to overwrite and
// closes some of the auto-mount windows early. It is not the write guard: the
// writer takes the whole disk offline (prepareRawTarget) immediately before
// writing, which force-dismounts everything race-free. So a volume that cannot
// be locked here — the common case, since Windows storm-mounts a removable
// target's stale partitions faster than this pass can lock them — is ignored
// rather than surfaced, because the offline covers it and a warning on a
// perfectly healthy flash only alarms.
func unmountUMSDisk(d UMSDisk) error {
	diskNum, ok := physicalDriveNumber(d.DevPath)
	if !ok {
		return nil
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	fh, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return nil // volume enumeration unavailable; nothing to lock
	}
	defer windows.FindVolumeClose(fh)
	for {
		volPath := windows.UTF16ToString(buf)
		if h, locked, err := lockVolumeOnDisk(volPath, diskNum); err == nil && locked {
			lockedVolumes.Lock()
			lockedVolumes.byDisk[d.DevPath] = append(lockedVolumes.byDisk[d.DevPath], h)
			lockedVolumes.Unlock()
		}
		if err := windows.FindNextVolume(fh, &buf[0], uint32(len(buf))); err != nil {
			return nil
		}
	}
}

// lockVolumeOnDisk checks whether volPath (`\\?\Volume{...}\`) has extents on
// disk diskNum and, if so, locks and dismounts it, returning the open handle
// that holds the lock. The extents check uses a zero-access handle —
// IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS needs no access rights — so volumes on
// other disks are never opened for write, and a filter-driver denial cannot
// be mistaken for "different disk". Only failures on volumes that are on the
// disk are errors.
func lockVolumeOnDisk(volPath string, diskNum uint32) (windows.Handle, bool, error) {
	trimmed := strings.TrimSuffix(volPath, `\`)
	wpath, err := windows.UTF16PtrFromString(trimmed)
	if err != nil {
		return 0, false, nil
	}
	q, err := windows.CreateFile(wpath, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, false, nil // vanished mid-enumeration
	}
	var bytesReturned uint32
	var ext volumeDiskExtents
	extErr := windows.DeviceIoControl(q, ioctlVolumeGetDiskExtents, nil, 0,
		(*byte)(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), &bytesReturned, nil)
	windows.CloseHandle(q)
	if extErr != nil {
		return 0, false, nil // no extents to report (e.g. a dynamic volume) — not a gadget LUN
	}
	onDisk := false
	for i := uint32(0); i < ext.NumberOfDiskExtents && i < uint32(len(ext.Extents)); i++ {
		if ext.Extents[i].DiskNumber == diskNum {
			onDisk = true
		}
	}
	if !onDisk {
		return 0, false, nil
	}
	h, err := windows.CreateFile(wpath, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, false, fmt.Errorf("opening mounted volume %s on PhysicalDrive%d: %w", volPath, diskNum, err)
	}
	if err := windows.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &bytesReturned, nil); err != nil {
		windows.CloseHandle(h)
		return 0, false, fmt.Errorf("locking volume %s on PhysicalDrive%d (is another program using it?): %w", volPath, diskNum, err)
	}
	_ = windows.DeviceIoControl(h, fsctlDismountVolume, nil, 0, nil, 0, &bytesReturned, nil)
	return h, true, nil
}

// ejectUMSDisk ejects the LUN's medium — the "host is done" signal the
// flashing initrd waits for — and releases the volume locks unmountUMSDisk
// took. IOCTL_STORAGE_EJECT_MEDIA keeps the USB device attached.
func ejectUMSDisk(d UMSDisk) error {
	err := ejectMedia(d.DevPath)
	releaseVolumeLocks(d)
	return err
}

// releaseVolumeLocks closes the volume locks unmountUMSDisk took on the disk.
func releaseVolumeLocks(d UMSDisk) {
	lockedVolumes.Lock()
	for _, h := range lockedVolumes.byDisk[d.DevPath] {
		windows.CloseHandle(h)
	}
	delete(lockedVolumes.byDisk, d.DevPath)
	lockedVolumes.Unlock()
}

// physicalDriveNumber parses N out of `\\.\PhysicalDriveN`.
func physicalDriveNumber(devPath string) (uint32, bool) {
	const prefix = `\\.\PhysicalDrive`
	if !strings.HasPrefix(devPath, prefix) {
		return 0, false
	}
	var n uint32
	rest := devPath[len(prefix):]
	if rest == "" {
		return 0, false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint32(c-'0')
	}
	return n, true
}

// ejectMedia allows medium removal and ejects it (SCSI START STOP UNIT).
func ejectMedia(devPath string) error {
	wpath, err := windows.UTF16PtrFromString(devPath)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(wpath, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", devPath, err)
	}
	defer windows.CloseHandle(h)
	var bytesReturned uint32
	allow := [1]byte{0} // PREVENT_MEDIA_REMOVAL.PreventMediaRemoval = FALSE
	_ = windows.DeviceIoControl(h, ioctlStorageMediaRemoval, &allow[0], 1, nil, 0, &bytesReturned, nil)
	if err := windows.DeviceIoControl(h, ioctlStorageEjectMedia, nil, 0, nil, 0, &bytesReturned, nil); err != nil {
		return fmt.Errorf("IOCTL_STORAGE_EJECT_MEDIA(%s): %w", devPath, err)
	}
	return nil
}
