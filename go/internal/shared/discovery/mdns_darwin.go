//go:build darwin

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// BrowseMDNSServices discovers mDNS services of the given type on macOS
// using dns-sd. Returns all services found within the timeout.
func BrowseMDNSServices(ctx context.Context, serviceType string, timeout time.Duration) ([]MDNSService, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	browseCtx, browseCancel := context.WithTimeout(ctx, timeout)
	defer browseCancel()

	instances, err := dnssdBrowse(browseCtx, serviceType)
	if err != nil {
		return nil, err
	}

	var services []MDNSService
	seen := make(map[string]bool)

	for _, inst := range instances {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, 2*time.Second)
		svc, err := resolveMDNSService(resolveCtx, inst, serviceType)
		resolveCancel()
		if err != nil {
			continue
		}

		key := fmt.Sprintf("%s-%s-%d", svc.InstanceName, svc.Hostname, svc.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		services = append(services, svc)
	}

	return services, nil
}

// resolveMDNSService runs dns-sd -L to resolve a browse result into an MDNSService.
func resolveMDNSService(ctx context.Context, inst browseResult, serviceType string) (MDNSService, error) {
	cmd := exec.CommandContext(ctx, "dns-sd", "-L", inst.instanceName, serviceType, inst.domain)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return MDNSService{}, err
	}
	if err := cmd.Start(); err != nil {
		return MDNSService{}, err
	}

	var hostname string
	var port int
	txtRecords := make(map[string]string)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.Contains(line, "can be reached at") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "at" && i+1 < len(parts) {
					hostPort := parts[i+1]
					h, portStr, splitErr := net.SplitHostPort(hostPort)
					if splitErr == nil {
						hostname = strings.TrimSuffix(h, ".")
						fmt.Sscanf(portStr, "%d", &port)
					}
					break
				}
			}

			for _, field := range parts {
				if k, v, ok := strings.Cut(field, "="); ok {
					txtRecords[k] = v
				}
			}

			_ = cmd.Process.Kill()
			break
		}
	}

	_ = cmd.Wait()

	if hostname == "" {
		return MDNSService{}, fmt.Errorf("could not resolve instance %q", inst.instanceName)
	}

	ipAddr := ""
	if addrs, lookupErr := net.LookupHost(hostname); lookupErr == nil && len(addrs) > 0 {
		ipAddr = addrs[0]
	}

	return MDNSService{
		InstanceName: inst.instanceName,
		Hostname:     hostname,
		IPAddress:    ipAddr,
		Port:         port,
		TXTRecords:   txtRecords,
	}, nil
}
