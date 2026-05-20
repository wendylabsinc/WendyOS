//go:build windows

package discovery

import (
	"os"
	"path/filepath"
)

// powershellExe is the absolute path to powershell.exe, resolved once at
// package init time. Bare PATH lookup is unsafe in a 32-bit process on
// 64-bit Windows: PATH lands in SysWOW64, which has a legacy Storage module.
// Resolving via System32 (or Sysnative under WoW64) ensures the host-arch
// PowerShell is used.
var powershellExe = resolvePowershellExe()

func resolvePowershellExe() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	candidates := []string{
		filepath.Join(systemRoot, "Sysnative", "WindowsPowerShell", "v1.0", "powershell.exe"),
		filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "powershell"
}
