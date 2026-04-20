// Package network implements WiFi management using NetworkManager (nmcli).
package network

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// NMCLINetworkManager implements services.NetworkManager using nmcli commands.
type NMCLINetworkManager struct {
	logger    *zap.Logger
	nmcliPath string
}

// NewNMCLINetworkManager creates a new NMCLINetworkManager.
// Returns nil if nmcli is not available on the system.
// The resolved path is stored so that later exec calls succeed even if
// PATH changes (e.g. when running under a systemd service).
func NewNMCLINetworkManager(logger *zap.Logger) *NMCLINetworkManager {
	path, err := exec.LookPath("nmcli")
	if err != nil {
		// Systemd services may have a restricted PATH; check common locations.
		for _, p := range []string{"/usr/bin/nmcli", "/usr/sbin/nmcli", "/usr/local/bin/nmcli"} {
			if _, statErr := os.Stat(p); statErr == nil {
				path = p
				break
			}
		}
		if path == "" {
			logger.Warn("nmcli not found, WiFi management will be unavailable")
			return nil
		}
	}
	return &NMCLINetworkManager{logger: logger, nmcliPath: path}
}

// ListWiFiNetworks scans for and lists available WiFi networks.
func (n *NMCLINetworkManager) ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	// Rescan first.
	rescan := exec.CommandContext(ctx, n.nmcliPath, "device", "wifi", "rescan")
	_ = rescan.Run() // Ignore errors; rescan may fail if already scanning.

	// List networks.
	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t", "-f", "SSID,SIGNAL,SECURITY,IN-USE", "device", "wifi", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nmcli wifi list: %w", err)
	}

	var networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.SplitN(line, ":", 4)
		if len(fields) < 4 {
			continue
		}

		ssid := fields[0]
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true

		net := &agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			Ssid: ssid,
		}
		if signal, err := strconv.Atoi(fields[1]); err == nil {
			s := int32(signal)
			net.SignalStrength = &s
		}
		networks = append(networks, net)
	}

	n.logger.Info("Listed WiFi networks", zap.Int("count", len(networks)))
	return networks, nil
}

// Connect connects to a WiFi network using nmcli, resolving the binary path
// with the same fallback logic used by NewNMCLINetworkManager. Suitable for
// callers that don't hold an NMCLINetworkManager instance.
func Connect(ctx context.Context, ssid, password string) error {
	path, err := exec.LookPath("nmcli")
	if err != nil {
		for _, p := range []string{"/usr/bin/nmcli", "/usr/sbin/nmcli", "/usr/local/bin/nmcli"} {
			if _, statErr := os.Stat(p); statErr == nil {
				path = p
				break
			}
		}
		if path == "" {
			return fmt.Errorf("nmcli not found")
		}
	}
	return runNMCLIConnect(ctx, path, ssid, password)
}

// ConnectToWiFi connects to a WiFi network.
func (n *NMCLINetworkManager) ConnectToWiFi(ctx context.Context, ssid, password string) error {
	if err := runNMCLIConnect(ctx, n.nmcliPath, ssid, password); err != nil {
		return err
	}
	n.logger.Info("Connected to WiFi", zap.String("ssid", ssid))
	return nil
}

func runNMCLIConnect(ctx context.Context, nmcliPath, ssid, password string) error {
	var cmd *exec.Cmd
	if password != "" {
		cmd = exec.CommandContext(ctx, nmcliPath, "device", "wifi", "connect", ssid, "password", password)
	} else {
		cmd = exec.CommandContext(ctx, nmcliPath, "device", "wifi", "connect", ssid)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nmcli connect: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// GetWiFiStatus returns the current WiFi connection status.
func (n *NMCLINetworkManager) GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error) {
	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t", "-f", "TYPE,STATE,CONNECTION", "device", "status")
	output, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("nmcli device status: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.SplitN(line, ":", 3)
		if len(fields) < 3 {
			continue
		}

		devType := fields[0]
		state := fields[1]
		connName := fields[2]

		if devType == "wifi" && state == "connected" {
			return true, connName, nil
		}
	}

	return false, "", nil
}

// DisconnectWiFi disconnects from the current WiFi network.
func (n *NMCLINetworkManager) DisconnectWiFi(ctx context.Context) error {
	// Find the WiFi device name.
	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t", "-f", "DEVICE,TYPE", "device", "status")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("nmcli device status: %w", err)
	}

	var wifiDevice string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ":", 2)
		if len(fields) == 2 && fields[1] == "wifi" {
			wifiDevice = fields[0]
			break
		}
	}

	if wifiDevice == "" {
		return fmt.Errorf("no WiFi device found")
	}

	disconnCmd := exec.CommandContext(ctx, n.nmcliPath, "device", "disconnect", wifiDevice)
	if disconnOutput, err := disconnCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli disconnect: %s: %w", strings.TrimSpace(string(disconnOutput)), err)
	}

	n.logger.Info("Disconnected from WiFi", zap.String("device", wifiDevice))
	return nil
}
