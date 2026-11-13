package mcp

import "syscall"

// runSysProcAttr also has the kernel send the CLI SIGTERM if the MCP server
// dies without a graceful shutdown (crash, SIGKILL), so it stops as on Ctrl-C.
// Pdeathsig follows the thread that started the child; the Go runtime keeps
// that thread for the process lifetime because the caller never locks it.
func runSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
