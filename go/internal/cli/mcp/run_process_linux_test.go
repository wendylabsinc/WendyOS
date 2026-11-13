package mcp

import (
	"os/exec"
	"syscall"
	"testing"
)

// If the MCP server dies without a graceful shutdown, the CLI is told to stop.
func TestRunProcessStopsWithTheMCPServer(t *testing.T) {
	cmd := exec.Command("true")
	configureRunProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("SysProcAttr = %+v", cmd.SysProcAttr)
	}
}
