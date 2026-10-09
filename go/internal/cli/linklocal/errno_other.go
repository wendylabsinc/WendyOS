//go:build !windows

package linklocal

import "syscall"

// darwin and the BSDs report a failed ARP as EHOSTDOWN, Linux as EHOSTUNREACH.
var (
	refusedErrnos = []syscall.Errno{syscall.ECONNREFUSED}
	noRouteErrnos = []syscall.Errno{syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EHOSTDOWN}
)
