// Package network implements WiFi management using NetworkManager (nmcli).
package network

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
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
	path := resolveNMCLIPath()
	if path == "" {
		logger.Warn("nmcli not found, WiFi management will be unavailable")
		return nil
	}
	return &NMCLINetworkManager{logger: logger, nmcliPath: path}
}

func resolveNMCLIPath() string {
	if path, err := exec.LookPath("nmcli"); err == nil {
		return path
	}
	// Systemd services may have a restricted PATH; check common locations.
	for _, p := range []string{"/usr/bin/nmcli", "/usr/sbin/nmcli", "/usr/local/bin/nmcli"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// nmcli -t output escapes embedded colons as `\:` — we need to respect that
// when splitting. splitNMCLI splits a single record into `fields` substrings,
// undoing the backslash escaping that nmcli applies.
func splitNMCLI(line string, fields int) []string {
	out := make([]string, 0, fields)
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			cur.WriteByte(line[i+1])
			i++
			continue
		}
		if c == ':' && len(out) < fields-1 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// classifySecurity converts an nmcli SECURITY string (e.g. "WPA2", "WPA1 WPA2",
// "WPA3", "WPA2 802.1X", "WEP", "") into a WiFiSecurityType enum.
func classifySecurity(s string) agentpb.WiFiSecurityType {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch {
	case s == "" || s == "--" || s == "NONE":
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN
	case strings.Contains(s, "802.1X"):
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE
	case strings.Contains(s, "WPA3") || strings.Contains(s, "SAE"):
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE
	case strings.Contains(s, "WPA2"):
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK
	case strings.Contains(s, "WPA"):
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA_PSK
	case strings.Contains(s, "WEP"):
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP
	default:
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED
	}
}

type knownProfile struct {
	Name     string
	UUID     string
	Priority int32
	Security agentpb.WiFiSecurityType
	SSID     string
}

// listKnownProfiles returns one entry per saved 802-11-wireless connection.
func (n *NMCLINetworkManager) listKnownProfiles(ctx context.Context) ([]knownProfile, error) {
	// Ask nmcli for all wifi connection profiles.
	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t",
		"-f", "NAME,UUID,TYPE,AUTOCONNECT-PRIORITY",
		"connection", "show")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nmcli connection show: %w", err)
	}

	var result []knownProfile
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 4)
		if len(fields) < 4 {
			continue
		}
		if fields[2] != "802-11-wireless" {
			continue
		}
		var prio int32
		if p, err := strconv.Atoi(fields[3]); err == nil {
			prio = int32(p)
		}
		kp := knownProfile{Name: fields[0], UUID: fields[1], Priority: prio}

		// Fetch the SSID + key-mgmt for this profile.
		dcmd := exec.CommandContext(ctx, n.nmcliPath, "-t", "-g",
			"802-11-wireless.ssid,802-11-wireless-security.key-mgmt",
			"connection", "show", kp.UUID)
		dout, derr := dcmd.Output()
		if derr == nil {
			lines := strings.Split(strings.TrimRight(string(dout), "\n"), "\n")
			if len(lines) >= 1 {
				kp.SSID = unescapeNMCLI(lines[0])
			}
			if len(lines) >= 2 {
				kp.Security = classifyKeyMgmt(lines[1])
			}
		}
		if kp.SSID == "" {
			kp.SSID = kp.Name
		}
		result = append(result, kp)
	}
	return result, nil
}

func unescapeNMCLI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func classifyKeyMgmt(k string) agentpb.WiFiSecurityType {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "none", "":
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN
	case "wpa-psk":
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK
	case "sae":
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE
	case "wpa-eap", "ieee8021x":
		return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE
	}
	return agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED
}

// ListWiFiNetworks scans for and lists available WiFi networks, merging scan
// results with saved profiles so the response carries is_known/priority/security.
func (n *NMCLINetworkManager) ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	rescan := exec.CommandContext(ctx, n.nmcliPath, "device", "wifi", "rescan")
	_ = rescan.Run()

	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t",
		"-f", "IN-USE,SSID,SIGNAL,SECURITY",
		"device", "wifi", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nmcli wifi list: %w", err)
	}

	known, _ := n.listKnownProfiles(ctx)
	knownBySSID := make(map[string]knownProfile, len(known))
	for _, k := range known {
		knownBySSID[k.SSID] = k
	}

	var networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 4)
		if len(fields) < 4 {
			continue
		}
		inUse := strings.TrimSpace(fields[0]) == "*"
		ssid := fields[1]
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true

		net := &agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			Ssid:     ssid,
			Security: classifySecurity(fields[3]),
		}
		if signal, err := strconv.Atoi(fields[2]); err == nil {
			s := int32(signal)
			net.SignalStrength = &s
		}
		if inUse {
			net.IsConnected = true
		}
		if kp, ok := knownBySSID[ssid]; ok {
			net.IsKnown = true
			p := kp.Priority
			net.Priority = &p
			if net.Security == agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED {
				net.Security = kp.Security
			}
			delete(knownBySSID, ssid)
		}
		networks = append(networks, net)
	}

	// Include saved-but-not-currently-visible networks so the TUI can still
	// rank them.
	for _, kp := range knownBySSID {
		p := kp.Priority
		networks = append(networks, &agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			Ssid:     kp.SSID,
			Security: kp.Security,
			IsKnown:  true,
			Priority: &p,
		})
	}

	n.logger.Info("Listed WiFi networks", zap.Int("count", len(networks)))
	return networks, nil
}

// Connect connects to a WiFi network using nmcli, resolving the binary path
// with the same fallback logic used by NewNMCLINetworkManager. Suitable for
// callers that don't hold an NMCLINetworkManager instance.
func Connect(ctx context.Context, ssid, password string) error {
	path := resolveNMCLIPath()
	if path == "" {
		return fmt.Errorf("nmcli not found")
	}
	return runNMCLIConnect(ctx, path, ssid, password, false)
}

// ConnectToWiFi connects to a WiFi network.
func (n *NMCLINetworkManager) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error {
	hidden := req.GetHidden()
	if err := runNMCLIConnect(ctx, n.nmcliPath, req.GetSsid(), req.GetPassword(), hidden); err != nil {
		return err
	}
	n.logger.Info("Connected to WiFi", zap.String("ssid", req.GetSsid()))
	return nil
}

func runNMCLIConnect(ctx context.Context, nmcliPath, ssid, password string, hidden bool) error {
	args := []string{"device", "wifi", "connect", ssid}
	if password != "" {
		args = append(args, "password", password)
	}
	if hidden {
		args = append(args, "hidden", "yes")
	}
	cmd := exec.CommandContext(ctx, nmcliPath, args...)
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
		fields := splitNMCLI(scanner.Text(), 3)
		if len(fields) < 3 {
			continue
		}
		if fields[0] == "wifi" && fields[1] == "connected" {
			return true, fields[2], nil
		}
	}
	return false, "", nil
}

// DisconnectWiFi disconnects from the current WiFi network.
func (n *NMCLINetworkManager) DisconnectWiFi(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, n.nmcliPath, "-t", "-f", "DEVICE,TYPE", "device", "status")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("nmcli device status: %w", err)
	}

	var wifiDevice string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 2)
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

// ListKnownWiFiNetworks returns saved WiFi profiles ordered by descending priority.
func (n *NMCLINetworkManager) ListKnownWiFiNetworks(ctx context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error) {
	profiles, err := n.listKnownProfiles(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(profiles, func(i, j int) bool {
		return profiles[i].Priority > profiles[j].Priority
	})
	out := make([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, &agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{
			Ssid:     p.SSID,
			Uuid:     p.UUID,
			Priority: p.Priority,
			Security: p.Security,
		})
	}
	return out, nil
}

func (n *NMCLINetworkManager) uuidForSSID(ctx context.Context, ssid string) (string, error) {
	profiles, err := n.listKnownProfiles(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range profiles {
		if p.SSID == ssid {
			return p.UUID, nil
		}
	}
	return "", fmt.Errorf("no saved profile for SSID %q", ssid)
}

// SetWiFiNetworkPriority updates a saved profile's autoconnect priority.
func (n *NMCLINetworkManager) SetWiFiNetworkPriority(ctx context.Context, ssid string, priority int32) error {
	uuid, err := n.uuidForSSID(ctx, ssid)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, n.nmcliPath, "connection", "modify", uuid,
		"connection.autoconnect-priority", strconv.Itoa(int(priority)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli modify: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n.logger.Info("Set WiFi priority", zap.String("ssid", ssid), zap.Int32("priority", priority))
	return nil
}

// ReorderKnownWiFiNetworks assigns descending priorities to the given SSIDs.
// The first SSID in the slice gets the highest priority.
func (n *NMCLINetworkManager) ReorderKnownWiFiNetworks(ctx context.Context, orderedSSIDs []string) error {
	if len(orderedSSIDs) == 0 {
		return nil
	}
	profiles, err := n.listKnownProfiles(ctx)
	if err != nil {
		return err
	}
	bySSID := make(map[string]knownProfile, len(profiles))
	for _, p := range profiles {
		bySSID[p.SSID] = p
	}
	// Use priorities starting at len(orderedSSIDs) and counting down so the
	// top entry gets the largest value.
	top := int32(len(orderedSSIDs))
	for i, ssid := range orderedSSIDs {
		kp, ok := bySSID[ssid]
		if !ok {
			return fmt.Errorf("no saved profile for SSID %q", ssid)
		}
		newPrio := top - int32(i)
		if kp.Priority == newPrio {
			continue
		}
		cmd := exec.CommandContext(ctx, n.nmcliPath, "connection", "modify", kp.UUID,
			"connection.autoconnect-priority", strconv.Itoa(int(newPrio)))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("nmcli modify %s: %s: %w", ssid, strings.TrimSpace(string(out)), err)
		}
	}
	n.logger.Info("Reordered WiFi networks", zap.Strings("order", orderedSSIDs))
	return nil
}

// ForgetWiFiNetwork deletes a saved profile by SSID.
func (n *NMCLINetworkManager) ForgetWiFiNetwork(ctx context.Context, ssid string) error {
	uuid, err := n.uuidForSSID(ctx, ssid)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, n.nmcliPath, "connection", "delete", uuid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n.logger.Info("Forgot WiFi network", zap.String("ssid", ssid))
	return nil
}
