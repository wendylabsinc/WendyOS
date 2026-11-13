package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// explicitHostnamePath holds a literal hostname set via SetHostname. It lives in
// the agent's config directory and is read verbatim by generate-hostname.sh on
// boot, so an explicit rename survives reboots without the "wendyos-" prefix the
// device-name flow would otherwise derive.
const explicitHostnamePath = "/etc/wendy-agent/hostname"

// maxHostnameLen is the RFC 1035 limit for a single DNS label.
const maxHostnameLen = 63

// avahiServiceFile is the mDNS advertisement the agent edits at runtime. Unlike
// explicitHostnamePath it is NOT persistent: it lives on the rootfs, so it is
// per-OTA-slot, and wendyos-identity.service rewrites its name/displayname TXT
// records from /etc/wendyos/device-name on every boot. See
// ReassertHostnameAdvertisement.
const avahiServiceFile = "/etc/avahi/services/wendyos-mdns.service"

// validHostname reports whether name is a valid DNS label suitable for use as a
// literal hostname: 1–63 characters, starting with a lowercase letter, followed
// by lowercase letters, digits, or hyphens, and not ending in a hyphen.
func validHostname(name string) bool {
	if len(name) == 0 || len(name) > maxHostnameLen {
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

// applyHostname sets the device's hostname to a literal value (no "wendyos-"
// prefix), applies it to the running system and mDNS immediately, and persists
// it to explicitHostnamePath so it survives reboots. It is the runtime
// counterpart to the boot-time device-name flow in
// configpartition.applyDeviceName (which derives "wendyos-<name>" instead).
func applyHostname(logger *zap.Logger, hostname string) error {
	if !validHostname(hostname) {
		return fmt.Errorf("invalid hostname %q: must match ^[a-z][a-z0-9-]{0,62}$ and not end with '-'", hostname)
	}

	// Persist the literal hostname for boot-time reuse by generate-hostname.sh.
	const configDir = "/etc/wendy-agent"
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", configDir, err)
	}
	if err := os.WriteFile(explicitHostnamePath, []byte(hostname+"\n"), 0o644); err != nil {
		return fmt.Errorf("persisting hostname to %s: %w", explicitHostnamePath, err)
	}

	// Apply to the running system. Write /etc/hostname directly (rather than
	// hostnamectl) so it works even when /etc/hostname is bind-mounted, matching
	// generate-hostname.sh.
	if err := os.WriteFile("/etc/hostname", []byte(hostname+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing /etc/hostname: %w", err)
	}
	if out, err := exec.Command("/usr/bin/hostname", hostname).CombinedOutput(); err != nil {
		return fmt.Errorf("setting running hostname: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	updateEtcHosts(logger, hostname)

	// Update the mDNS advertisement and restart avahi-daemon so the new hostname
	// is published live (a restart is required for avahi to re-read gethostname()).
	// Best-effort: the hostname is already set and persisted, and avahi will pick
	// it up on next restart/reboot regardless.
	updateAvahiHostname(logger, hostname)

	logger.Info("Applied hostname", zap.String("hostname", hostname))
	return nil
}

// updateEtcHosts keeps the 127.0.1.1 loopback alias in sync with the hostname,
// mirroring generate-hostname.sh. Best-effort: failures are logged, not fatal.
func updateEtcHosts(logger *zap.Logger, hostname string) {
	const hostsPath = "/etc/hosts"
	line := fmt.Sprintf("127.0.1.1 %s %s.local", hostname, hostname)

	data, err := os.ReadFile(hostsPath)
	if err != nil {
		if writeErr := os.WriteFile(hostsPath, []byte(line+"\n"), 0o644); writeErr != nil {
			logger.Warn("Could not write /etc/hosts", zap.Error(writeErr))
		}
		return
	}

	var kept []string
	for l := range strings.SplitSeq(string(data), "\n") {
		// Drop only lines whose address field is exactly 127.0.1.1, so we don't
		// accidentally remove unrelated entries like "127.0.1.10".
		if fields := strings.Fields(l); len(fields) > 0 && fields[0] == "127.0.1.1" {
			continue
		}
		kept = append(kept, l)
	}
	content := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n" + line + "\n"
	if err := os.WriteFile(hostsPath, []byte(content), 0o644); err != nil {
		logger.Warn("Could not update /etc/hosts", zap.Error(err))
	}
}

// updateAvahiHostname rewrites the name/displayname/fqdn TXT records in the avahi
// service file and restarts avahi-daemon so mDNS reflects the new hostname. This
// mirrors configpartition.updateAvahiDeviceName; the two are kept separate to
// avoid an import cycle (configpartition imports this package). Keep the avahi
// service-file format in sync between them.
func updateAvahiHostname(logger *zap.Logger, hostname string) {
	unlock := LockAvahiServiceFile()
	defer unlock()

	data, err := os.ReadFile(avahiServiceFile)
	if err != nil {
		logger.Warn("Could not read avahi service file", zap.String("path", avahiServiceFile), zap.Error(err))
		return
	}

	content := avahiContentForHostname(string(data), hostname)
	if err := os.WriteFile(avahiServiceFile, []byte(content), 0o644); err != nil {
		logger.Warn("Could not write avahi service file", zap.String("path", avahiServiceFile), zap.Error(err))
		return
	}

	// Unconditional restart, unlike the re-assert path: here the hostname itself
	// just changed, so avahi must re-read gethostname() even when the TXT records
	// happen to be unchanged.
	if restartAvahiDaemon(logger) {
		logger.Info("Restarted avahi-daemon with new hostname", zap.String("hostname", hostname))
	}
}

// ReassertHostnameAdvertisement re-publishes the mDNS TXT records for a hostname
// previously set via SetHostname ('wendy device rename'). Call it once at agent
// startup, after the boot-time identity units have run.
//
// A rename persists the hostname itself (explicitHostnamePath is bind-mounted to
// /data and generate-hostname.sh prefers it), but the advertisement carrying the
// name/displayname TXT records is rootfs state that gets reverted underneath us
// two ways: wendyos-identity.service re-derives those records from
// /etc/wendyos/device-name on every boot, and an A/B OTA brings back a fresh
// service file from the new slot. Either way the device goes on resolving as
// <name>.local while 'wendy device list' shows the pre-rename name again.
//
// Best-effort and quiet by design: with no rename in effect, or when the records
// already agree, it touches nothing and leaves avahi-daemon alone — this runs on
// every agent start, and a needless restart would drop the advertisement.
func ReassertHostnameAdvertisement(logger *zap.Logger) {
	reassertHostnameAdvertisement(logger, explicitHostnamePath, avahiServiceFile, func() bool {
		return restartAvahiDaemon(logger)
	})
}

// reassertHostnameAdvertisement is the testable core of
// ReassertHostnameAdvertisement, with the paths and the avahi restart injected.
func reassertHostnameAdvertisement(logger *zap.Logger, hostnamePath, serviceFile string, restartAvahi func() bool) {
	unlock := LockAvahiServiceFile()
	defer unlock()

	data, err := os.ReadFile(hostnamePath)
	if err != nil {
		// A missing file is the normal "never renamed" case, not a problem.
		if !os.IsNotExist(err) {
			logger.Warn("Could not read persisted hostname", zap.String("path", hostnamePath), zap.Error(err))
		}
		return
	}

	hostname := strings.TrimSpace(string(data))
	if !validHostname(hostname) {
		logger.Warn("Ignoring invalid persisted hostname; leaving the mDNS advertisement as-is",
			zap.String("path", hostnamePath), zap.String("hostname", hostname))
		return
	}

	service, err := os.ReadFile(serviceFile)
	if err != nil {
		// Absent on a non-WendyOS host (e.g. the Linux desktop install), where
		// there is no avahi service file to keep in sync.
		if !os.IsNotExist(err) {
			logger.Warn("Could not read avahi service file", zap.String("path", serviceFile), zap.Error(err))
		}
		return
	}

	content := avahiContentForHostname(string(service), hostname)
	if content == string(service) {
		return // already advertising the renamed device
	}

	if err := os.WriteFile(serviceFile, []byte(content), 0o644); err != nil {
		logger.Warn("Could not write avahi service file", zap.String("path", serviceFile), zap.Error(err))
		return
	}
	if restartAvahi() {
		logger.Info("Re-applied renamed hostname to the mDNS advertisement",
			zap.String("hostname", hostname))
	}
}

// avahiContentForHostname returns the avahi service file content with the
// hostname-derived TXT records set. Only those records are rewritten, so the
// port and the tls/assetid/orgid records owned by UpdateAvahiForProvisioning
// survive untouched.
func avahiContentForHostname(content, hostname string) string {
	content = replaceAvahiTXTRecord(content, "name", hostname)
	content = replaceAvahiTXTRecord(content, "displayname", avahiDisplayName(hostname))
	return replaceAvahiTXTRecord(content, "fqdn", "sh.wendy."+hostname)
}

// restartAvahiDaemon restarts avahi-daemon so it re-reads both the service file
// and gethostname(). A full restart is required: --reload (SIGHUP) only
// refreshes service files, leaving the %h-based records stale.
func restartAvahiDaemon(logger *zap.Logger) bool {
	// A stuck service manager must not hold up the service-file lock's other
	// writers, among them enrollment, or the agent's shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restart := exec.CommandContext(ctx, "/usr/bin/systemctl", "restart", "avahi-daemon")
	restart.Env = systemPathEnv()
	restart.WaitDelay = time.Second
	if out, err := restart.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		logger.Warn("systemctl restart avahi-daemon failed", zap.Error(err), zap.String("output", string(out)))
		return false
	}
	return true
}

// avahiServiceFileMu serializes the agent's read-modify-write edits of the
// service file, which run from different goroutines at runtime.
var avahiServiceFileMu sync.Mutex

// LockAvahiServiceFile holds the service-file lock until unlock is called.
func LockAvahiServiceFile() (unlock func()) {
	avahiServiceFileMu.Lock()
	return avahiServiceFileMu.Unlock
}

// replaceAvahiTXTRecord replaces the value in a <txt-record>key=...</txt-record> line.
func replaceAvahiTXTRecord(content, key, value string) string {
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

// systemPathEnv returns the current environment with PATH replaced by a full
// system PATH, so tools invoked by the agent can be found even when it runs
// under systemd with a restricted PATH.
func systemPathEnv() []string {
	env := os.Environ()
	const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = "PATH=" + systemPath
			return env
		}
	}
	return append(env, "PATH="+systemPath)
}
