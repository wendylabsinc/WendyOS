//go:build windows

package winusb

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// InstallDriverFor stages a complete family package and binds the selected
// hardware ID. UpdateDriverForPlugAndPlayDevices is ID-wide, so refuse if
// another device with that ID is present. Other board families are untouched.
// Requires elevation; the command layer handles UAC before calling this.
func InstallDriverFor(out io.Writer, target Device, profile DriverProfile) error {
	if !profile.supports(target) {
		return fmt.Errorf("unsupported %s target %s", profile.Name, target.HardwareID())
	}
	if err := ValidateDriverTarget(target); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "wendy-usb-driver-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	infPath := filepath.Join(dir, profile.inf)
	catPath := filepath.Join(dir, profile.catalog)
	fmt.Fprintf(out, "Preparing %s USB driver package…\n", profile.Name)
	if err := os.WriteFile(infPath, []byte(generateProfileINF(profile)), 0o644); err != nil {
		return err
	}
	cert, err := createSigningCert(true)
	if err != nil {
		return err
	}
	defer cert.Free()
	if err := buildAndSignCatalog(catPath, infPath, profile.hardwareIDs(), cert); err != nil {
		return err
	}
	if err := cert.installToStores(); err != nil {
		return err
	}
	if err := stageDriverPackage(infPath); err != nil {
		return err
	}
	// Repeat immediately before the ID-wide bind in case a board was unplugged
	// or another appeared while signing/staging the package.
	if err := ValidateDriverTarget(target); err != nil {
		return err
	}
	ok, err := bindDriverToPresentDevices(infPath, target.HardwareID())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s disappeared before its driver could be bound", target.InstanceID)
	}
	// PnP can complete asynchronously. Do not equate staging or a bind call's
	// return value with a usable, up-to-date target.
	deadline := time.Now().Add(15 * time.Second)
	for {
		devices, err := ListVendor(target.VID)
		if err != nil {
			return err
		}
		for _, d := range devices {
			if !strings.EqualFold(d.InstanceID, target.InstanceID) {
				continue
			}
			ready, err := DriverReady(d, profile)
			if err != nil {
				return err
			}
			if ready {
				fmt.Fprintf(out, "%s driver ready for %s.\n", profile.Name, target.InstanceID)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s package staged, but %s did not acquire a compatible binding; reconnect it and retry", profile.Name, target.InstanceID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ValidateDriverTarget checks that an ID-wide bind would affect only the
// selected device. Callers can surface this error before asking for elevation;
// InstallDriverFor repeats the check immediately before modifying the binding.
func ValidateDriverTarget(target Device) error {
	devices, err := ListVendor(target.VID)
	if err != nil {
		return err
	}
	return soleBindingTarget(devices, target)
}

func soleBindingTarget(devices []Device, target Device) error {
	matched := false
	count := 0
	for _, d := range devices {
		if d.VID != target.VID || d.PID != target.PID {
			continue
		}
		count++
		matched = matched || strings.EqualFold(d.InstanceID, target.InstanceID)
	}
	if !matched {
		return fmt.Errorf("selected USB device %s is no longer present", target.InstanceID)
	}
	if count != 1 {
		return fmt.Errorf("%d devices share %s; disconnect the others before installing its driver", count, target.HardwareID())
	}
	return nil
}

// stageDriverPackage copies the INF (and its referenced catalog) into the Windows
// driver store via SetupCopyOEMInf. Fails if the catalog isn't validly signed by
// a trusted publisher — which is why installToStores runs first.
func stageDriverPackage(infPath string) error {
	winf, err := windows.UTF16PtrFromString(infPath)
	if err != nil {
		return err
	}
	const spostPath = 1 // SPOST_PATH
	r, _, e := procSetupCopyOEMInfW.Call(
		uintptr(unsafe.Pointer(winf)),
		0,         // OEMSourceMediaLocation
		spostPath, // OEMSourceMediaType
		0,         // CopyStyle
		0,         // DestinationInfFileName
		0,         // DestinationInfFileNameSize
		0,         // RequiredSize
		0,         // DestinationInfFileNameComponent
	)
	if r == 0 {
		return fmt.Errorf("SetupCopyOEMInf: %w", e)
	}
	return nil
}

// bindDriverToPresentDevices installs infPath onto any currently-present device
// matching hwid. Returns (false, nil) when no such device is present (not an
// error — the package is staged and will bind on connect). Reports whether a
// reboot was requested (ignored; WinUSB never needs one).
func bindDriverToPresentDevices(infPath, hwid string) (bool, error) {
	winf, err := windows.UTF16PtrFromString(infPath)
	if err != nil {
		return false, err
	}
	whwid, err := windows.UTF16PtrFromString(hwid)
	if err != nil {
		return false, err
	}
	var rebootRequired uint32
	r, _, e := procUpdateDriverForPlugAndPlayDevicesW.Call(
		0, // hwndParent
		uintptr(unsafe.Pointer(whwid)),
		uintptr(unsafe.Pointer(winf)),
		uintptr(installFlagForce),
		uintptr(unsafe.Pointer(&rebootRequired)),
	)
	if r == 0 {
		// ERROR_NO_SUCH_DEVINST / ERROR_NO_MORE_ITEMS: no present device with this
		// ID — expected for the gadget PID and any absent board. Not an error.
		if errno, ok := e.(windows.Errno); ok {
			switch uintptr(errno) {
			case 0xE000020B, // ERROR_NO_SUCH_DEVINST (SPAPI)
				uintptr(windows.ERROR_NO_MORE_ITEMS),
				uintptr(windows.ERROR_FILE_NOT_FOUND):
				return false, nil
			}
		}
		return false, fmt.Errorf("UpdateDriverForPlugAndPlayDevices: %w", e)
	}
	return true, nil
}
