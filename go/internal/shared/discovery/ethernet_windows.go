//go:build windows

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// netAdapterPowershell joins Get-NetAdapter with the first IPv4 address from
// Get-NetIPAddress for each adapter and emits a compact JSON array. ifIndex is
// the join key. SilentlyContinue prevents errors when an adapter has no IPv4
// address (e.g., link is down).
const netAdapterPowershell = `Get-NetAdapter | ForEach-Object {
  $adapter = $_
  $ip = (Get-NetIPAddress -InterfaceIndex $adapter.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1).IPAddress
  [PSCustomObject]@{
    Name = $adapter.Name
    InterfaceDescription = $adapter.InterfaceDescription
    MacAddress = $adapter.MacAddress
    LinkSpeed = $adapter.LinkSpeed
    IPAddress = $ip
  }
} | ConvertTo-Json -Compress`

type netAdapterEntry struct {
	Name                 string `json:"Name"`
	InterfaceDescription string `json:"InterfaceDescription"`
	MacAddress           string `json:"MacAddress"`
	LinkSpeed            string `json:"LinkSpeed"`
	IPAddress            string `json:"IPAddress"`
}

// discoverEthernet uses PowerShell to enumerate network adapters on Windows
// and returns those whose Name or InterfaceDescription contains "Wendy"
// (case-insensitive), matching the linux/darwin filter convention.
func discoverEthernet(ctx context.Context) ([]models.EthernetInterface, error) {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", netAdapterPowershell)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running Get-NetAdapter: %w", err)
	}
	return parseNetAdapterJSON(string(out)), nil
}

// parseNetAdapterJSON parses the ConvertTo-Json output produced by
// netAdapterPowershell and filters for Wendy interfaces. PowerShell omits the
// outer array when there is exactly one result, so we normalize.
func parseNetAdapterJSON(jsonOut string) []models.EthernetInterface {
	trimmed := strings.TrimSpace(jsonOut)
	if trimmed == "" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		trimmed = "[" + trimmed + "]"
	}

	var entries []netAdapterEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil
	}

	var devices []models.EthernetInterface
	for _, e := range entries {
		nameL := strings.ToLower(e.Name + " " + e.InterfaceDescription)
		if !strings.Contains(nameL, "wendy") {
			continue
		}
		devices = append(devices, models.EthernetInterface{
			Name:          e.Name,
			DisplayName:   e.InterfaceDescription,
			MACAddress:    e.MacAddress,
			IPAddress:     e.IPAddress,
			LinkSpeed:     e.LinkSpeed,
			IsWendyDevice: true,
		})
	}
	return devices
}
