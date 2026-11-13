package linklocal

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows reports Winsock codes, which syscall's portable names do not match.
var (
	refusedErrnos = []syscall.Errno{syscall.ECONNREFUSED, windows.WSAECONNREFUSED}
	noRouteErrnos = []syscall.Errno{
		syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EHOSTDOWN,
		windows.WSAEHOSTUNREACH, windows.WSAENETUNREACH, windows.WSAEHOSTDOWN,
	}
)
