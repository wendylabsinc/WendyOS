//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// cpUTF8 is the Windows code page identifier for UTF-8. x/sys/windows does
// not export this constant, so we use the literal value documented by
// Microsoft (Win32 API: `CP_UTF8 = 65001`).
const cpUTF8 = 65001

// init switches the Windows console to UTF-8 for both stdin and stdout so SSIDs
// (and any other non-ASCII text) round-trip without being transcoded to the
// active OEM/ANSI codepage. Without this, an emoji-bearing SSID rendered to the
// console becomes a literal `?` byte the moment Windows converts UTF-8 output
// to cp437/cp850, and the same `?` is what the user types back when they
// re-enter the SSID for `wendy device wifi connect` — at which point the
// agent's nmcli scan can never match it.
//
// Failures here are non-fatal: if the process is not attached to a console
// (e.g. piped, redirected, or running under a service host) the calls return
// an error which we ignore — there is no console to fix in that case.
func init() {
	_ = windows.SetConsoleOutputCP(cpUTF8)
	_ = windows.SetConsoleCP(cpUTF8)
}
