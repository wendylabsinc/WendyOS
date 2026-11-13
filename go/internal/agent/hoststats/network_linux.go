//go:build linux

package hoststats

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func ownAccountingUnit() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return agentAccountingUnit(data)
}

// EnableAgentNetworkAccounting runs before the agent opens network sockets.
// The runtime-only service property survives an agent restart, not a reboot.
// No traffic policy or firewall rule is changed. Unsupported kernels keep
// device counters available and report service counters as unavailable.
func EnableAgentNetworkAccounting(ctx context.Context) error {
	unit, err := ownAccountingUnit()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "set-property", "--runtime", unit, "IPAccounting=yes").CombinedOutput()
	if err != nil {
		return fmt.Errorf("enabling agent IP accounting: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func ReadNetwork(ctx context.Context) NetworkSample {
	var sample NetworkSample
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err == nil {
		sample.BootID = strings.TrimSpace(string(boot))
	}
	data, err := os.ReadFile("/proc/net/dev")
	if err == nil {
		sample.Interfaces, err = ParseProcNetDev(data)
	}
	if err != nil {
		sample.InterfacesError = "Device network counters are unavailable."
	}
	var interfaces []NetworkInterface
	for _, iface := range sample.Interfaces {
		base := filepath.Join("/sys/class/net", iface.Name)
		index, readErr := os.ReadFile(filepath.Join(base, "ifindex"))
		if readErr != nil {
			continue
		} // interface disappeared during the read
		id, parseErr := strconv.ParseUint(strings.TrimSpace(string(index)), 10, 32)
		if parseErr != nil {
			continue
		}
		iface.Index = uint32(id)
		path, pathErr := filepath.EvalSymlinks(base)
		if pathErr != nil {
			continue
		}
		iface.Virtual = strings.Contains(path, "/virtual/")
		typeData, _ := os.ReadFile(filepath.Join(base, "type"))
		iface.Loopback = strings.TrimSpace(string(typeData)) == "772"
		interfaces = append(interfaces, iface)
	}
	sample.Interfaces = interfaces
	sort.Slice(sample.Interfaces, func(i, j int) bool { return sample.Interfaces[i].Name < sample.Interfaces[j].Name })
	if len(interfaces) == 0 && sample.InterfacesError == "" {
		sample.InterfacesError = "Device network counters are unavailable."
	}
	unit, err := ownAccountingUnit()
	if err == nil {
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		output, readErr := exec.CommandContext(readCtx, "systemctl", "--no-pager", "show", unit,
			"--property=IPAccounting,IPIngressBytes,IPEgressBytes,InvocationID").Output()
		cancel()
		if readErr == nil {
			sample.Agent, err = parseAgentNetwork(output)
		} else {
			err = readErr
		}
	}
	if err != nil {
		sample.AgentError = "Agent service IP counters are unavailable."
	}
	var now unix.Timespec
	if unix.ClockGettime(unix.CLOCK_BOOTTIME, &now) == nil {
		sample.BootTimeNS = uint64(now.Nano())
	}
	return sample
}
