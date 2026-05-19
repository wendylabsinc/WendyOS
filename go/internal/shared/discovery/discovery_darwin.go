//go:build darwin

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
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
	indexes := make(map[string]int)

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
		devices = appendPreferredLANDevice(devices, indexes, key, dev)
	}

	return devices, nil
}

type browseResult struct {
	instanceName  string
	domain        string
	interfaceName string
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
			interfaceName := ""
			if interfaceIndex, err := strconv.Atoi(fields[3]); err == nil {
				if iface, ifaceErr := net.InterfaceByIndex(interfaceIndex); ifaceErr == nil {
					interfaceName = iface.Name
				}
			}
			lineCh <- parsedLine{
				result: browseResult{
					instanceName:  strings.Join(fields[6:], " "),
					domain:        fields[4],
					interfaceName: interfaceName,
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
			key := pl.result.instanceName + "%" + pl.result.interfaceName
			if pl.ok && !seen[key] {
				seen[key] = true
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

	// parseTXT extracts key=value pairs from a dns-sd TXT record line.
	// dns-sd escapes spaces inside values as "\ "; we split on unescaped
	// whitespace so that values like "Dynamic\ Cosmos" round-trip correctly.
	parseTXT := func(line string) {
		var fields []string
		var cur strings.Builder
		for i := 0; i < len(line); i++ {
			if line[i] == '\\' && i+1 < len(line) && (line[i+1] == ' ' || line[i+1] == '\t') {
				cur.WriteByte(line[i+1])
				i++
			} else if line[i] == ' ' || line[i] == '\t' {
				if cur.Len() > 0 {
					fields = append(fields, cur.String())
					cur.Reset()
				}
			} else {
				cur.WriteByte(line[i])
			}
		}
		if cur.Len() > 0 {
			fields = append(fields, cur.String())
		}
		for _, field := range fields {
			if k, v, ok := strings.Cut(field, "="); ok {
				txtRecords[k] = v
			}
		}
	}

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

			// TXT records on the same line (some versions).
			parseTXT(line)

			// TXT records are typically on the next line indented with a space.
			if scanner.Scan() {
				parseTXT(scanner.Text())
			}

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

	dev := models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		Port:          port,
		IsMTLS:        txtRecords["tls"] == "true",
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}
	setLANNetworkInterface(&dev, inst.interfaceName, darwinInterfaceDisplayName(ctx, inst.interfaceName), getInterfaceLinkSpeed(ctx, inst.interfaceName))
	return dev, nil
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

// unescapeDNSSDName converts dns-sd's backslash-escaped spaces ("\ ") back to
// regular spaces, matching the display form of an mDNS service instance name.
func unescapeDNSSDName(s string) string {
	return strings.ReplaceAll(s, `\ `, " ")
}

func deviceFromBrowse(inst browseResult) models.LANDevice {
	displayName := unescapeDNSSDName(inst.instanceName)

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

	dev := models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		Port:          port,
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}
	setLANNetworkInterface(&dev, inst.interfaceName, "", "")
	return dev
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
