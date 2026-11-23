package services

import (
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/osworkarounds"
)

// rebootGrace bounds how long the agent waits for a systemd shutdown to take
// effect before forcing an immediate restart. A clean shutdown normally kills
// this process long before the grace expires; reaching the end of it means the
// shutdown is stuck (a hung unmount, a container that will not stop), and a
// device wedged mid-update is worse than a hard restart.
const rebootGrace = 60 * time.Second

// rebooter restarts the device. Every side effect is a field so tests can drive
// each path without restarting the test runner.
type rebooter struct {
	logger *zap.Logger
	// sync flushes all mounted filesystems (sync(2)).
	sync func()
	// immediate restarts the kernel now, without unmounting anything.
	immediate func() error
	// clean asks systemd for an orderly shutdown, which unmounts filesystems.
	clean func() error
	grace time.Duration
	sleep func(time.Duration)
}

// systemctlRebootArgs is the systemctl command line for an orderly reboot that
// hands argument to the kernel's restart; an empty argument is a plain reboot.
// systemd keeps the argument in /run/systemd/reboot-param until the final
// reboot(2), so it survives the whole shutdown.
//
// --no-block is required, not merely an optimisation: without it systemctl waits
// for the shutdown transaction to finish, and the caller is a unit that the very
// same transaction is stopping, so the call would block until it is killed.
func systemctlRebootArgs(argument string) []string {
	args := []string{"--no-block"}
	if argument != "" {
		// Accepted since systemd 246; WendyOS (Yocto scarthgap) ships 255.
		args = append(args, "--reboot-argument="+argument)
	}
	return append(args, "reboot")
}

// reboot flushes filesystems and then restarts immediately.
//
// The flush is unconditional and is the fix for WDY-2200's underlying defect:
// restarting the kernel with no userspace sync can discard *any* recently
// written data, and on WendyOS < 0.18.1 the data discarded is the staged UEFI
// capsule, which strands the device's OTA entirely.
func (r rebooter) reboot() error {
	r.sync()
	return r.immediate()
}

// rebootClean flushes filesystems and hands over to systemd so they are also
// unmounted, then forces an immediate restart if that shutdown does not take
// effect within the grace period.
//
// The handover matters beyond the flush: install + a clean `systemctl reboot` is
// the exact combination WDY-2200 validated on hardware (the capsule applied and
// ESRT lowest_supported_version advanced), so affected devices take the path with
// evidence behind it. The flush still happens first, so the capsule is durable
// even when it is the shutdown itself that hangs.
func (r rebooter) rebootClean() error {
	r.sync()
	if err := r.clean(); err != nil {
		// Nothing is pending, so there is nothing to wait for.
		r.logger.Warn("Could not request a clean shutdown; restarting immediately", zap.Error(err))
		return r.immediate()
	}
	r.sleep(r.grace)
	// Still running: the orderly shutdown never completed.
	r.logger.Error("Clean shutdown did not take effect; forcing an immediate restart",
		zap.Duration("grace", r.grace))
	return r.immediate()
}

// rebootAfterOSUpdate restarts the device once an OS update has been installed,
// taking whichever path the running OS version requires.
//
// The running OS matters because the agent, not wendyos-update, performs this
// reboot — which is what lets a new agent fix an OS old enough to be unable to
// deliver its own fix (see osworkarounds).
func rebootAfterOSUpdate(r rebooter, osVersion string) error {
	if osworkarounds.For(osVersion).CleanRebootForCapsuleDurability {
		r.logger.Info("Rebooting via systemd so the staged update reaches disk (WDY-2200 workaround)",
			zap.String("os_version", osVersion))
		return r.rebootClean()
	}
	return r.reboot()
}
