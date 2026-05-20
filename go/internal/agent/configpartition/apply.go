package configpartition

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/network"
	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

// elfMachineByArch maps GOARCH values to ELF e_machine field values (little-endian uint16).
var elfMachineByArch = map[string]uint16{
	"arm64": 0x00B7, // EM_AARCH64
	"amd64": 0x003E, // EM_X86_64
}

const (
	configDir          = "/config"
	agentBinaryName    = "wendy-agent"
	defaultInstallPath = "/usr/local/bin/wendy-agent"
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

// applyBinaryUpdate installs a new agent binary from cfgDir if present and valid.
// Returns true when the binary was installed and the caller should exit.
// installPath is the destination (e.g. /usr/local/bin/wendy-agent).
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

// validDeviceName reports whether name satisfies the WendyOS device name rules:
// starts with a lowercase letter, followed by 2–63 lowercase letters, digits, or hyphens.
func validDeviceName(name string) bool {
	if len(name) < 3 || len(name) > 64 {
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
	return true
}

// applyDeviceName writes the device name to /etc/wendyos/device-name, then
// regenerates the hostname and reloads avahi so the mDNS advertisement reflects
// the new name immediately.
func applyDeviceName(logger *zap.Logger, name string) error {
	if !validDeviceName(name) {
		return fmt.Errorf("invalid device name %q: must match ^[a-z][a-z0-9-]{2,63}$", name)
	}

	const deviceNamePath = "/etc/wendyos/device-name"
	if err := os.MkdirAll("/etc/wendyos", 0o755); err != nil {
		return fmt.Errorf("creating /etc/wendyos: %w", err)
	}
	if err := os.WriteFile(deviceNamePath, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing device name: %w", err)
	}
	logger.Info("Wrote device name", zap.String("name", name), zap.String("path", deviceNamePath))

	// generate-hostname.sh reads /etc/wendyos/device-name (which we just wrote)
	// and derives wendyos-<name>, updating /etc/hostname and the running hostname.
	hostnameScript := exec.Command("generate-hostname.sh")
	if out, err := hostnameScript.CombinedOutput(); err != nil {
		logger.Warn("generate-hostname.sh failed", zap.Error(err), zap.String("output", string(out)))
	} else {
		logger.Info("Hostname updated", zap.String("hostname", "wendyos-"+name))
	}

	// update-mdns-uuid.sh is a first-boot placeholder replacer and is a no-op
	// on a running device. Update the avahi service file directly instead.
	updateAvahiDeviceName(logger, name)

	return nil
}

// updateAvahiDeviceName rewrites the name/displayname/fqdn TXT records in the
// avahi service file and reloads avahi-daemon so mDNS picks up the new name.
func updateAvahiDeviceName(logger *zap.Logger, name string) {
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
	restart := exec.Command("systemctl", "restart", "avahi-daemon")
	if out, err := restart.CombinedOutput(); err != nil {
		logger.Warn("systemctl restart avahi-daemon failed", zap.Error(err), zap.String("output", string(out)))
	} else {
		logger.Info("Restarted avahi-daemon with new device name", zap.String("name", name))
	}
}

// UpdateAvahiForProvisioning rewrites the _wendyos._udp service block in the
// avahi service file to advertise the mTLS port and a tls=true TXT record,
// then restarts avahi-daemon so the mDNS advertisement reflects that the
// device is now provisioned.
//
// The service file name varies by image (e.g. wendyos-mdns.service or
// wendy-agent.service), so we scan all files in /etc/avahi/services/ and
// update the first one that contains a _wendyos._udp block.
func UpdateAvahiForProvisioning(logger *zap.Logger, mtlsPort int) {
	const serviceDir = "/etc/avahi/services"

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

		content := updateWendyOSServicePort(string(data), mtlsPort)
		if err := os.WriteFile(serviceFile, []byte(content), 0o644); err != nil {
			logger.Warn("Could not write avahi service file",
				zap.String("path", serviceFile), zap.Error(err))
			return
		}

		restart := exec.Command("systemctl", "restart", "avahi-daemon")
		if out, err := restart.CombinedOutput(); err != nil {
			logger.Warn("systemctl restart avahi-daemon failed after provisioning",
				zap.Error(err), zap.String("output", string(out)))
		} else {
			logger.Info("Updated avahi advertisement for mTLS",
				zap.String("file", e.Name()), zap.Int("port", mtlsPort))
		}
		return
	}

	logger.Warn("No avahi service file with _wendyos._udp found; mDNS not updated")
}

// updateWendyOSServicePort finds the _wendyos._udp service block and updates
// its port to mtlsPort and adds/updates a tls=true TXT record. Other service
// blocks (SSH, HTTP, etc.) are left untouched.
func updateWendyOSServicePort(content string, mtlsPort int) string {
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
	block = portRe.ReplaceAllString(block, fmt.Sprintf("<port>%d</port>", mtlsPort))

	// Add or update the tls=true TXT record.
	if strings.Contains(block, "<txt-record>tls=") {
		block = replaceTXTRecord(block, "tls", "true")
	} else {
		block = strings.Replace(block, "</service>",
			"    <txt-record>tls=true</txt-record>\n  </service>", 1)
	}

	return content[:serviceStart] + block + content[serviceEnd:]
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
	installPath := defaultInstallPath
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			installPath = real
		}
	}
	if applyBinaryUpdate(logger, configDir, installPath) {
		os.Exit(0)
	}
	applyWendyConf(logger, configDir)
	applyPreProvisioning(logger, configDir, configPath)
}
