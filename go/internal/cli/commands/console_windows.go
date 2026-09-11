//go:build windows

package commands

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modkernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = modkernel32.NewProc("GetConsoleProcessList")
)

// PauseBeforeExitIfSoleConsole keeps final output readable when wendy runs in
// a console window that dies with the process — the UAC-elevated relaunch
// (ShellExecute "runas" opens a new console) and a double-clicked wendy.exe
// both end with the window vanishing the instant the process exits, taking
// any error or success summary with it. When this process is the console's
// only client and stdin is interactive, wait for Enter before exiting; when
// launched from an existing shell (the normal case) the console has another
// client and this is a no-op.
func PauseBeforeExitIfSoleConsole() {
	// The hidden driver helper must exit so its waiting parent can continue.
	if len(os.Args) > 1 && os.Args[1] == "__usb-driver" {
		return
	}
	if !soleConsoleOwner() || !stdinIsTerminal() {
		return
	}
	fmt.Print("\nPress Enter to close this window... ")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// soleConsoleOwner reports whether this process is the only one attached to
// its console, i.e. the window closes when the process exits.
func soleConsoleOwner() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
