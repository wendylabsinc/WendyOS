//go:build windows

package t234

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const rawWriteFlags = os.O_RDWR

// blockDeviceSize reports the size of dev in bytes. Regular files (tests)
// short-circuit to Stat. Physical drives are sized via
// IOCTL_DISK_GET_DRIVE_GEOMETRY_EX (FILE_ANY_ACCESS, so it works on the
// writer's write-only handle where IOCTL_DISK_GET_LENGTH_INFO — which demands
// read access — would not), with GET_LENGTH_INFO as the fallback.
func blockDeviceSize(dev *os.File) (int64, error) {
	if info, err := dev.Stat(); err == nil && info.Mode().IsRegular() {
		return info.Size(), nil
	}
	h := windows.Handle(dev.Fd())
	var bytesReturned uint32
	var geo diskGeometryEx
	if err := windows.DeviceIoControl(h, ioctlDiskGetDriveGeometryEx, nil, 0,
		(*byte)(unsafe.Pointer(&geo)), uint32(unsafe.Sizeof(geo)), &bytesReturned, nil); err == nil {
		return geo.DiskSize, nil
	}
	var length int64
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0,
		(*byte)(unsafe.Pointer(&length)), uint32(unsafe.Sizeof(length)), &bytesReturned, nil); err != nil {
		return 0, fmt.Errorf("sizing %s: %w", dev.Name(), err)
	}
	return length, nil
}
