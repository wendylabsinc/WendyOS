//go:build linux

package localmesh

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// preflightKernel checks capabilities without creating an interface, changing
// a route, or reclaiming state. Probe actual APIs rather than reading kernel
// config files, which vendor images need not expose. Dummy support is checked
// by the real, ownership-tracked LinkAdd during setup.
func preflightKernel() error {
	return runKernelPreflight([]kernelCapabilityCheck{
		{"open an IPv6 socket", kernelIPv6Requirement, func() error {
			fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
			if err != nil {
				return err
			}
			return unix.Close(fd)
		}},
		{"inspect IPv6 policy routing", kernelPolicyRequirement, func() error {
			_, err := netlink.RuleList(netlink.FAMILY_V6)
			return err
		}},
		{"inspect TUN device capabilities", kernelTUNRequirement, func() error {
			file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer file.Close()
			features, err := unix.IoctlGetInt(int(file.Fd()), unix.TUNGETFEATURES)
			if err != nil {
				return err
			}
			if features&unix.IFF_TUN == 0 {
				return fmt.Errorf("TUNGETFEATURES does not report IFF_TUN: %w", unix.EOPNOTSUPP)
			}
			return nil
		}},
	})
}
