//go:build !linux

package containerd

// Reusable network namespaces depend on Linux nsfs bind mounts and netlink.
func networkSandboxHealthy(_, _ string) bool { return false }

func networkNamespaceHasIP(_, _ string) bool { return false }

func taskUsesNetworkSandbox(_ string, _ uint32) bool { return false }
