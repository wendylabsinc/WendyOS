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

	"github.com/wendylabsinc/wendy/internal/shared/nmcli"
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

// splitNMCLI is the legacy local alias for nmcli.Split. Kept so the rest of
// this file reads the same; the underlying implementation also unescapes
// `\n`/`\r`/`\t` and forces a UTF-8 locale on every nmcli invocation.
func splitNMCLI(line string, fields int) []string {
	return nmcli.Split(line, fields)
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
// It fetches per-profile SSID + key-mgmt details in a single nmcli invocation
// so the cost is O(1) exec calls regardless of how many profiles exist.
func (n *NMCLINetworkManager) listKnownProfiles(ctx context.Context) ([]knownProfile, error) {
	// First pass: list wifi connections and their priorities.
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t",
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
		result = append(result, knownProfile{Name: fields[0], UUID: fields[1], Priority: prio})
	}

	if len(result) == 0 {
		return result, nil
	}

	// Second pass: fetch ssid + key-mgmt for every wifi profile in a single
	// `nmcli connection show UUID1 UUID2 ...` call. With `-g`, nmcli prints
	// one field value per line per connection in the order it received the
	// UUIDs, so 2 lines per profile (ssid, key-mgmt).
	args := []string{"-t", "-g",
		"802-11-wireless.ssid,802-11-wireless-security.key-mgmt",
		"connection", "show"}
	for _, p := range result {
		args = append(args, p.UUID)
	}
	dcmd := nmcli.Command(ctx, n.nmcliPath, args...)
	dout, derr := dcmd.Output()
	if derr != nil {
		// Fall back to name-as-ssid if the detail fetch fails so callers
		// still get a usable list.
		for i := range result {
			if result[i].SSID == "" {
				result[i].SSID = result[i].Name
			}
		}
		return result, nil
	}
	lines := strings.Split(strings.TrimRight(string(dout), "\n"), "\n")
	for i := range result {
		base := i * 2
		if base < len(lines) {
			result[i].SSID = unescapeNMCLI(lines[base])
		}
		if base+1 < len(lines) {
			result[i].Security = classifyKeyMgmt(lines[base+1])
		}
		if result[i].SSID == "" {
			result[i].SSID = result[i].Name
		}
	}
	return result, nil
}

func unescapeNMCLI(s string) string {
	return nmcli.Unescape(s)
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
	rescan := nmcli.Command(ctx, n.nmcliPath, "device", "wifi", "rescan")
	_ = rescan.Run()

	cmd := nmcli.Command(ctx, n.nmcliPath, "-t",
		"-f", "IN-USE,SSID,SIGNAL,SECURITY",
		"device", "wifi", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nmcli wifi list: %w", err)
	}

	known, knownErr := n.listKnownProfiles(ctx)
	if knownErr != nil {
		// Don't fail the whole scan — just surface a warning and emit
		// scan-only results. The TUI will still render, but known/priority
		// columns will be blank.
		n.logger.Warn("Failed to list saved WiFi profiles; continuing with scan-only results", zap.Error(knownErr))
	}
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

// SavedCredential describes a WiFi profile that the agent should persist via
// nmcli without necessarily activating it. Used by the config-partition
// provisioning path on first boot.
type SavedCredential struct {
	SSID     string
	Password string
	Priority int32
	Hidden   bool
	Security string // "wpa2" / "wpa3" / "open" / "" (autodetect)
}

// AddOrUpdateProfile adds or updates a NetworkManager profile for the given
// credential without activating it. If a profile with the same SSID already
// exists, its password / priority / hidden flag are updated in place.
func AddOrUpdateProfile(ctx context.Context, c SavedCredential) error {
	path := resolveNMCLIPath()
	if path == "" {
		return fmt.Errorf("nmcli not found")
	}
	return addOrUpdateProfile(ctx, path, c)
}

func addOrUpdateProfile(ctx context.Context, nmcliPath string, c SavedCredential) error {
	uuid, err := existingProfileUUID(ctx, nmcliPath, c.SSID)
	if err != nil {
		return err
	}
	if uuid != "" {
		return modifyProfile(ctx, nmcliPath, uuid, c)
	}
	return addProfile(ctx, nmcliPath, c)
}

// existingProfileUUID returns the UUID of the saved 802-11-wireless profile
// whose ssid field equals ssid. Returns ("", nil) when no such profile
// exists. Uses a single batched `nmcli connection show UUID1 UUID2 …` call
// so the cost is O(1) exec calls in the number of saved profiles.
func existingProfileUUID(ctx context.Context, nmcliPath, ssid string) (string, error) {
	out, err := nmcli.Command(ctx, nmcliPath, "-t",
		"-f", "NAME,UUID,TYPE", "connection", "show").Output()
	if err != nil {
		return "", fmt.Errorf("nmcli connection show: %w", err)
	}
	var uuids []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 3)
		if len(fields) < 3 || fields[2] != "802-11-wireless" {
			continue
		}
		uuids = append(uuids, fields[1])
	}
	if len(uuids) == 0 {
		return "", nil
	}
	args := append([]string{"-t", "-g", "802-11-wireless.ssid", "connection", "show"}, uuids...)
	dout, derr := nmcli.Command(ctx, nmcliPath, args...).Output()
	if derr != nil {
		return "", fmt.Errorf("nmcli connection show (ssids): %w", derr)
	}
	lines := strings.Split(strings.TrimRight(string(dout), "\n"), "\n")
	for i, uuid := range uuids {
		if i < len(lines) && unescapeNMCLI(lines[i]) == ssid {
			return uuid, nil
		}
	}
	return "", nil
}

func addProfile(ctx context.Context, nmcliPath string, c SavedCredential) error {
	args := []string{"connection", "add", "type", "wifi",
		"con-name", c.SSID,
		"ssid", c.SSID,
		"autoconnect", "yes",
	}
	if c.Priority != 0 {
		args = append(args, "connection.autoconnect-priority", strconv.Itoa(int(c.Priority)))
	}
	if c.Hidden {
		args = append(args, "802-11-wireless.hidden", "yes")
	}
	keyMgmt := keyMgmtFromHint(c.Security, c.Password)
	if keyMgmt != "" {
		args = append(args, "802-11-wireless-security.key-mgmt", keyMgmt)
		if c.Password != "" {
			args = append(args, "802-11-wireless-security.psk", c.Password)
		}
	}
	cmd := nmcli.Command(ctx, nmcliPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection add %s: %s: %w", c.SSID, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func modifyProfile(ctx context.Context, nmcliPath, uuid string, c SavedCredential) error {
	args := []string{"connection", "modify", uuid}
	if c.Priority != 0 {
		args = append(args, "connection.autoconnect-priority", strconv.Itoa(int(c.Priority)))
	}
	if c.Hidden {
		args = append(args, "802-11-wireless.hidden", "yes")
	}
	if c.Password != "" {
		km := keyMgmtFromHint(c.Security, c.Password)
		if km == "" {
			km = "wpa-psk"
		}
		args = append(args, "802-11-wireless-security.key-mgmt", km)
		args = append(args, "802-11-wireless-security.psk", c.Password)
	}
	if len(args) == 3 {
		// Nothing to change beyond the existing profile — still fine.
		return nil
	}
	cmd := nmcli.Command(ctx, nmcliPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection modify %s: %s: %w", c.SSID, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ActivateProfile brings up a saved profile (the equivalent of `nmcli
// connection up`). It is a no-op when the profile is already active.
func ActivateProfile(ctx context.Context, ssid string) error {
	path := resolveNMCLIPath()
	if path == "" {
		return fmt.Errorf("nmcli not found")
	}
	return activateProfile(ctx, path, ssid)
}

func activateProfile(ctx context.Context, nmcliPath, ssid string) error {
	uuid, err := existingProfileUUID(ctx, nmcliPath, ssid)
	if err != nil {
		return err
	}
	if uuid == "" {
		return fmt.Errorf("no saved profile for SSID %q", ssid)
	}
	cmd := nmcli.Command(ctx, nmcliPath, "connection", "up", uuid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection up %s: %s: %w", ssid, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// keyMgmtFromHint picks the nmcli `802-11-wireless-security.key-mgmt` value
// for a given security hint. When the hint is empty and a password is
// provided, defaults to wpa-psk (the most common case on consumer APs).
func keyMgmtFromHint(hint, password string) string {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case "open", "none":
		return ""
	case "wep":
		return "none" // nmcli uses "none" + wep-key-type for WEP
	case "wpa", "wpa-psk", "wpa2", "wpa2-psk":
		return "wpa-psk"
	case "wpa3", "sae":
		return "sae"
	case "wpa-eap", "wpa2-enterprise", "enterprise":
		return "wpa-eap"
	}
	if password != "" {
		return "wpa-psk"
	}
	return ""
}

// ConnectToWiFi connects to a WiFi network.
//
// For visible networks we let `nmcli device wifi connect` auto-detect the
// security from the scan. For hidden networks or when the caller supplied an
// explicit security hint (both are cases where autodetection can't be trusted
// — `device wifi connect` has no way to express key-mgmt), we create/update a
// profile with the requested key-mgmt and activate it.
func (n *NMCLINetworkManager) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error {
	ssid := req.GetSsid()
	hidden := req.GetHidden()
	secHint := securityHintFromProto(req.GetSecurity())

	// Snapshot whether a saved profile exists *before* we touch nmcli so we
	// can roll back any profile we (or `nmcli device wifi connect`) created
	// when activation later fails. Without this, a failed authentication
	// leaves the SSID with a saved profile, and ListWiFiNetworks then reports
	// the network as IsKnown — making the UI show ★ for a connection that
	// never actually succeeded. We deliberately do NOT touch a pre-existing
	// profile, since that one holds credentials from a previously-working
	// connection that a single mistyped retry shouldn't destroy.
	preExistingUUID, err := existingProfileUUID(ctx, n.nmcliPath, ssid)
	if err != nil {
		return fmt.Errorf("checking for existing WiFi profile: %w", err)
	}

	if hidden || secHint != "" {
		cred := SavedCredential{
			SSID:     ssid,
			Password: req.GetPassword(),
			Hidden:   hidden,
			Security: secHint,
		}
		if err := addOrUpdateProfile(ctx, n.nmcliPath, cred); err != nil {
			return fmt.Errorf("preparing WiFi profile: %w", err)
		}
		if err := activateProfile(ctx, n.nmcliPath, ssid); err != nil {
			n.cleanupTransientProfile(ctx, ssid, preExistingUUID)
			return err
		}
		n.logger.Info("Connected to WiFi via profile",
			zap.String("ssid", ssid),
			zap.Bool("hidden", hidden),
			zap.String("security", secHint))
		return nil
	}

	if err := runNMCLIConnect(ctx, n.nmcliPath, ssid, req.GetPassword(), hidden); err != nil {
		n.cleanupTransientProfile(ctx, ssid, preExistingUUID)
		return err
	}
	n.logger.Info("Connected to WiFi", zap.String("ssid", ssid))
	return nil
}

// cleanupTransientProfile removes a saved nmcli profile for ssid if and only
// if it didn't already exist before the connect attempt. preExistingUUID is
// what existingProfileUUID returned before activation: an empty value means
// the profile we now see was created during the failed attempt itself, so
// it's safe to delete. Cleanup is best-effort — if it fails we just log,
// since the original connect error is what the caller actually needs.
func (n *NMCLINetworkManager) cleanupTransientProfile(ctx context.Context, ssid, preExistingUUID string) {
	if preExistingUUID != "" {
		return
	}
	uuid, err := existingProfileUUID(ctx, n.nmcliPath, ssid)
	if err != nil || uuid == "" {
		return
	}
	cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "delete", uuid)
	if out, err := cmd.CombinedOutput(); err != nil {
		n.logger.Warn("failed to clean up profile after failed connect",
			zap.String("ssid", ssid),
			zap.String("output", strings.TrimSpace(string(out))),
			zap.Error(err))
	}
}

// securityHintFromProto maps the proto enum to the free-form string consumed
// by keyMgmtFromHint. Returns "" for UNSPECIFIED so callers can treat that as
// "no hint, use scan autodetect".
func securityHintFromProto(t agentpb.WiFiSecurityType) string {
	switch t {
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN:
		return "open"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP:
		return "wep"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA_PSK:
		return "wpa-psk"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK:
		return "wpa2-psk"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE:
		return "sae"
	case agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE:
		return "wpa2-enterprise"
	}
	return ""
}

func runNMCLIConnect(ctx context.Context, nmcliPath, ssid, password string, hidden bool) error {
	args := []string{"device", "wifi", "connect", ssid}
	if password != "" {
		args = append(args, "password", password)
	}
	if hidden {
		args = append(args, "hidden", "yes")
	}
	cmd := nmcli.Command(ctx, nmcliPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nmcli connect: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// GetWiFiStatus returns the current WiFi connection status.
func (n *NMCLINetworkManager) GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error) {
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t", "-f", "TYPE,STATE,CONNECTION", "device", "status")
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
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t", "-f", "DEVICE,TYPE", "device", "status")
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

	disconnCmd := nmcli.Command(ctx, n.nmcliPath, "device", "disconnect", wifiDevice)
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
	cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "modify", uuid,
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
		cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "modify", kp.UUID,
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
	cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "delete", uuid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n.logger.Info("Forgot WiFi network", zap.String("ssid", ssid))
	return nil
}

// ForgetWiFiNetworkByUUID deletes a saved profile directly by its NetworkManager UUID.
func (n *NMCLINetworkManager) ForgetWiFiNetworkByUUID(ctx context.Context, uuid string) error {
	cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "delete", uuid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n.logger.Info("Forgot WiFi network", zap.String("uuid", uuid))
	return nil
}

// SetWiFiNetworkPriorityByUUID sets the autoconnect priority for a profile identified by UUID.
func (n *NMCLINetworkManager) SetWiFiNetworkPriorityByUUID(ctx context.Context, uuid string, priority int32) error {
	cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "modify", uuid,
		"connection.autoconnect-priority", strconv.Itoa(int(priority)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli modify: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n.logger.Info("Set WiFi priority", zap.String("uuid", uuid), zap.Int32("priority", priority))
	return nil
}

// ReorderKnownWiFiNetworksByUUID assigns descending priorities to profiles identified by UUID.
// The first UUID in the slice gets the highest priority.
func (n *NMCLINetworkManager) ReorderKnownWiFiNetworksByUUID(ctx context.Context, orderedUUIDs []string) error {
	if len(orderedUUIDs) == 0 {
		return nil
	}
	top := int32(len(orderedUUIDs))
	for i, uuid := range orderedUUIDs {
		newPrio := top - int32(i)
		cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "modify", uuid,
			"connection.autoconnect-priority", strconv.Itoa(int(newPrio)))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("nmcli modify %s: %s: %w", uuid, strings.TrimSpace(string(out)), err)
		}
	}
	n.logger.Info("Reordered WiFi networks", zap.Strings("order", orderedUUIDs))
	return nil
}
