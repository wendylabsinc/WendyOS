//go:build !linux

package services

// containerStorageOnRootSlot is meaningless off Linux (no /var/lib/containerd
// bind-mount convention to check), so it always reports false. This is safe
// because ContainerStorageGate is already a no-op on non-WendyOS hosts via
// defaultIsWendyOSHost, which this stub backs on non-Linux platforms.
func containerStorageOnRootSlot() bool { return false }
