// Package network implements WiFi management using NetworkManager (nmcli).
package network

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	"github.com/wendylabsinc/wendy/go/internal/shared/nmcli"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// NMCLINetworkManager implements services.NetworkManager using nmcli commands.
type NMCLINetworkManager struct {
	logger    *zap.Logger
	nmcliPath string

	// scanMu guards lastScan and scanning. The agent builds a single manager
	// and hands it to the v1 and v2 gRPC services and the BLE dispatcher, so
	// they all share one scan rate limit (see scanWiFi).
	scanMu sync.Mutex
	// lastScan is when the last scan the agent asked for returned. Zero
	// until the first one.
	lastScan time.Time
	// scanning is closed when the scan in progress returns. Nil while no
	// scan is in progress.
	scanning chan struct{}
}

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

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading nmcli connection list: %w", err)
	}

	uuids := make([]string, len(result))
	for i := range result {
		uuids[i] = result[i].UUID
	}
	profiles, _ := readWiFiProfiles(ctx, n.nmcliPath, uuids)
	for i := range result {
		p, ok := profiles[result[i].UUID]
		if !ok || p.SSID == "" {
			// The name still identifies the profile when its details can't be read.
			result[i].SSID = result[i].Name
			continue
		}
		result[i].SSID = p.SSID
		result[i].Security = classifyKeyMgmt(p.KeyMgmt)
	}
	return result, nil
}

const (
	uuidField    = "connection.uuid"
	ifaceField   = "connection.interface-name"
	modeField    = "802-11-wireless.mode"
	ssidField    = "802-11-wireless.ssid"
	keyMgmtField = "802-11-wireless-security.key-mgmt"
	authAlgField = "802-11-wireless-security.auth-alg"
	// profileFields ends with the SSID: values come back raw, and the SSID is the
	// only field that can span lines.
	profileFields = ifaceField + "," + modeField + "," + keyMgmtField + "," + authAlgField + "," + ssidField

	// nmcliNoSuchConnection is nmcli's exit status for a profile that doesn't exist.
	nmcliNoSuchConnection = 10
)

// wifiProfile holds the fields read back from one saved WiFi profile.
type wifiProfile struct {
	SSID    string
	KeyMgmt string
	AuthAlg string
	Iface   string // the device the profile is bound to, if any
	Mode    string
}

// joinsWithPassphrase reports whether p joins networks as a client with a WPA
// passphrase.
func (p wifiProfile) joinsWithPassphrase() bool {
	return (p.Mode == "" || p.Mode == "infrastructure") && (p.KeyMgmt == "wpa-psk" || p.KeyMgmt == "sae")
}

// wifiProfileUUIDs lists the UUIDs of the saved WiFi profiles.
func wifiProfileUUIDs(ctx context.Context, nmcliPath string) ([]string, error) {
	out, err := nmcli.Command(ctx, nmcliPath, "-t", "-f", "UUID,TYPE", "connection", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("nmcli connection show: %w", err)
	}
	var uuids []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 2)
		if len(fields) == 2 && fields[1] == "802-11-wireless" {
			uuids = append(uuids, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading nmcli connection list: %w", err)
	}
	return uuids, nil
}

// readWiFiProfiles reads the given profiles in one nmcli call, or one call each
// when that output can't be split. Profiles that no longer exist are left out.
func readWiFiProfiles(ctx context.Context, nmcliPath string, uuids []string) (map[string]wifiProfile, error) {
	if len(uuids) == 0 {
		return map[string]wifiProfile{}, nil
	}
	args := append([]string{"--escape", "no", "-t", "-f", uuidField + "," + profileFields,
		"connection", "show"}, uuids...)
	if out, err := nmcli.Command(ctx, nmcliPath, args...).Output(); err == nil {
		if profiles, ok := splitWiFiProfiles(string(out), uuids); ok {
			return profiles, nil
		}
	}
	profiles := make(map[string]wifiProfile, len(uuids))
	for _, uuid := range uuids {
		p, err := readWiFiProfile(ctx, nmcliPath, uuid)
		if profileGone(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		profiles[uuid] = p
	}
	return profiles, nil
}

// splitWiFiProfiles splits batched output at each record's own uuid line, which
// is longer than any SSID can be, so an SSID can never fake a boundary.
func splitWiFiProfiles(out string, uuids []string) (map[string]wifiProfile, bool) {
	profiles := make(map[string]wifiProfile, len(uuids))
	for i, uuid := range uuids {
		rest, ok := strings.CutPrefix(out, uuidField+":"+uuid+"\n")
		if !ok {
			return nil, false
		}
		record := rest
		if i+1 < len(uuids) {
			end := strings.Index(rest, "\n\n"+uuidField+":"+uuids[i+1]+"\n")
			if end < 0 {
				return nil, false
			}
			record, out = rest[:end+1], rest[end+2:]
		}
		p, err := parseWiFiProfile(record)
		if err != nil {
			return nil, false
		}
		profiles[uuid] = p
	}
	return profiles, true
}

// readWiFiProfile reads one profile on its own.
func readWiFiProfile(ctx context.Context, nmcliPath, uuid string) (wifiProfile, error) {
	out, err := nmcli.Command(ctx, nmcliPath, "--escape", "no", "-t", "-f", profileFields,
		"connection", "show", uuid).Output()
	if err != nil {
		return wifiProfile{}, fmt.Errorf("nmcli connection show %s: %w", uuid, err)
	}
	return parseWiFiProfile(string(out))
}

// parseWiFiProfile parses readWiFiProfile's output; a profile without a
// security setting has no security lines.
func parseWiFiProfile(out string) (wifiProfile, error) {
	var p wifiProfile
	p.Iface, out = cutField(out, ifaceField)
	p.Mode, out = cutField(out, modeField)
	p.KeyMgmt, out = cutField(out, keyMgmtField)
	p.AuthAlg, out = cutField(out, authAlgField)
	ssid, ok := strings.CutPrefix(out, ssidField+":")
	if !ok {
		return wifiProfile{}, fmt.Errorf("unexpected nmcli profile output %q", out)
	}
	p.SSID = strings.TrimSuffix(ssid, "\n")
	return p, nil
}

// cutField splits a leading "field:value" line off out, if out starts with one.
func cutField(out, field string) (value, rest string) {
	line, ok := strings.CutPrefix(out, field+":")
	if !ok {
		return "", out
	}
	value, rest, _ = strings.Cut(line, "\n")
	return value, rest
}

// profileGone reports whether err means the profile no longer exists.
func profileGone(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == nmcliNoSuchConnection
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

// ListWiFiNetworks lists available WiFi networks, merging scan results with
// saved profiles so the response carries is_known/priority/security. It scans
// first at most once per scanInterval (see scanWiFi).
func (n *NMCLINetworkManager) ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	output, err := n.scanWiFi(ctx)
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

	networks := parseWiFiList(string(output), known)
	n.logger.Info("Listed WiFi networks", zap.Int("count", len(networks)))
	return networks, nil
}

// scanInterval is the least time between two scans the agent asks for, counted
// from when the previous one returned. List calls inside it get
// NetworkManager's current list without a scan. Why 30 s:
//
//   - The WiFi screen (`wendy device wifi`) polls every 2.5 s, and a scan on a
//     Jetson Orin Nano holds the radio for 3.8–10.6 s. A scan per call would
//     keep the radio scanning for as long as the screen is open.
//   - Realtek's vendor drivers, rtl88x2ce on the Orin Nano developer kit
//     among them, refuse a scan while the link is busy, unless the previous
//     scan request is younger than scan_interval_thr (16 s by default in
//     Realtek's non-Android builds). Asking more often than that gets past
//     the check that protects a busy link.
//   - nmcli itself treats a list from the last 30 s as fresh.
const scanInterval = 30 * time.Second

// scanTimeout bounds the wait for a scan. nmcli stops waiting after 15 s and
// prints the list it has, so this only catches an nmcli or NetworkManager
// that never answers.
var scanTimeout = 20 * time.Second

// nmcliTimeout bounds the nmcli calls that don't scan, `device status` and
// `device wifi list --rescan no`. They only read NetworkManager's state, so
// this too only catches an nmcli or NetworkManager that never answers.
var nmcliTimeout = 10 * time.Second

// scanWiFi returns `nmcli -t -f IN-USE,SSID,SIGNAL,SECURITY device wifi list`
// output. Across all callers, the agent asks for a scan at most once per
// scanInterval. A call that starts the scan or arrives during it waits for
// it, or for its own ctx, whichever comes first. Then every call gets
// NetworkManager's current list.
//
// The scan runs on its own goroutine, bounded by scanTimeout rather than by
// the ctx of the call that started it. It serves every call waiting on it,
// so a caller that goes away (a closed picker, a dropped BLE link) doesn't
// cut it short for the rest.
//
// The scan is `--rescan yes`: nmcli requests a scan and waits (at most 15 s)
// for one that ends after the call started. Don't go back to `device wifi
// rescan` plus a plain `device wifi list`: rescan returns as soon as
// NetworkManager accepts the request, seconds before the scan ends, and a
// plain list (`--rescan auto`) prints NetworkManager's list whenever any scan
// ended in the last 30 s. NetworkManager counts a scan the driver refused as
// ended, so that list can be minutes old.
//
// Don't ask again when a scan "ends" too fast to be real (under about 1.5 s).
// The driver refused it because the link was busy, and the Realtek driver
// honors a request that comes soon after, so asking again would get past
// the check that protects a busy link. The next scan waits for the next
// interval instead.
//
// No scan is asked for while a WiFi connection is being activated. That
// counts as a scan for the interval: NetworkManager would hold the request
// and run it as soon as the connection is up, and the agent shouldn't scan
// a link that has just come up either.
func (n *NMCLINetworkManager) scanWiFi(ctx context.Context) ([]byte, error) {
	if scanning := n.scanIfDue(ctx); scanning != nil {
		select {
		case <-scanning:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return n.listWiFi(ctx, "no")
}

// scanIfDue starts a scan if none was asked for in the last scanInterval. It
// returns a channel that is closed when the scan in progress returns, or nil
// if no scan is in progress.
func (n *NMCLINetworkManager) scanIfDue(ctx context.Context) <-chan struct{} {
	n.scanMu.Lock()
	defer n.scanMu.Unlock()

	if n.scanning != nil {
		return n.scanning
	}
	if time.Since(n.lastScan) < scanInterval {
		return nil
	}
	scanning := make(chan struct{})
	n.scanning = scanning
	go func() {
		n.scan(context.WithoutCancel(ctx))
		n.scanMu.Lock()
		n.lastScan = time.Now()
		n.scanning = nil
		n.scanMu.Unlock()
		close(scanning)
	}()
	return scanning
}

// scan asks for a scan and waits for it, unless a WiFi connection is being
// activated. It drops nmcli's output: callers read NetworkManager's list
// once the scan returns.
func (n *NMCLINetworkManager) scan(ctx context.Context) {
	if n.wifiActivating(ctx) {
		n.logger.Info("Skipped WiFi scan: a WiFi connection is being activated")
		return
	}
	start := time.Now()
	if _, err := n.listWiFi(ctx, "yes"); err != nil {
		n.logger.Warn("WiFi scan failed; listing the networks NetworkManager already has", zap.Error(err))
		return
	}
	// A refused scan "ends" in about 0.5 s; real ones take seconds.
	n.logger.Info("WiFi scan ended", zap.Duration(logfields.Duration, time.Since(start)))
}

// listWiFi runs `nmcli device wifi list` with the given --rescan mode. It
// gives up after scanTimeout for a scan and nmcliTimeout otherwise.
func (n *NMCLINetworkManager) listWiFi(ctx context.Context, rescan string) ([]byte, error) {
	timeout := nmcliTimeout
	if rescan == "yes" {
		timeout = scanTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t",
		"-f", "IN-USE,SSID,SIGNAL,SECURITY",
		"device", "wifi", "list", "--rescan", rescan)
	return cmd.Output()
}

// wifiActivating reports whether NetworkManager is connecting or
// disconnecting a WiFi device. It doesn't scan in those states: it holds an
// explicit scan request until the state ends, while `--rescan yes` waits up
// to 15 s.
func (n *NMCLINetworkManager) wifiActivating(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, nmcliTimeout)
	defer cancel()
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t", "-f", "TYPE,STATE", "device", "status")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 2)
		if len(fields) == 2 && fields[0] == "wifi" &&
			(strings.HasPrefix(fields[1], "connecting") || fields[1] == "deactivating") {
			return true
		}
	}
	return false
}

// parseWiFiList merges the output of `nmcli -t -f IN-USE,SSID,SIGNAL,SECURITY
// device wifi list` with the saved profiles, producing one entry per SSID.
//
// A single SSID often surfaces on several BSS rows (multi-band APs, mesh nodes,
// repeaters). nmcli marks IN-USE (`*`) only on the one BSS the device is
// actually associated with, and sorts the list by signal — so the marked BSS is
// not necessarily the first row for its SSID. We keep the first (strongest) row
// for the displayed signal/security but OR the IN-USE marker across every BSS of
// the SSID. Without this, the "Connected" status flaps off whenever the
// associated BSS's signal dips below a sibling BSS between polls.
func parseWiFiList(scanOutput string, known []knownProfile) []*agentpb.ListWiFiNetworksResponse_WiFiNetwork {
	knownBySSID := make(map[string]knownProfile, len(known))
	for _, k := range known {
		knownBySSID[k.SSID] = k
	}

	var networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	bySSID := make(map[string]*agentpb.ListWiFiNetworksResponse_WiFiNetwork)

	scanner := bufio.NewScanner(strings.NewReader(scanOutput))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 4)
		if len(fields) < 4 {
			continue
		}
		inUse := strings.TrimSpace(fields[0]) == "*"
		ssid := fields[1]
		if ssid == "" {
			continue
		}
		if existing, ok := bySSID[ssid]; ok {
			// Another BSS for an SSID we've already recorded. Keep the
			// stronger first row's display fields, but a connected marker on
			// any BSS means the SSID is connected.
			if inUse {
				existing.IsConnected = true
			}
			continue
		}

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
		bySSID[ssid] = net
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

	return networks
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
	existing, err := existingProfileUUID(ctx, nmcliPath, c.SSID)
	if err != nil {
		return err
	}
	_, err = saveProfile(ctx, nmcliPath, existing, c)
	return err
}

// saveProfile updates the profile existing, or adds one when existing is empty,
// and returns the UUID of the saved profile.
func saveProfile(ctx context.Context, nmcliPath, existing string, c SavedCredential) (string, error) {
	if existing != "" {
		return existing, modifyProfile(ctx, nmcliPath, existing, c)
	}
	return addProfile(ctx, nmcliPath, c)
}

// savedProfile is a saved WiFi profile and its UUID.
type savedProfile struct {
	UUID string
	wifiProfile
}

// ssidProfiles returns the saved profiles for ssid in nmcli's order. A read
// failure is an error, so a caller never takes a saved profile as absent.
func ssidProfiles(ctx context.Context, nmcliPath, ssid string) ([]savedProfile, error) {
	uuids, err := wifiProfileUUIDs(ctx, nmcliPath)
	if err != nil {
		return nil, err
	}
	profiles, err := readWiFiProfiles(ctx, nmcliPath, uuids)
	if err != nil {
		return nil, err
	}
	var matches []savedProfile
	for _, uuid := range uuids {
		if p, ok := profiles[uuid]; ok && p.SSID == ssid {
			matches = append(matches, savedProfile{UUID: uuid, wifiProfile: p})
		}
	}
	return matches, nil
}

// existingProfileUUID returns the UUID of the first saved profile for ssid, or "".
func existingProfileUUID(ctx context.Context, nmcliPath, ssid string) (string, error) {
	matches, err := ssidProfiles(ctx, nmcliPath, ssid)
	if err != nil || len(matches) == 0 {
		return "", err
	}
	return matches[0].UUID, nil
}

// addedUUIDRe matches the UUID in nmcli's "Connection 'NAME' (UUID) successfully
// added." line; the last match wins, since NAME may contain anything.
var addedUUIDRe = regexp.MustCompile(`\(([0-9A-Fa-f-]{36})\) successfully added`)

// addProfile creates a profile and returns its UUID, so the caller can act on
// it without looking it up again.
func addProfile(ctx context.Context, nmcliPath string, c SavedCredential) (string, error) {
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
	// The reply is parsed, so keep it untranslated.
	cmd.Env = append(cmd.Env, "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nmcli connection add %s: %s: %w", c.SSID, strings.TrimSpace(string(out)), err)
	}
	if m := addedUUIDRe.FindAllStringSubmatch(string(out), -1); len(m) > 0 {
		return m[len(m)-1][1], nil
	}
	// Only an SSID's sole profile can be told apart as the one just added.
	matches, err := ssidProfiles(ctx, nmcliPath, c.SSID)
	if err == nil && len(matches) != 1 {
		err = fmt.Errorf("%d saved profiles for SSID %q", len(matches), c.SSID)
	}
	if err != nil {
		return "", fmt.Errorf("finding the added profile: %w", err)
	}
	return matches[0].UUID, nil
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
		if km == "wpa-psk" || km == "sae" {
			// WPA leaves the authentication algorithm to the supplicant.
			args = append(args, authAlgField, "")
		}
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
	return upProfile(ctx, nmcliPath, ssid, uuid, "")
}

// upProfile activates a profile, on device when one is given.
func upProfile(ctx context.Context, nmcliPath, ssid, uuid, device string) error {
	args := []string{"connection", "up", uuid}
	if device != "" {
		args = append(args, "ifname", device)
	}
	cmd := nmcli.Command(ctx, nmcliPath, args...)
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

// ConnectToWiFi connects through a profile it writes for hidden networks and
// known key management, and through `nmcli device wifi connect` otherwise.
func (n *NMCLINetworkManager) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error {
	ssid := req.GetSsid()
	hidden := req.GetHidden()
	password := req.GetPassword()
	security := securityHintFromProto(req.GetSecurity())
	var network scannedNetwork
	scanned := false
	if security == "" && !hidden && password != "" {
		network, scanned = scanNetwork(ctx, n.nmcliPath, ssid)
	}

	// Snapshot whether a saved profile exists *before* we touch nmcli so we
	// can roll back any profile we (or `nmcli device wifi connect`) created
	// when activation later fails. Without this, a failed authentication
	// leaves the SSID with a saved profile, and ListWiFiNetworks then reports
	// the network as IsKnown — making the UI show ★ for a connection that
	// never actually succeeded. We deliberately do NOT touch a pre-existing
	// profile, since that one holds credentials from a previously-working
	// connection that a single mistyped retry shouldn't destroy.
	saved, err := ssidProfiles(ctx, n.nmcliPath, ssid)
	if err != nil {
		return fmt.Errorf("checking for existing WiFi profile: %w", err)
	}
	if scanned {
		return n.connectScanned(ctx, ssid, password, network, saved)
	}
	preExistingUUID := ""
	if len(saved) > 0 {
		preExistingUUID = saved[0].UUID
	}

	if hidden || security != "" {
		cred := SavedCredential{
			SSID:     ssid,
			Password: password,
			Hidden:   hidden,
			Security: security,
		}
		uuid, err := saveProfile(ctx, n.nmcliPath, preExistingUUID, cred)
		if err != nil {
			return fmt.Errorf("preparing WiFi profile: %w", err)
		}
		if err := upProfile(ctx, n.nmcliPath, ssid, uuid, ""); err != nil {
			n.cleanupTransientProfile(ctx, ssid, preExistingUUID, uuid)
			return err
		}
		n.logger.Info("Connected to WiFi via profile",
			zap.String("ssid", ssid),
			zap.Bool("hidden", hidden),
			zap.String("security", security))
		return nil
	}

	if err := runNMCLIConnect(ctx, n.nmcliPath, ssid, password, hidden); err != nil {
		n.cleanupTransientProfile(ctx, ssid, preExistingUUID, "")
		return err
	}
	n.logger.Info("Connected to WiFi", zap.String("ssid", ssid))
	return nil
}

// connectScanned joins a passphrase network the scan shows. Like nmcli, it reuses
// a saved profile the network accepts, or adds one, on a device that sees it.
func (n *NMCLINetworkManager) connectScanned(ctx context.Context, ssid, password string, network scannedNetwork, saved []savedProfile) error {
	device := network.devices[0]
	reuse, keyMgmt := "", network.keyMgmt(device)
	if p, dev, ok := reusableProfile(saved, network, activeDevices(ctx, n.nmcliPath)); ok {
		reuse, keyMgmt, device = p.UUID, p.KeyMgmt, dev
	}
	cred := SavedCredential{SSID: ssid, Password: password, Security: keyMgmt}
	uuid, err := saveProfile(ctx, n.nmcliPath, reuse, cred)
	if err != nil {
		return fmt.Errorf("preparing WiFi profile: %w", err)
	}
	if err := upProfile(ctx, n.nmcliPath, ssid, uuid, device); err != nil {
		n.cleanupTransientProfile(ctx, ssid, reuse, uuid)
		return err
	}
	n.logger.Info("Connected to WiFi via profile",
		zap.String("ssid", ssid),
		zap.String("security", keyMgmt),
		zap.String("device", device))
	return nil
}

// reusableProfile picks a saved profile and its device. An active WPA profile
// comes first, since the cache can miss the BSSes it suits.
func reusableProfile(saved []savedProfile, network scannedNetwork, active map[string]string) (savedProfile, string, bool) {
	for _, p := range saved {
		if dev := active[p.UUID]; dev != "" && p.joinsWithPassphrase() {
			return p, dev, true
		}
	}
	for _, p := range saved {
		dev := network.devices[0]
		if p.Iface != "" {
			dev = p.Iface
		}
		if network.accepts(p.wifiProfile, dev) {
			return p, dev, true
		}
	}
	return savedProfile{}, "", false
}

// activeDevices maps each active connection's UUID to its device; nil when
// nmcli can't be read.
func activeDevices(ctx context.Context, nmcliPath string) map[string]string {
	out, err := nmcli.Command(ctx, nmcliPath, "-t", "-f", "UUID,DEVICE", "connection", "show", "--active").Output()
	if err != nil {
		return nil
	}
	active := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := splitNMCLI(line, 2); len(fields) == 2 {
			active[fields[0]] = fields[1]
		}
	}
	return active
}

// keyMgmts records which passphrase key managements a set of BSSes takes.
type keyMgmts struct{ psk, sae bool }

// scannedNetwork is a passphrase network as the cached scan shows it.
type scannedNetwork struct {
	devices []string            // the devices that see it, in nmcli's device order
	takes   map[string]keyMgmts // what the BSSes each device sees take
}

// keyMgmt is the key-mgmt for a new profile on device, preferring wpa-psk as
// NetworkManager does.
func (s scannedNetwork) keyMgmt(device string) string {
	if s.takes[device].psk {
		return "wpa-psk"
	}
	return "sae"
}

// accepts reports whether p can join the network on device, as nmcli checks
// before reusing a profile.
func (s scannedNetwork) accepts(p wifiProfile, device string) bool {
	t := s.takes[device]
	return p.joinsWithPassphrase() && (p.KeyMgmt == "wpa-psk" && t.psk || p.KeyMgmt == "sae" && t.sae)
}

// scanNetwork reads ssid from the cached scan; ok is false when the network is
// absent or uses anything but a passphrase, leaving nmcli to decide.
func scanNetwork(ctx context.Context, nmcliPath, ssid string) (network scannedNetwork, ok bool) {
	out, err := nmcli.Command(ctx, nmcliPath, "-t", "-f", "SSID,SSID-HEX,SECURITY,DEVICE",
		"device", "wifi", "list", "--rescan", "no").Output()
	if err != nil {
		return scannedNetwork{}, false
	}
	rows := map[string][]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 4)
		if len(fields) != 4 || fields[0] != ssid {
			continue
		}
		// A profile stores the SSID as text, so it only matches a network whose
		// raw SSID bytes are that text.
		if raw, err := hex.DecodeString(fields[1]); err != nil || string(raw) != ssid {
			return scannedNetwork{}, false
		}
		device := fields[3]
		if _, seen := rows[device]; !seen {
			network.devices = append(network.devices, device)
		}
		rows[device] = append(rows[device], fields[2])
	}
	network.takes = make(map[string]keyMgmts, len(rows))
	for device, security := range rows {
		psk, sae, passphrase := passphraseSecurity(security)
		if !passphrase {
			return scannedNetwork{}, false
		}
		network.takes[device] = keyMgmts{psk: psk, sae: sae}
	}
	return network, len(network.devices) > 0
}

// passphraseSecurity reports which passphrase key managements the scanned BSSes
// of one network take; ok is false when none does or one needs anything else.
func passphraseSecurity(rows []string) (psk, sae, ok bool) {
	for _, row := range rows {
		for _, tok := range strings.Fields(strings.ToUpper(row)) {
			switch {
			case tok == "WPA1" || tok == "WPA2":
				psk = true
			case tok == "WPA3":
				sae = true
			case tok == "802.1X" || tok == "WEP" || strings.HasPrefix(tok, "OWE"):
				return false, false, false
			}
		}
	}
	return psk, sae, psk || sae
}

// cleanupTransientProfile removes a saved nmcli profile for ssid if and only
// if it didn't already exist before the connect attempt. preExistingUUID is
// the saved profile the attempt used, if any: an empty value means
// the profile we now see was created during the failed attempt itself, so
// it's safe to delete. Cleanup is best-effort — if it fails we just log,
// since the original connect error is what the caller actually needs.
// createdUUID names that profile when the caller knows it; "" looks it up.
func (n *NMCLINetworkManager) cleanupTransientProfile(ctx context.Context, ssid, preExistingUUID, createdUUID string) {
	if preExistingUUID != "" {
		return
	}
	uuid := createdUUID
	if uuid == "" {
		var err error
		if uuid, err = existingProfileUUID(ctx, n.nmcliPath, ssid); err != nil || uuid == "" {
			return
		}
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

func (n *NMCLINetworkManager) GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error) {
	cmd := nmcli.Command(ctx, n.nmcliPath, "-t", "-f", "TYPE,STATE,CONNECTION", "device", "status")
	output, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("nmcli device status: %w", err)
	}

	foundWiFi := false
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := splitNMCLI(scanner.Text(), 3)
		if len(fields) < 3 {
			continue
		}
		if fields[0] == "wifi" {
			foundWiFi = true
		}
		if fields[0] == "wifi" && fields[1] == "connected" {
			return true, fields[2], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, "", err
	}
	if !foundWiFi {
		return false, "", fmt.Errorf("no WiFi device found")
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
