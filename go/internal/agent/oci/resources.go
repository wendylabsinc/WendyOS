package oci

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// DefaultMaxPIDs is the conservative process/thread ceiling applied to every
// container that does not declare its own `pids` limit (WDY-1101). It is a
// fork-bomb / runaway-task DoS guard: without it a single malicious or buggy
// container can exhaust the host PID space and take down the agent and every
// other app. The value is intentionally generous — well above what normal
// (even heavily threaded ML/ROS 2) workloads need — so it bounds a runaway
// without throttling legitimate apps; an app that genuinely needs more sets a
// higher `pids` in wendy.json. Unlike memory/CPU, a default PID cap does not
// risk OOM-kills or CPU throttling, so it is safe to apply by default while
// memory and CPU stay opt-in (WDY-1729 backward compatibility).
const DefaultMaxPIDs int64 = 4096

// ApplyResourceLimits translates the app/service ResourceLimits into cgroup
// constraints on the OCI spec. It is additive: only the fields the developer
// set are applied, and existing Resources entries (notably the device cgroup
// rules established by DefaultSpec and ApplyEntitlements) are preserved. It
// returns an error if any set value fails to parse — the caller should refuse
// to start the container rather than silently run it unbounded.
//
// A default PID limit (DefaultMaxPIDs) is always applied when none is declared,
// even for nil/empty limits; memory and CPU are left unbounded unless declared.
func ApplyResourceLimits(spec *Spec, limits *appconfig.ResourceLimits) error {
	var memBytes, quota *int64
	var period *uint64
	var pids *int64

	if limits != nil {
		var err error
		if memBytes, err = limits.MemoryLimitBytes(); err != nil {
			return err
		}
		if quota, period, err = limits.CPUQuota(); err != nil {
			return err
		}
		pids = limits.PIDsLimit()
	}

	// WDY-1101: every container gets a fork-bomb guard unless it sets its own.
	if pids == nil {
		d := DefaultMaxPIDs
		pids = &d
	}

	if spec.Linux == nil {
		spec.Linux = &Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &LinuxResources{}
	}
	res := spec.Linux.Resources

	if memBytes != nil {
		// OCI defines Swap as RAM + swap. Equal ceilings disable swap on
		// cgroup v2 (memory.swap.max=0), so an explicit memory budget cannot
		// silently consume unbounded host swap after hitting memory.max.
		res.Memory = &LinuxMemory{Limit: memBytes, Swap: memBytes}
	}
	if quota != nil {
		res.CPU = &LinuxCPU{Quota: quota, Period: period}
	}
	res.Pids = &LinuxPids{Limit: *pids}
	return nil
}
