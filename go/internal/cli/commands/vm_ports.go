package commands

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// Match the full agent endpoint, including its port, to the live VM record.
// A loopback connection alone could be a native agent or another tunnel.
func userVMForConnection(conn *grpcclient.AgentConnection) (string, error) {
	if conn == nil {
		return "", nil
	}
	// A named VM never becomes an ordinary localhost device just because it
	// stopped during a build. Also refuse a reused endpoint owned by another
	// VM, instead of redirecting a push or port forward to that new owner.
	named := conn.SimulatorName
	if named == "" && conn.Reconnect != nil {
		return "", nil
	}
	host, portString, err := net.SplitHostPort(conn.Addr)
	if err != nil || host != "127.0.0.1" {
		if named != "" {
			return "", fmt.Errorf("VM %q has an invalid agent endpoint %q", named, conn.Addr)
		}
		return "", nil
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		if named != "" {
			return "", fmt.Errorf("VM %q has an invalid agent endpoint %q", named, conn.Addr)
		}
		return "", nil
	}
	statuses, err := vmStatusesFn()
	if err != nil {
		return "", err
	}
	for _, st := range statuses {
		if named != "" && st.Name != named {
			continue
		}
		if st.Running && st.State.NetMode == vm.NetUser && st.State.AgentPort != 0 &&
			(port == st.State.AgentPort || port == st.State.AgentPort+1) {
			return st.Name, nil
		}
	}
	if named != "" {
		return "", fmt.Errorf("VM %q is no longer running at %s; reconnect to vm:%s before deploying", named, conn.Addr, named)
	}
	return "", nil
}

var forwardVMPorts = func(ctx context.Context, name string, ports []int) error {
	s, err := vm.NewStore()
	if err != nil {
		return err
	}
	return s.EnsureTCPPorts(ctx, name, ports)
}

func prepareVMAppPorts(ctx context.Context, conn *grpcclient.AgentConnection, configs ...*appconfig.AppConfig) error {
	ports := vmAppPorts(configs...)
	if len(ports) == 0 {
		return nil
	}
	name, err := userVMForConnection(conn)
	if err != nil {
		return fmt.Errorf("finding simulator for app port forwarding: %w", err)
	}
	if name == "" {
		return nil
	}
	return forwardVMPorts(ctx, name, ports)
}

func vmAppPorts(configs ...*appconfig.AppConfig) []int {
	seen := map[int]bool{}
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		for _, entitlement := range cfg.Entitlements {
			if entitlement.Type == appconfig.EntitlementNetwork {
				for _, mapping := range entitlement.Ports {
					// The container runtime publishes Container on the guest's
					// Host port. QEMU must forward that published port, not the
					// private port inside the container.
					if mapping.Host != 0 {
						seen[int(mapping.Host)] = true
					}
				}
			}
			if entitlement.Type == "http" && entitlement.Port != 0 {
				seen[entitlement.Port] = true
			}
		}
		if cfg.Readiness != nil && cfg.Readiness.TCPSocket != nil && cfg.Readiness.TCPSocket.Port != 0 {
			seen[cfg.Readiness.TCPSocket.Port] = true
		}
	}
	ports := make([]int, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}
