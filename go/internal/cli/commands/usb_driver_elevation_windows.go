//go:build windows

package commands

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SHELLEXECUTEINFOW uses pointer-sized handles on both Windows architectures.
type shellExecuteInfo struct {
	size, mask                        uint32
	window                            windows.Handle
	verb, file, parameters, directory *uint16
	show                              int32
	instance                          windows.Handle
	idList                            unsafe.Pointer
	class                             *uint16
	classKey                          windows.Handle
	hotKey                            uint32
	icon, process                     windows.Handle
}

func runElevatedUSBDriver(instance string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	parameters, err := windows.UTF16PtrFromString("__usb-driver " + syscall.EscapeArg(instance))
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	info := shellExecuteInfo{
		// Retain the process handle, complete the launch synchronously, inherit
		// the console and suppress shell error dialogs (UAC remains enabled).
		mask: 0x40 | 0x100 | 0x8000 | 0x400,
		verb: verb, file: file, parameters: parameters, show: windows.SW_HIDE,
	}
	info.size = uint32(unsafe.Sizeof(info))
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	if r, _, err := proc.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return fmt.Errorf("USB driver installation cancelled: administrator approval was declined")
		}
		return fmt.Errorf("starting elevated USB driver helper: %w", err)
	}
	if info.process == 0 {
		return fmt.Errorf("USB driver helper returned no process handle")
	}
	defer windows.CloseHandle(info.process)
	// Do not resume a flash while driver installation is still running.
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("USB driver installation failed (exit %d); run the install command from an administrator terminal for driver diagnostics", code)
	}
	return nil
}
