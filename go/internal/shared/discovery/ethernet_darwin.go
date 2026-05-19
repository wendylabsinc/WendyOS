//go:build darwin

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// discoverEthernet enumerates network interfaces on macOS and returns those
// whose display name or BSD name contains "Wendy" (USB-Ethernet gadget mode).
func discoverEthernet(ctx context.Context) ([]models.EthernetInterface, error) {
	// networksetup -listallhardwareports gives us display names mapped to BSD names.
	// Output format:
	//   Hardware Port: <display name>
	//   Device: <bsd name>
	//   Ethernet Address: <mac>
	cmd := exec.CommandContext(ctx, "networksetup", "-listallhardwareports")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running networksetup: %w", err)
	}

	type hwPort struct {
		displayName string
		bsdName     string
		macAddress  string
	}

	var ports []hwPort
	var current hwPort
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			current = hwPort{displayName: strings.TrimPrefix(line, "Hardware Port: ")}
		case strings.HasPrefix(line, "Device:"):
			current.bsdName = strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
		case strings.HasPrefix(line, "Ethernet Address:"):
			current.macAddress = strings.TrimSpace(strings.TrimPrefix(line, "Ethernet Address:"))
			if current.bsdName != "" {
				ports = append(ports, current)
			}
			current = hwPort{}
		}
	}

	// Filter for Wendy interfaces.
	var devices []models.EthernetInterface
	for _, p := range ports {
		nameL := strings.ToLower(p.displayName + " " + p.bsdName)
		if !strings.Contains(nameL, "wendy") {
			continue
		}

		iface := models.EthernetInterface{
			Name:          p.bsdName,
			DisplayName:   p.displayName,
			MACAddress:    p.macAddress,
			IsWendyDevice: true,
		}

		// Get IP address from ifconfig.
		iface.IPAddress = getInterfaceIP(ctx, p.bsdName)

		// Get link speed from ifconfig media line.
		iface.LinkSpeed = getInterfaceLinkSpeed(ctx, p.bsdName)

		devices = append(devices, iface)
	}
	return devices, nil
}

func getInterfaceIP(ctx context.Context, bsdName string) string {
	cmd := exec.CommandContext(ctx, "ifconfig", bsdName)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "inet ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	return ""
}

func darwinInterfaceDisplayName(ctx context.Context, bsdName string) string {
	if bsdName == "" {
		return ""
	}

	cmd := exec.CommandContext(ctx, "networksetup", "-listallhardwareports")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var currentDisplayName string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			currentDisplayName = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			if strings.TrimSpace(strings.TrimPrefix(line, "Device:")) == bsdName {
				return currentDisplayName
			}
		}
	}
	return ""
}

// linkSpeedRe matches speed values in ifconfig media lines, e.g. "1000baseT", "10Gbase-T".
var linkSpeedRe = regexp.MustCompile(`(\d+\.?\d*)(G)?base`)

func getInterfaceLinkSpeed(ctx context.Context, bsdName string) string {
	cmd := exec.CommandContext(ctx, "ifconfig", bsdName)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "media:") {
			continue
		}
		matches := linkSpeedRe.FindStringSubmatch(line)
		if len(matches) < 2 {
			return ""
		}
		speed := matches[1]
		if matches[2] == "G" {
			return speed + " Gbps"
		}
		var mbps float64
		fmt.Sscanf(speed, "%f", &mbps)
		if mbps >= 1000 {
			return fmt.Sprintf("%.0f Gbps", mbps/1000)
		}
		return fmt.Sprintf("%.0f Mbps", mbps)
	}
	return ""
}
