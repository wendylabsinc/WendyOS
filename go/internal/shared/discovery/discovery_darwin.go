//go:build darwin

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
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
			// Resolve failed (e.g. could not parse hostname) — fall back to
			// a device derived from the browse instance name.
			dev = deviceFromBrowse(inst)
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
	cmd := exec.CommandContext(ctx, "dns-sd", "-B", serviceType, "local")
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

	return models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		Port:          port,
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}, nil
}

// deviceFromBrowse builds a LANDevice from browse results alone, without
// resolving via dns-sd -L. Used as a fallback when resolve fails (e.g.
// the service has no TXT records).

var hostnameLabelRegexp = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// isValidHostnameLabel reports whether s is a valid RFC1123 hostname label.
func isValidHostnameLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	return hostnameLabelRegexp.MatchString(s)
}

func deviceFromBrowse(inst browseResult) models.LANDevice {
	displayName := inst.instanceName

	var (
		id       string
		hostname string
		port     int
	)

	// Only synthesize a hostname/ID when the instance name is already a valid
	// hostname label. Otherwise, leave Hostname empty and Port zero to avoid
	// exposing a misleading dialable pair.
	if isValidHostnameLabel(inst.instanceName) {
		id = inst.instanceName
		hostname = inst.instanceName + ".local"
		port = 50051
	}

	return models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		Port:          port,
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}
}

// discoverLANContinuous keeps dns-sd -B running and sends each newly
// discovered device to ch as it's resolved. Runs until ctx is cancelled.
func discoverLANContinuous(ctx context.Context, ch chan<- models.LANDevice) {
	defer close(ch)

	cmd := exec.CommandContext(ctx, "dns-sd", "-B", wendyServiceType, "local")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}

	seen := make(map[string]bool)
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

		inst := browseResult{
			instanceName: strings.Join(fields[6:], " "),
			domain:       fields[4],
		}

		if seen[inst.instanceName] {
			continue
		}
		seen[inst.instanceName] = true

		resolveCtx, resolveCancel := context.WithTimeout(ctx, 2*time.Second)
		dev, err := dnssdResolve(resolveCtx, inst)
		resolveCancel()
		if err != nil {
			dev = deviceFromBrowse(inst)
		}

		select {
		case ch <- dev:
		case <-ctx.Done():
			return
		}
	}

	_ = cmd.Wait()
}
