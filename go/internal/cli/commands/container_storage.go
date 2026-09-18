package commands

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// isWendyOSAgent reports whether resp describes a WendyOS device, as opposed
// to a generic Linux/macOS host running the agent directly. It is used to
// scope container-storage-on-root-slot handling (WDY-3127) to devices that
// actually rely on a /data bind mount for container storage; a plain Linux
// host with its container storage on / is a normal, supported configuration.
//
// os_version is only populated on WendyOS ("WendyOS-<version>"); device_type
// (the MACHINE/BOARD value from /etc/wendyos/device-type) is likewise only
// populated on WendyOS. Either signal is sufficient.
func isWendyOSAgent(resp *agentpb.GetAgentVersionResponse) bool {
	return strings.HasPrefix(resp.GetOsVersion(), "WendyOS-") || resp.GetDeviceType() != ""
}

// containerStorageDegraded reports whether container storage is stuck on the
// OS root slot because the /data bind mount is not active (WDY-3127).
//
// Agents new enough to know about this report it explicitly via
// container_storage_degraded, which is honoured verbatim (including an
// explicit false, which can disagree with the derivation below on a WendyOS
// device where being on / is actually fine, e.g. a device with no separate
// /data partition at all). Older agents don't set the field, so it is
// derived: a WendyOS device whose container storage mountpoint is "/" while
// a "/data" partition exists is exactly the failure this gate is for — /data
// exists but isn't mounted where container storage expects it.
func containerStorageDegraded(resp *agentpb.GetAgentVersionResponse) bool {
	if resp != nil && resp.ContainerStorageDegraded != nil {
		return resp.GetContainerStorageDegraded()
	}
	if !isWendyOSAgent(resp) {
		return false
	}
	if resp.GetContainerStorage().GetMountpoint() != "/" {
		return false
	}
	for _, p := range resp.GetPartitions() {
		if p.GetMountpoint() == "/data" {
			return true
		}
	}
	return false
}

// containerStorageDegradedError is returned by preflightContainerStorage to
// refuse a deploy while container storage is on the OS root slot (WDY-3127).
type containerStorageDegradedError struct {
	storage *agentpb.DiskPartition
}

func (e *containerStorageDegradedError) Error() string {
	const (
		prefix = "Deploy refused: container storage on this device is on the OS root slot"
		suffix = " because the /data bind mount is not active. Power-cycle the device; " +
			"if 'wendy device info' still shows container storage on /, the OS did not mount /data (WDY-3127)."
	)
	if e.storage == nil || e.storage.GetDevice() == "" || e.storage.GetTotalBytes() <= 0 {
		return prefix + suffix
	}
	return fmt.Sprintf("%s (/ on %s, %s of %s used)%s", prefix,
		e.storage.GetDevice(), formatGigabytes(e.storage.GetUsedBytes()), formatGigabytes(e.storage.GetTotalBytes()), suffix)
}

// preflightContainerStorage refuses a deploy before any build or push starts
// when container storage is on the OS root slot (WDY-3127). It returns nil
// when the device is healthy.
func preflightContainerStorage(resp *agentpb.GetAgentVersionResponse) error {
	if !containerStorageDegraded(resp) {
		return nil
	}
	return &containerStorageDegradedError{storage: resp.GetContainerStorage()}
}

// containerStorageDegradedWarningText is the warning line 'wendy device info'
// prints when container storage is on the OS root slot (WDY-3127).
func containerStorageDegradedWarningText(resp *agentpb.GetAgentVersionResponse) string {
	return fmt.Sprintf(
		"container storage is on the OS root slot (/ on %s); the /data bind mount is not active. "+
			"Deploys are refused until the device is power-cycled (WDY-3127).",
		resp.GetContainerStorage().GetDevice(),
	)
}
