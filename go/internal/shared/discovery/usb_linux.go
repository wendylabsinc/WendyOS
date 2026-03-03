//go:build linux

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// discoverUSB uses lsusb to find USB-connected Wendy devices on Linux.
// lsusb output format: "Bus 001 Device 002: ID 1234:5678 Manufacturer Device Name"
func discoverUSB(ctx context.Context) ([]models.USBDevice, error) {
	cmd := exec.CommandContext(ctx, "lsusb")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running lsusb: %w", err)
	}

	var devices []models.USBDevice
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(strings.ToLower(line), "wendy") {
			continue
		}

		dev := models.USBDevice{
			IsWendyDevice: true,
		}

		// Extract "ID VVVV:PPPP"
		idIdx := strings.Index(line, "ID ")
		if idIdx >= 0 {
			rest := line[idIdx+3:]
			fields := strings.SplitN(rest, " ", 2)
			if len(fields) >= 1 {
				vidpid := strings.SplitN(fields[0], ":", 2)
				if len(vidpid) == 2 {
					dev.VendorID = fmt.Sprintf("0x%s", vidpid[0])
					dev.ProductID = fmt.Sprintf("0x%s", vidpid[1])
				}
			}
			if len(fields) >= 2 {
				dev.Name = strings.TrimSpace(fields[1])
				dev.DisplayName = dev.Name
			}
		}

		if dev.Name == "" {
			dev.Name = "Wendy Device"
			dev.DisplayName = dev.Name
		}

		devices = append(devices, dev)
	}
	return devices, nil
}
