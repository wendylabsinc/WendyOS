package localmesh

import (
	"errors"
	"fmt"
	"syscall"
)

const (
	kernelIPv6Requirement   = "CONFIG_IPV6 and IPv6 enabled at boot and on the interface"
	kernelPolicyRequirement = "CONFIG_IPV6_MULTIPLE_TABLES"
	kernelTUNRequirement    = "CONFIG_TUN and an accessible /dev/net/tun character device"
)

// kernelOperationError keeps the syscall error available to errors.Is while
// identifying the operation that failed. A kernel option is a troubleshooting
// hint only when the error indicates unavailable support, never a diagnosis
// based on a vendor's possibly absent or stale config file.
func kernelOperationError(operation, requirement string, err error) error {
	if err == nil {
		return nil
	}
	if requirement != "" && (errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENOENT)) {
		return fmt.Errorf("wendy-net kernel: %s: %w; check %s", operation, err, requirement)
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("wendy-net kernel: %s: %w; check process permissions and confinement, including CAP_NET_ADMIN for net setup", operation, err)
	}
	return fmt.Errorf("wendy-net kernel: %s: %w", operation, err)
}

type kernelCapabilityCheck struct {
	operation   string
	requirement string
	check       func() error
}

func runKernelPreflight(checks []kernelCapabilityCheck) error {
	for _, check := range checks {
		if err := check.check(); err != nil {
			return kernelOperationError(check.operation, check.requirement, err)
		}
	}
	return nil
}
