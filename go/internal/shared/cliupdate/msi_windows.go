package cliupdate

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These identifiers are the UpgradeCode and WendyExe Component GUID in
// go/installer/windows/wendy.wxs. Query Windows Installer directly: merely
// residing in Program Files does not establish installation ownership.
const wendyMSIUpgradeCode = "{f47ac10b-58cc-4372-a567-0e02b2c3d479}"
const wendyMSIExecutableComponent = "{a1b2c3d4-e5f6-7890-abcd-ef1234567890}"

func ownsMSIExecutable(executable string) bool {
	msi := windows.NewLazySystemDLL("msi.dll")
	enumerate := msi.NewProc("MsiEnumRelatedProductsW")
	componentPath := msi.NewProc("MsiGetComponentPathW")
	if enumerate.Find() != nil || componentPath.Find() != nil {
		return false
	}
	upgrade, _ := windows.UTF16PtrFromString(wendyMSIUpgradeCode)
	component, _ := windows.UTF16PtrFromString(wendyMSIExecutableComponent)
	// Bound the enumeration even if an unexpected Windows Installer database
	// contains an excessive number of registrations for the same product.
	for index := uint32(0); index < 64; index++ {
		var product [39]uint16
		result, _, _ := enumerate.Call(uintptr(unsafe.Pointer(upgrade)), 0, uintptr(index), uintptr(unsafe.Pointer(&product[0])))
		if result != 0 {
			return false
		}
		var path [32768]uint16
		size := uint32(len(path))
		state, _, _ := componentPath.Call(uintptr(unsafe.Pointer(&product[0])), uintptr(unsafe.Pointer(component)), uintptr(unsafe.Pointer(&path[0])), uintptr(unsafe.Pointer(&size)))
		if state != 3 {
			continue
		} // INSTALLSTATE_LOCAL
		resolved, err := filepath.EvalSymlinks(windows.UTF16ToString(path[:]))
		if err == nil && samePath(executable, resolved, "windows") {
			return true
		}
	}
	return false
}
