//go:build !linux

package containerd

import (
	"fmt"
	"os"
)

// cniNetnsBindDir is private runtime state; tests redirect it to a temporary directory.
var cniNetnsBindDir = "/run/wendy/netns"

// bindNetnsForCNI falls back to the /proc/self/fd/<n> form on non-Linux
// platforms. The bind-mount approach requires Linux mount namespaces; on
// macOS (used for development and testing) the fallback is acceptable since
// the CNI binary is never actually executed on non-Linux systems.
func bindNetnsForCNI(containerID string, netnsRef *os.File) (netnsPath string, cleanup func(), persistent bool) {
	_ = containerID
	return fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd()), func() { netnsRef.Close() }, false
}

func cleanupNetworkSandboxPath(_ string) {}
