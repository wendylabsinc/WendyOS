//go:build !linux && !windows

package mcp

import "syscall"

// runSysProcAttr has no parent-death signal outside Linux; a graceful MCP
// shutdown still stops the run (serveStdioStreams).
func runSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
