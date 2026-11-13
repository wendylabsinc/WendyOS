package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// ReadOnlyMonitoring keeps observability clients from managing the selected
// VM's lifecycle or runtime. Ordinary interactive CLI connections keep their
// existing startup and update behavior unless this option is supplied.
func ReadOnlyMonitoring() resolveOption {
	return func(c *resolveConfig) {
		c.readOnlyMonitoring = true
		c.suppressUpdateCheck = true
		c.suppressProvisioningHint = true
		c.disablePickerEnroll = true
		c.nonInteractive = true
	}
}

func monitoringOptions(readOnly bool) []resolveOption {
	if readOnly {
		return []resolveOption{ReadOnlyMonitoring()}
	}
	return nil
}

// connectRunningSimulator resolves only an existing, running VM. In particular
// it never calls ensureSimulatorRunning, robot provisioning, or update checks.
// The address controls routing; vm:<name> remains the key that authenticates
// the guest, independent of other VMs or containers on the same loopback host.
func connectRunningSimulator(ctx context.Context, device string) (*grpcclient.AgentConnection, bool, error) {
	name, matched, err := simulatorName(device)
	if err != nil {
		return nil, true, err
	}
	if !matched {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	statuses, err := vmStatusesFn()
	if err != nil {
		return nil, true, markSimulatorUnavailable(err)
	}
	for _, status := range statuses {
		if status.Name != name {
			continue
		}
		if !status.Running || status.State.PID == 0 {
			return nil, true, markSimulatorUnavailable(fmt.Errorf("VM %q is not running; start it before opening its dashboard or logs", name))
		}
		addr := vmAddress(status)
		if addr == "" {
			return nil, true, markSimulatorUnavailable(fmt.Errorf("VM %q has no forwarded agent address", name))
		}
		probeCtx, cancel := context.WithTimeout(ctx, vmProbeBudget)
		defer cancel()
		conn, _, err := connectSimulatorAgent(probeCtx, name, addr)
		if err != nil {
			return nil, true, markSimulatorUnavailable(err)
		}
		return conn, true, nil
	}
	return nil, true, markSimulatorUnavailable(fmt.Errorf("VM %q does not exist", name))
}
