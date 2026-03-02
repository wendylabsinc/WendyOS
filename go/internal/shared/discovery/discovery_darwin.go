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

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// discoverLAN uses the macOS dns-sd command to browse for WendyOS devices.
// This works across all network interfaces including USB host-mode connections,
// unlike raw multicast libraries which miss interfaces the system resolver covers.
func discoverLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	browseCtx, browseCancel := context.WithTimeout(ctx, timeout)
	defer browseCancel()

	instances, err := dnssdBrowse(browseCtx, wendyServiceType)
	if err != nil {
		return nil, err
	}

	var devices []models.LANDevice
	seen := make(map[string]bool)

	for _, inst := range instances {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, 2*time.Second)
		dev, err := dnssdResolve(resolveCtx, inst)
		resolveCancel()
		if err != nil {
			continue
		}

		key := fmt.Sprintf("%s-%s-%d", dev.DisplayName, dev.Hostname, dev.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		devices = append(devices, dev)
	}

	return devices, nil
}

type browseResult struct {
	instanceName string
	domain       string
}

// dnssdBrowse runs dns-sd -B and returns as soon as results stop arriving.
// It uses a short settle timer: once the first result arrives, it waits up to
// 500ms for more results before returning. This avoids waiting for the full timeout.
func dnssdBrowse(ctx context.Context, serviceType string) ([]browseResult, error) {
	cmd := exec.CommandContext(ctx, "dns-sd", "-B", serviceType, "local.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Parse lines on a channel so we can select with a timer.
	type parsedLine struct {
		result browseResult
		ok     bool
	}
	lineCh := make(chan parsedLine, 16)

	go func() {
		defer close(lineCh)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, "Add") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 7 {
				continue
			}
			lineCh <- parsedLine{
				result: browseResult{
					instanceName: strings.Join(fields[6:], " "),
					domain:       fields[4],
				},
				ok: true,
			}
		}
	}()

	var results []browseResult
	seen := make(map[string]bool)

	// Wait up to the context deadline for the first result.
	// Once we get one, use a short settle timer for additional results.
	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return results, nil
		case pl, open := <-lineCh:
			if !open {
				_ = cmd.Wait()
				return results, nil
			}
			if pl.ok && !seen[pl.result.instanceName] {
				seen[pl.result.instanceName] = true
				results = append(results, pl.result)
				// Reset settle timer: wait 500ms for more results.
				settle = time.After(500 * time.Millisecond)
			}
		case <-settle:
			// No new results in 500ms, we're done.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return results, nil
		}
	}
}

// dnssdResolve runs dns-sd -L to resolve an instance to hostname, port, and TXT records.
// Returns as soon as the "can be reached at" line is parsed.
func dnssdResolve(ctx context.Context, inst browseResult) (models.LANDevice, error) {
	cmd := exec.CommandContext(ctx, "dns-sd", "-L", inst.instanceName, wendyServiceType, inst.domain)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return models.LANDevice{}, err
	}
	if err := cmd.Start(); err != nil {
		return models.LANDevice{}, err
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
					h, p, err := net.SplitHostPort(hostPort)
					if err == nil {
						hostname = strings.TrimSuffix(h, ".")
						fmt.Sscanf(p, "%d", &port)
					}
					break
				}
			}

			// Also grab TXT from this same line or subsequent content.
			// Parse any key=value pairs on the line.
			for _, field := range parts {
				if k, v, ok := strings.Cut(field, "="); ok {
					txtRecords[k] = v
				}
			}

			// Got what we need, kill the process early.
			_ = cmd.Process.Kill()
			break
		}
	}

	_ = cmd.Wait()

	if hostname == "" {
		return models.LANDevice{}, fmt.Errorf("could not resolve instance %q", inst.instanceName)
	}

	displayName := strings.TrimSuffix(hostname, ".local")
	if dn, ok := txtRecords["displayname"]; ok {
		displayName = dn
	}

	id := ""
	if v, ok := txtRecords["wendyosdevice"]; ok {
		id = v
	} else if v, ok := txtRecords["id"]; ok {
		id = v
	}
	if id == "" {
		id = displayName
	}

	ipAddr := ""
	if addrs, err := net.LookupHost(hostname); err == nil && len(addrs) > 0 {
		ipAddr = addrs[0]
	}

	return models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		IPAddress:     ipAddr,
		Port:          port,
		InterfaceType: "LAN",
		IsWendyDevice: true,
	}, nil
}
