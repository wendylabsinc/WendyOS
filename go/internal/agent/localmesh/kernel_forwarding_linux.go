//go:build linux

package localmesh

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const meshForwardingOwner = "wendy-mesh-transit"

func meshForwardingRule() []string {
	return []string{"-i", peerInterfacePrefix + "+", "-o", peerInterfacePrefix + "+", "-d", meshid.MeshPrefix.String(), "-m", "comment", "--comment", meshForwardingOwner, "-j", "ACCEPT"}
}

func meshForwardingCommand(action string) error {
	_, err := policyCommand(context.Background(), "ip6tables", append([]string{"-w", "3", "-t", "filter", action, "FORWARD"}, meshForwardingRule()...)...)
	return err
}

func reclaimMeshForwarding() error {
	// Restore a crashed process's global baseline before removing its grants.
	// A live sharing owner keeps the common switch and fallback in place.
	if err := hostnetwork.ReconcileIPv6Forwarding(); err != nil {
		return err
	}
	for meshForwardingCommand("-C") == nil {
		if err := meshForwardingCommand("-D"); err != nil {
			return err
		}
	}
	return nil
}

func (k *Kernel) enableForwarding() error {
	if err := hostnetwork.AcquireIPv6Forwarding(meshForwardingOwner); err != nil {
		return err
	}
	k.forwarding = true
	// Acquire installs the disabled-baseline fallback first; this narrow grant
	// then goes above it. Public/LAN traffic never gets a mesh transit exception.
	if err := meshForwardingCommand("-I"); err != nil {
		return err
	}
	k.forwardRule = true
	return nil
}

func (k *Kernel) closeForwarding() error {
	if k.forwardRule {
		if err := meshForwardingCommand("-D"); err != nil {
			return err
		}
		k.forwardRule = false
	}
	if !k.forwarding {
		return nil
	}
	if err := hostnetwork.ReleaseIPv6Forwarding(meshForwardingOwner); err != nil {
		return errors.Join(errors.New("restore wendy net IPv6 forwarding"), err)
	}
	k.forwarding = false
	return nil
}
