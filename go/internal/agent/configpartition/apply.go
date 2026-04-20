package configpartition

import (
	"context"
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

// applyWendyConf reads /config/wendy.conf, applies WiFi config ([wifi] section)
// and device name ([device] section), then deletes the file regardless of outcome
// so bad values are not retried on every boot.
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

	wifi := sections["wifi"]
	ssid := wifi["ssid"]
	if ssid == "" {
		logger.Warn("wendy.conf [wifi] section has no ssid, skipping WiFi provisioning")
	} else {
		if err := network.Connect(context.Background(), ssid, wifi["password"]); err != nil {
			logger.Error("Failed to connect to WiFi from config partition",
				zap.String("ssid", ssid), zap.Error(err))
		} else {
			logger.Info("Connected to WiFi from config partition", zap.String("ssid", ssid))
		}
	}

	if name := sections["device"]["name"]; name != "" {
		if err := applyDeviceName(logger, name); err != nil {
			logger.Error("Failed to apply device name from config partition",
				zap.String("name", name), zap.Error(err))
		}
	}

	// Always delete so we don't retry on the next boot.
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

	// update-mdns-uuid.sh is a first-boot placeholder replacer and is a no-op
	// on a running device. Update the avahi service file directly instead.
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

// Apply checks the config partition for a pending agent binary and WiFi config,
// applying them in order. If a binary update is installed, the process exits
// so systemd can restart it with the new binary.
func Apply(logger *zap.Logger) {
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
}
