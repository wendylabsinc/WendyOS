package configpartition

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sysextfs"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/network"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// elfMachineByArch maps GOARCH values to ELF e_machine field values (little-endian uint16).
var elfMachineByArch = map[string]uint16{
	"arm64": 0x00B7, // EM_AARCH64
	"amd64": 0x003E, // EM_X86_64
}

const (
	configDir          = "/config"
	agentBinaryName    = "wendy-agent"
	defaultInstallPath = "/opt/wendyos/bin/wendy-agent"
)

// validateELF checks that the file at path is a 64-bit ELF binary compiled for
// the same architecture as the running process.
func validateELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read the first 20 bytes: ELF ident (16 bytes) + e_type (2) + e_machine (2).
	buf := make([]byte, 20)
	if _, err := io.ReadFull(f, buf); err != nil {
		return fmt.Errorf("reading ELF header: %w", err)
	}

	// Magic: \x7fELF
	if buf[0] != 0x7f || buf[1] != 'E' || buf[2] != 'L' || buf[3] != 'F' {
		return fmt.Errorf("not an ELF binary (bad magic)")
	}

	// EI_CLASS == 2 → 64-bit
	if buf[4] != 2 {
		return fmt.Errorf("not a 64-bit ELF (EI_CLASS=%d)", buf[4])
	}

	// EI_DATA == 1 → little-endian (required — e_machine is decoded as little-endian below)
	if buf[5] != 1 {
		return fmt.Errorf("not a little-endian ELF (EI_DATA=%d)", buf[5])
	}

	// e_machine at bytes 18–19, little-endian
	machine := uint16(buf[18]) | uint16(buf[19])<<8
	expected, ok := elfMachineByArch[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("unsupported host architecture: %s", runtime.GOARCH)
	}
	if machine != expected {
		return fmt.Errorf("ELF architecture mismatch: file=0x%04X want=0x%04X (%s)", machine, expected, runtime.GOARCH)
	}

	return nil
}

// parseINI parses a minimal INI file into map[section]map[key]value.
// Lines starting with '#' or ';' are comments. Keys and values are trimmed.
// Duplicate keys in the same section take the last value.
func parseINI(data []byte) map[string]map[string]string {
	result := make(map[string]map[string]string)
	var section string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if result[section] == nil {
				result[section] = make(map[string]string)
			}
			continue
		}
		if section == "" {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			result[section][key] = val
		}
	}
	return result
}

// copyFile copies src to dst with the given permissions, flushing before returning.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst) //nolint:errcheck
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst) //nolint:errcheck
		return err
	}
	return out.Close()
}

func applyBinaryUpdate(logger *zap.Logger, cfgDir, installPath string) bool {
	src := cfgDir + "/" + agentBinaryName
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return false
	}

	if err := validateELF(src); err != nil {
		logger.Error("Config partition binary failed ELF validation, removing",
			zap.String("path", src), zap.Error(err))
		os.Remove(src) //nolint:errcheck
		return false
	}

	// A seeded binary written under a merged sysext hierarchy is discarded on the
	// next boot, so the seed would silently do nothing and retry forever.
	if err := sysextfs.CheckDurable(filepath.Dir(installPath)); err != nil {
		logger.Error("Refusing to install agent binary from config partition",
			zap.String("dst", installPath), zap.Error(err))
		return false
	}

	tmp := installPath + ".new"
	if err := copyFile(src, tmp, 0o755); err != nil {
		logger.Error("Failed to copy agent binary from config partition",
			zap.String("src", src), zap.String("dst", tmp), zap.Error(err))
		return false
	}

	if err := os.Rename(tmp, installPath); err != nil {
		logger.Error("Failed to install agent binary (rename)",
			zap.String("tmp", tmp), zap.String("dst", installPath), zap.Error(err))
		os.Remove(tmp) //nolint:errcheck
		return false
	}

	if err := os.Remove(src); err != nil {
		logger.Warn("Failed to remove config partition binary after install",
			zap.String("path", src), zap.Error(err))
	}

	logger.Info("Installed agent binary from config partition, signalling restart",
		zap.String("installPath", installPath))
	return true
}

// applyWendyConf reads /config/wendy.conf, registers every `[wifi]` /
// `[wifi.N]` profile via NetworkManager, activates the highest-priority one
// that is in range, applies the device name from the `[device]` section if
// present, then deletes the file regardless of outcome (bad values should
// not be retried on every boot).
func applyWendyConf(logger *zap.Logger, cfgDir string) {
	confPath := cfgDir + "/wendy.conf"
	data, err := os.ReadFile(confPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		logger.Error("Failed to read wendy.conf from config partition",
			zap.String("path", confPath), zap.Error(err))
		if err := os.Remove(confPath); err != nil {
			logger.Warn("Failed to remove wendy.conf after read error",
				zap.String("path", confPath), zap.Error(err))
		}
		return
	}

	sections := parseINI(data)
	creds := wendyconf.UnmarshalWiFi(sections)
	if len(creds) == 0 {
		logger.Warn("wendy.conf has no usable WiFi credentials, skipping WiFi provisioning")
	} else {
		ctx := context.Background()
		for _, c := range creds {
			err := network.AddOrUpdateProfile(ctx, network.SavedCredential{
				SSID:     c.SSID,
				Password: c.Password,
				Priority: c.Priority,
				Hidden:   c.Hidden,
				Security: c.Security,
			})
			if err != nil {
				logger.Error("Failed to register WiFi profile from config partition",
					zap.String("ssid", c.SSID), zap.Error(err))
				continue
			}
			logger.Info("Registered WiFi profile from config partition",
				zap.String("ssid", c.SSID),
				zap.Int32("priority", c.Priority))
		}
		// Try to bring up the first (highest-priority) credential so the
		// device is online immediately when it's in range. Lower-priority
		// profiles stay saved for future locations.
		for _, c := range creds {
			if err := network.ActivateProfile(ctx, c.SSID); err != nil {
				logger.Warn("Could not activate WiFi profile",
					zap.String("ssid", c.SSID), zap.Error(err))
				continue
			}
			logger.Info("Activated WiFi profile from config partition", zap.String("ssid", c.SSID))
			break
		}
	}

	if name := sections["device"]["name"]; name != "" {
		if err := applyDeviceName(logger, name); err != nil {
			logger.Error("Failed to apply device name from config partition",
				zap.String("name", name), zap.Error(err))
		}
	}

	if err := os.Remove(confPath); err != nil {
		logger.Warn("Failed to remove wendy.conf after applying",
			zap.String("path", confPath), zap.Error(err))
	}
}

// maxDeviceNameLen caps the device name so the hostname this package derives as
// "wendyos-<name>" (via generate-hostname.sh in applyDeviceName) stays within the
// 63-octet RFC 1035 label limit. The 8-character "wendyos-" prefix leaves 55
// characters for the name; longer names produce an invalid hostname label.
const maxDeviceNameLen = 55

// validDeviceName: letter-led, 3-55 chars of [a-z0-9-], no trailing hyphen so the
// derived "wendyos-<name>" stays a valid DNS label. Mirrors is_valid_device_name.
func validDeviceName(name string) bool {
	if len(name) < 3 || len(name) > maxDeviceNameLen {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
			// always ok
		case (c >= '0' && c <= '9') || c == '-':
			if i == 0 {
				return false // must start with a letter
			}
		default:
			return false
		}
	}
	return name[len(name)-1] != '-'
}

func applyDeviceName(logger *zap.Logger, name string) error {
	if !validDeviceName(name) {
		return fmt.Errorf("invalid device name %q: must match ^[a-z][a-z0-9-]{1,53}[a-z0-9]$", name)
	}

	const deviceNamePath = "/etc/wendyos/device-name"
	if err := os.MkdirAll("/etc/wendyos", 0o755); err != nil {
		return fmt.Errorf("creating /etc/wendyos: %w", err)
	}
	if err := os.WriteFile(deviceNamePath, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing device name: %w", err)
	}
	logger.Info("Wrote device name", zap.String("name", name), zap.String("path", deviceNamePath))

	// A device-name apply is a deliberate re-identification, so clear any explicit
	// rename — it outranks device-name (hostname.go, update-mdns-uuid.sh) and would
	// otherwise override this name on the next boot.
	const explicitHostnamePath = "/etc/wendy-agent/hostname"
	if err := os.Remove(explicitHostnamePath); err == nil {
		logger.Info("Cleared explicit hostname so the device name takes effect",
			zap.String("path", explicitHostnamePath))
	} else if !os.IsNotExist(err) {
		logger.Warn("Could not clear explicit hostname; it will keep overriding the device name",
			zap.String("path", explicitHostnamePath), zap.Error(err))
	}

	// Build an env with a full system PATH so scripts can find standard
	// utilities (mkdir, logger, etc.) even when the agent runs under systemd
	// with a restricted PATH.
	env := os.Environ()
	const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	replaced := false
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = "PATH=" + systemPath
			replaced = true
			break
		}
	}
	if !replaced {
		env = append(env, "PATH="+systemPath)
	}

	// generate-hostname.sh reads /etc/wendyos/device-name (which we just wrote)
	// and derives wendyos-<name>, updating /etc/hostname and the running hostname.
	hostnameScript := exec.Command("/usr/sbin/generate-hostname.sh")
	hostnameScript.Env = env
	if out, err := hostnameScript.CombinedOutput(); err != nil {
		logger.Warn("generate-hostname.sh failed", zap.Error(err), zap.String("output", string(out)))
	} else {
		logger.Info("Hostname updated", zap.String("hostname", "wendyos-"+name))
	}

	// update-mdns-uuid.sh runs at boot, before the agent, so it cannot publish a
	// name applied here at runtime. Update the avahi service file directly
	// instead. (It is no longer the one-shot placeholder replacer it once was —
	// it now re-derives name/displayname from device-name on every boot, which is
	// why services.ReassertHostnameAdvertisement re-applies an explicit rename
	// over the top of it at startup.)
	updateAvahiDeviceName(logger, name, env)

	return nil
}

// updateAvahiDeviceName rewrites the name/displayname/fqdn TXT records in the
// avahi service file and reloads avahi-daemon so mDNS picks up the new name.
func updateAvahiDeviceName(logger *zap.Logger, name string, env []string) {
	const serviceFile = "/etc/avahi/services/wendyos-mdns.service"

	data, err := os.ReadFile(serviceFile)
	if err != nil {
		logger.Warn("Could not read avahi service file", zap.String("path", serviceFile), zap.Error(err))
		return
	}

	displayName := avahiDisplayName(name)
	fqdn := "sh.wendy." + name

	content := replaceTXTRecord(string(data), "name", name)
	content = replaceTXTRecord(content, "displayname", displayName)
	content = replaceTXTRecord(content, "fqdn", fqdn)

	if err := os.WriteFile(serviceFile, []byte(content), 0o644); err != nil {
		logger.Warn("Could not write avahi service file", zap.String("path", serviceFile), zap.Error(err))
		return
	}

	// --reload (SIGHUP) only refreshes service files; it does not re-read the
	// hostname from gethostname(), so %h would stay stale. A full restart picks
	// up both the new service file and the updated hostname.
	// Use the absolute path: exec.Command resolves binaries from the calling
	// process's PATH, not from cmd.Env, so bare "systemctl" would not be found.
	restart := exec.Command("/usr/bin/systemctl", "restart", "avahi-daemon")
	restart.Env = env
	if out, err := restart.CombinedOutput(); err != nil {
		logger.Warn("systemctl restart avahi-daemon failed", zap.Error(err), zap.String("output", string(out)))
	} else {
		logger.Info("Restarted avahi-daemon with new device name", zap.String("name", name))
	}
}

// UpdateAvahiForProvisioning rewrites the _wendyos._udp service block in the
// avahi service file to advertise the mTLS port, a tls=true TXT record, and
// (when assetID > 0 / orgID > 0) assetid=<N> and orgid=<N> TXT records, then
// restarts avahi-daemon so the mDNS advertisement reflects that the device is
// now provisioned.
func UpdateAvahiForProvisioning(logger *zap.Logger, mtlsPort int, assetID, orgID int32) {
	updateAvahiService(logger, defaultAvahiServiceDir, mtlsPort, true, assetID, orgID)
}

// UpdateAvahiForUnprovisioning reverts the _wendyos._udp service block to
// advertise the plaintext agent port, a tls=false TXT record, and removes the
// assetid and orgid TXT records, then restarts avahi-daemon. It is the
// inverse of UpdateAvahiForProvisioning, used when a device is unprovisioned
// so it is rediscoverable for re-enrollment.
func UpdateAvahiForUnprovisioning(logger *zap.Logger, plaintextPort int) {
	updateAvahiService(logger, defaultAvahiServiceDir, plaintextPort, false, 0, 0)
}

// defaultAvahiServiceDir is the directory scanned for avahi service files on
// a real device. Tests pass an alternate directory to updateAvahiService.
const defaultAvahiServiceDir = "/etc/avahi/services"

// updateAvahiService points the _wendyos._udp advertisement at the given port
// and sets its tls and assetid TXT records. The service file name varies by
// image (e.g. wendyos-mdns.service or wendy-agent.service), so we scan all
// files in serviceDir and update the first one that contains a _wendyos._udp
// block.
func updateAvahiService(logger *zap.Logger, serviceDir string, port int, tls bool, assetID, orgID int32) {
	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		logger.Warn("Could not read avahi services dir", zap.String("path", serviceDir), zap.Error(err))
		return
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		serviceFile := filepath.Join(serviceDir, e.Name())
		data, err := os.ReadFile(serviceFile)
		if err != nil {
			continue
		}
		if !strings.Contains(string(data), "_wendyos._udp") {
			continue
		}

		content := updateWendyOSServicePort(string(data), port, tls, assetID, orgID)
		if err := os.WriteFile(serviceFile, []byte(content), 0o644); err != nil {
			logger.Warn("Could not write avahi service file",
				zap.String("path", serviceFile), zap.Error(err))
			return
		}

		restart := exec.Command("/usr/bin/systemctl", "restart", "avahi-daemon")
		if out, err := restart.CombinedOutput(); err != nil {
			logger.Warn("systemctl restart avahi-daemon failed",
				zap.Error(err), zap.String("output", string(out)))
		} else {
			logger.Info("Updated avahi advertisement",
				zap.String("file", e.Name()), zap.Int("port", port), zap.Bool("tls", tls), zap.Int32("assetId", assetID), zap.Int32("orgId", orgID))
		}
		return
	}

	logger.Warn("No avahi service file with _wendyos._udp found; mDNS not updated")
}

// updateWendyOSServicePort finds the _wendyos._udp service block and updates
// its port, tls TXT record, assetid TXT record, and orgid TXT record. Other
// service blocks (SSH, HTTP, etc.) are left untouched. When assetID <= 0 or
// orgID <= 0, the corresponding TXT record is removed (used on
// unprovisioning).
func updateWendyOSServicePort(content string, port int, tls bool, assetID, orgID int32) string {
	const typeTag = "<type>_wendyos._udp</type>"
	portRe := regexp.MustCompile(`<port>\d+</port>`)

	typeIdx := strings.Index(content, typeTag)
	if typeIdx < 0 {
		return content
	}

	// Walk back to find the opening <service tag for this block.
	serviceStart := strings.LastIndex(content[:typeIdx], "<service")
	if serviceStart < 0 {
		serviceStart = typeIdx
	}

	// Walk forward to find the closing </service> tag for this block.
	closeOffset := strings.Index(content[typeIdx:], "</service>")
	if closeOffset < 0 {
		return content
	}
	serviceEnd := typeIdx + closeOffset + len("</service>")

	block := content[serviceStart:serviceEnd]

	// Update port only within this block.
	block = portRe.ReplaceAllString(block, fmt.Sprintf("<port>%d</port>", port))

	// Add or update the tls TXT record.
	tlsValue := "false"
	if tls {
		tlsValue = "true"
	}
	if strings.Contains(block, "<txt-record>tls=") {
		block = replaceTXTRecord(block, "tls", tlsValue)
	} else {
		block = strings.Replace(block, "</service>",
			"    <txt-record>tls="+tlsValue+"</txt-record>\n  </service>", 1)
	}

	block = updateAssetIDTXTRecord(block, assetID)
	block = updateOrgIDTXTRecord(block, orgID)

	return content[:serviceStart] + block + content[serviceEnd:]
}

// updateAssetIDTXTRecord adds, updates, or removes the assetid TXT record
// within a single _wendyos._udp service block. assetID <= 0 removes the
// record (used on unprovisioning, where the asset ID is no longer known).
func updateAssetIDTXTRecord(block string, assetID int32) string {
	assetIDRe := regexp.MustCompile(`\s*<txt-record>assetid=[^<]*</txt-record>`)
	hasRecord := strings.Contains(block, "<txt-record>assetid=")

	if assetID <= 0 {
		if hasRecord {
			block = assetIDRe.ReplaceAllString(block, "")
		}
		return block
	}

	value := fmt.Sprintf("%d", assetID)
	if hasRecord {
		return replaceTXTRecord(block, "assetid", value)
	}
	return strings.Replace(block, "</service>",
		"    <txt-record>assetid="+value+"</txt-record>\n  </service>", 1)
}

// updateOrgIDTXTRecord adds, updates, or removes the orgid TXT record within
// a single _wendyos._udp service block. The mesh friendly-name resolver reads
// this to filter LAN peers to the dialer's own org. orgID <= 0 removes the
// record (used on unprovisioning, where the org is no longer known).
func updateOrgIDTXTRecord(block string, orgID int32) string {
	orgIDRe := regexp.MustCompile(`\s*<txt-record>orgid=[^<]*</txt-record>`)
	hasRecord := strings.Contains(block, "<txt-record>orgid=")

	if orgID <= 0 {
		if hasRecord {
			block = orgIDRe.ReplaceAllString(block, "")
		}
		return block
	}

	value := fmt.Sprintf("%d", orgID)
	if hasRecord {
		return replaceTXTRecord(block, "orgid", value)
	}
	return strings.Replace(block, "</service>",
		"    <txt-record>orgid="+value+"</txt-record>\n  </service>", 1)
}

// replaceTXTRecord replaces the value in a <txt-record>key=...</txt-record> line.
func replaceTXTRecord(content, key, value string) string {
	re := regexp.MustCompile(`(<txt-record>` + regexp.QuoteMeta(key) + `=)[^<]*(</txt-record>)`)
	return re.ReplaceAllString(content, `${1}`+value+`${2}`)
}

// avahiDisplayName converts "brave-dolphin" → "Brave Dolphin".
func avahiDisplayName(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// preProvisionedState is the provisioning state written by the CLI during imaging.
// JSON tags must match provisioningState in internal/agent/services.
type preProvisionedState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

// applyPreProvisioning checks cfgDir for a provisioning.json written by the CLI
// at imaging time. If present and valid, it copies the state to configPath so
// ProvisioningService.loadState() picks it up on first boot, then deletes the source.
func applyPreProvisioning(logger *zap.Logger, cfgDir, configPath string) {
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	data, err := os.ReadFile(srcPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		logger.Error("Failed to read pre-provisioning state from config partition",
			zap.String("path", srcPath), zap.Error(err))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	var state preProvisionedState
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Error("Failed to parse pre-provisioning state, removing",
			zap.String("path", srcPath), zap.Error(err))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	if !state.Enrolled || state.KeyPEM == "" || state.CertPEM == "" || state.CloudHost == "" {
		logger.Error("Pre-provisioning state is incomplete, removing",
			zap.String("path", srcPath))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	if err := services.WritePEMFiles(configPath, state.KeyPEM, state.CertPEM, state.ChainPEM); err != nil {
		logger.Error("Failed to write PEM files from config partition",
			zap.String("configPath", configPath), zap.Error(err))
		return
	}

	if err := os.WriteFile(filepath.Join(configPath, "provisioning.json"), data, 0o600); err != nil {
		logger.Error("Failed to write provisioning.json from config partition", zap.Error(err))
		return
	}

	if err := os.Remove(srcPath); err != nil {
		logger.Warn("Failed to remove pre-provisioning state from config partition",
			zap.String("path", srcPath), zap.Error(err))
	}

	logger.Info("Applied pre-provisioned state from config partition",
		zap.String("cloudHost", state.CloudHost),
		zap.Int32("orgId", state.OrgID),
		zap.Int32("assetId", state.AssetID),
	)
}

// Apply checks the config partition for a pending agent binary, WiFi config, and
// pre-provisioning state, applying them in order. If a binary update is installed,
// the process exits so systemd can restart it with the new binary.
// configPath is the agent's configuration directory (e.g. /etc/wendy-agent).
func Apply(logger *zap.Logger, configPath string) {
	// The running binary is the target. EvalSymlinks is best effort: it walks
	// every component and fails outright when a parent directory has been torn
	// down under the process, but the unresolved path still names the file.
	installPath := defaultInstallPath
	if exe, err := os.Executable(); err == nil {
		installPath = exe
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			installPath = real
		}
	}
	if applyBinaryUpdate(logger, configDir, installPath) {
		os.Exit(0)
	}
	applyWendyConf(logger, configDir)
	applyPreProvisioning(logger, configDir, configPath)
	applyClockFloor(logger, configDir, configPath)
	applyExtensions(logger, configDir)
}

// applyClockFloor copies clock_floor from cfgDir (the FAT32 config partition,
// written by the CLI at flash time) into configPath (the agent's private config
// directory, e.g. /etc/wendy-agent), so the agent can read it as a startup time
// floor.
func applyClockFloor(logger *zap.Logger, cfgDir, configPath string) {
	src := filepath.Join(cfgDir, "clock_floor")
	data, err := os.ReadFile(src)
	if err != nil {
		return // file absent — not an error on images that predate this feature
	}
	// The config directory may not exist yet on first boot: provisioning
	// (which creates it) can run after us or not at all on a fresh image,
	// and without the floor a no-RTC device boots months in the past.
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		if logger != nil {
			logger.Error("configpartition: failed to create config dir for clock_floor", zap.Error(err))
		}
		return
	}
	dst := filepath.Join(configPath, "clock_floor")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		if logger != nil {
			logger.Error("configpartition: failed to write clock_floor", zap.Error(err))
		}
	}
}

// seededExtension is one driver add-on declared in the config partition's
// extensions.json. It mirrors the registry's resolution shape: the agent
// fetches and verifies the .raw itself.
//
// Nothing in the tree writes this file yet, so the path below is reachable only
// by placing it on the config partition by hand. It also fails closed until a
// signing key is embedded — the seed requires a real signature.
type seededExtension struct {
	Name          string   `json:"name"`
	KernelVersion string   `json:"kernel_version,omitempty"`
	SHA256        string   `json:"sha256,omitempty"`
	Signature     string   `json:"signature,omitempty"` // base64 detached signature
	ArtifactURL   string   `json:"artifact_url"`
	ModulesLoad   []string `json:"modules_load,omitempty"`
}

// applyExtensions checks cfgDir for an extensions.json placed on the config
// partition at imaging time and installs each declared driver add-on
// (fetch -> verify sha256 + signature -> place on /data -> apply), then deletes
// the source (consume-and-erase). Absent file is the normal case and a no-op.
func applyExtensions(logger *zap.Logger, cfgDir string) {
	srcPath := filepath.Join(cfgDir, "extensions.json")
	data, err := os.ReadFile(srcPath)
	if os.IsNotExist(err) {
		return
	}
	// Consume-and-erase FIRST: whatever happens below, the seed is gone, so a bad
	// or unreachable entry can never re-wedge subsequent boots (e.g. if the device
	// is power-cycled while a fetch is in flight).
	if rmErr := os.Remove(srcPath); rmErr != nil {
		logger.Warn("Failed to remove seeded extensions from config partition",
			zap.String("path", srcPath), zap.Error(rmErr))
	}
	if err != nil {
		logger.Error("Failed to read seeded extensions from config partition",
			zap.String("path", srcPath), zap.Error(err))
		return
	}

	var exts []seededExtension
	if err := json.Unmarshal(data, &exts); err != nil {
		logger.Error("Failed to parse seeded extensions", zap.Error(err))
		return
	}
	if len(exts) == 0 {
		return
	}

	// This runs on the boot path before the network is reliably up, so bound the
	// whole batch: a slow or unreachable artifact URL must not stall startup. A
	// dropped seed is recoverable later via `wendy device drivers install`.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	svc := services.NewSeedDriverService(logger)
	linkDeadline := time.Now().Add(seedLinkWait)
	for _, e := range exts {
		sig, decErr := base64.StdEncoding.DecodeString(e.Signature)
		if decErr != nil {
			logger.Error("Skipping seeded driver with invalid signature encoding",
				zap.String("name", e.Name), zap.Error(decErr))
			continue
		}
		spec := services.DriverInstallSpec{
			Name:          e.Name,
			KernelVersion: e.KernelVersion,
			SHA256:        e.SHA256,
			Signature:     sig,
			ArtifactURL:   e.ArtifactURL,
			ModulesLoad:   e.ModulesLoad,
		}
		// This Apply() registers WiFi just above, so the link may not be up for the
		// first fetch. The seed is already erased, so retry rather than lose it;
		// briefly, because nothing is served until this returns.
		var err error
		for delay := time.Second; ; delay *= 2 {
			if err = svc.InstallFromURL(ctx, spec); err == nil || !linkNotReady(err) {
				break
			}
			if time.Now().Add(delay).After(linkDeadline) {
				break
			}
			time.Sleep(delay)
		}
		if err != nil {
			logger.Error("Failed to apply seeded driver",
				zap.String("name", e.Name), zap.Error(err))
			continue
		}
		logger.Info("Applied seeded driver add-on", zap.String("name", e.Name))
	}
}

// seedLinkWait bounds how long a seeded install waits for a still-associating
// link. Kept short deliberately: applyExtensions runs synchronously before the
// agent starts serving, so an unreachable artifact URL delays every RPC by this
// much. Long enough for DHCP after association, far short of the batch budget.
const seedLinkWait = 15 * time.Second

// linkNotReady reports whether err is the link still coming up (DNS or connect),
// as opposed to a validation, verification or apply failure.
func linkNotReady(err error) bool {
	// The SSRF guard rejects from inside the dialer, which wraps it in
	// *net.OpError too; retrying that just re-probes the blocked address.
	if strings.Contains(err.Error(), "refusing to fetch") {
		return false
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &opErr)
}
