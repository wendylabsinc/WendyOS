//go:build darwin

package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// Hardware-gated checks for the darwin config-partition write. Skipped unless
// WENDY_HW_DISK names a disk carrying a partition labelled "config" (e.g. a
// freshly written WendyOS card, or an SSD/NVMe target):
//
//	WENDY_HW_DISK=/dev/disk11 go test -count=1 ./go/internal/cli/commands/ -run TestHW_ -v
//
// On removable media these must pass WITHOUT sudo. That is the point of the fix:
// the previous mount_msdos path needed elevation and the legacy msdosfs kext,
// and tearing down the auto-mount to reach it is what produced WendyOS#1562's
// "Resource busy: exit status 71".
//
// To cover the elevated branch, point WENDY_HW_DISK at a target whose config
// volume has ownership enforcement ON — the default for built-in disks, and
// switchable on a plug-in disk with `sudo diskutil enableOwnership <part>`
// (remount for it to take effect; `disableOwnership` restores it). The volume
// then comes up owned by an unmapped uid, mountConfigPartition reports
// elevated=true, and writes go through sudoDirTarget's elevated cp.
// TestHW_WriteConfigPartition logs which branch it took, and needs sudo in that
// branch for both the write and the read-back.

func TestHW_MountConfigPartition(t *testing.T) {
	disk := os.Getenv("WENDY_HW_DISK")
	if disk == "" {
		t.Skip("set WENDY_HW_DISK=/dev/diskN to run against real hardware")
	}

	partDev, err := findConfigPartition(disk)
	if err != nil {
		t.Fatalf("findConfigPartition(%s): %v", disk, err)
	}
	t.Logf("config partition: %s", partDev)

	m, err := mountConfigPartition(partDev)
	if err != nil {
		t.Fatalf("mountConfigPartition(%s): %v", partDev, err)
	}
	defer m.release()
	t.Logf("mounted at %s (elevated=%v)", m.path, m.elevated)

	if m.path == "" {
		t.Fatal("mount returned an empty path")
	}
	if _, err := os.Stat(m.path); err != nil {
		t.Fatalf("mount point not usable: %v", err)
	}

	if m.elevated {
		t.Logf("NOTE: mount is not user-writable; writes go through sudoDirTarget")
		return
	}

	probe := filepath.Join(m.path, ".wendy-hw-test")
	if err := os.WriteFile(probe, []byte("hw test\n"), 0o644); err != nil {
		t.Fatalf("writing to a mount reported as user-writable failed: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Errorf("cleaning up probe file: %v", err)
	}
}

// TestHW_WriteConfigPartition drives the real writeConfigPartition — the
// function `wendy os install` calls — and verifies the payload the device will
// actually consume. It covers both mount branches: which one runs depends on
// whether WENDY_HW_DISK is removable or fixed media.
//
// Set WENDY_HW_AGENT to a real linux/arm64 wendy-agent build to exercise a
// realistic multi-megabyte copy (the elevated path routes it through `sudo cp`,
// so a truncating or mangling bug shows up here):
//
//	GOOS=linux GOARCH=arm64 go build -o /tmp/wendy-agent-arm64 ./go/cmd/wendy-agent
func TestHW_WriteConfigPartition(t *testing.T) {
	disk := os.Getenv("WENDY_HW_DISK")
	if disk == "" {
		t.Skip("set WENDY_HW_DISK=/dev/diskN to run against real hardware")
	}

	// A real arm64 agent when provided, else a minimal but valid arm64 ELF
	// header so the device-side validateELF equivalent below still has
	// something to check.
	agent := minimalARM64ELF()
	if p := os.Getenv("WENDY_HW_AGENT"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading WENDY_HW_AGENT %s: %v", p, err)
		}
		agent = b
		t.Logf("using real agent binary: %s (%d bytes)", p, len(agent))
	} else {
		t.Logf("using synthetic ELF stub (%d bytes); set WENDY_HW_AGENT for a realistic copy", len(agent))
	}

	creds := []wendyconf.WifiCredential{
		{SSID: "hw-test-primary", Password: "hw-test-secret", Priority: 100, Security: "wpa2"},
		{SSID: "hw-test-backup", Password: "hw-test-backup-secret", Priority: 50, Hidden: true},
	}
	const deviceName = "brave-dolphin"
	provJSON, err := json.Marshal(map[string]any{
		"enrolled": true, "cloudHost": "hw-test.invalid:443",
		"orgId": 64, "assetId": 4242,
		"keyPem":  "-----BEGIN PRIVATE KEY-----\nhw-test\n-----END PRIVATE KEY-----\n",
		"certPem": "-----BEGIN CERTIFICATE-----\nhw-test\n-----END CERTIFICATE-----\n",
	})
	if err != nil {
		t.Fatalf("building provisioning json: %v", err)
	}

	if err := writeConfigPartition(drive{DevicePath: disk}, configAgent{data: agent}, creds, deviceName, provJSON); err != nil {
		t.Fatalf("writeConfigPartition: %v", err)
	}

	// Re-locate the mount to inspect what landed. writeConfigPartition released
	// its own mount; on removable media DiskArbitration keeps the auto-mount.
	partDev, err := findConfigPartition(disk)
	if err != nil {
		t.Fatalf("findConfigPartition after write: %v", err)
	}
	m, err := mountConfigPartition(partDev)
	if err != nil {
		t.Fatalf("re-mounting to verify: %v", err)
	}
	defer m.release()
	t.Logf("verifying %s (elevated=%v)", m.path, m.elevated)

	// clock_floor is written LAST by writeConfigFilesTo, so its presence proves
	// the whole payload landed rather than a prefix of it.
	for _, name := range []string{"wendy-agent", "wendy.conf", "acme-enrollment.json", "clock_floor"} {
		size, err := hwStatSize(m, name)
		if err != nil {
			t.Errorf("%s missing from config partition: %v", name, err)
			continue
		}
		t.Logf("  %-18s %d bytes", name, size)
	}

	// The agent binary must arrive byte-for-byte: the device installs it and
	// runs it, and the elevated path copies it with root cp.
	got, err := hwReadFile(m, "wendy-agent")
	if err != nil {
		t.Fatalf("reading back wendy-agent: %v", err)
	}
	if len(got) != len(agent) {
		t.Errorf("wendy-agent truncated: got %d bytes, want %d", len(got), len(agent))
	} else if !bytes.Equal(got, agent) {
		t.Error("wendy-agent content differs from the source")
	} else {
		t.Logf("  wendy-agent verified byte-for-byte (%d bytes)", len(got))
	}

	// Mirror configpartition.validateELF: the agent DELETES the binary and
	// refuses to self-update if this check fails on-device.
	if err := assertARM64ELF(got); err != nil {
		t.Errorf("wendy-agent would be rejected by the device: %v", err)
	} else {
		t.Log("  wendy-agent passes the device's ELF validation (64-bit LE aarch64)")
	}

	conf, err := hwReadFile(m, "wendy.conf")
	if err != nil {
		t.Fatalf("reading back wendy.conf: %v", err)
	}
	for _, want := range []string{
		"ssid = hw-test-primary", "password = hw-test-secret",
		"ssid = hw-test-backup", "hidden = true",
		"[device]", "name = " + deviceName,
	} {
		if !bytes.Contains(conf, []byte(want)) {
			t.Errorf("wendy.conf missing %q; got:\n%s", want, conf)
		}
	}

	// Round-trip through the parser the agent itself uses, so this checks the
	// contract rather than the formatting.
	parsed := wendyconf.UnmarshalWiFi(parseINIForTest(conf))
	if len(parsed) != len(creds) {
		t.Errorf("agent-side parse found %d networks, want %d", len(parsed), len(creds))
	} else if parsed[0].SSID != "hw-test-primary" {
		t.Errorf("highest-priority network is %q, want hw-test-primary", parsed[0].SSID)
	} else {
		t.Logf("  wendy.conf parses back to %d networks, primary=%s", len(parsed), parsed[0].SSID)
	}

	prov, err := hwReadFile(m, "acme-enrollment.json")
	if err != nil {
		t.Fatalf("reading back acme-enrollment.json: %v", err)
	}
	if !bytes.Equal(prov, provJSON) {
		t.Errorf("acme-enrollment.json content differs:\ngot:  %s\nwant: %s", prov, provJSON)
	} else {
		t.Log("  acme-enrollment.json verified byte-for-byte")
	}
}

// hwStatSize and hwReadFile inspect the written payload. On an
// ownership-enforced mount the config volume is owned by an unmapped uid
// (-101/nobody), so the unprivileged test process cannot even stat inside it —
// the files are written correctly by the elevated cp, but only root can read
// them back. Verification therefore has to elevate exactly where the write did.
// This is a host-side artifact only: vfat carries no on-disk ownership, so the
// device applies its own uid when it boots.
func hwStatSize(m configMount, name string) (int64, error) {
	p := filepath.Join(m.path, name)
	if !m.elevated {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	}
	out, err := exec.Command("sudo", "/usr/bin/stat", "-f%z", p).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("sudo stat %s: %s: %w", p, strings.TrimSpace(string(out)), err)
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func hwReadFile(m configMount, name string) ([]byte, error) {
	p := filepath.Join(m.path, name)
	if !m.elevated {
		return os.ReadFile(p)
	}
	out, err := exec.Command("sudo", "/bin/cat", p).Output()
	if err != nil {
		return nil, fmt.Errorf("sudo cat %s: %w", p, err)
	}
	return out, nil
}

// assertARM64ELF applies the same checks as
// internal/agent/configpartition.validateELF, for the arm64 devices this
// payload targets.
func assertARM64ELF(b []byte) error {
	if len(b) < 20 {
		return errShortELF
	}
	if b[0] != 0x7f || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
		return errBadELFMagic
	}
	if b[4] != 2 {
		return errNot64Bit
	}
	if b[5] != 1 {
		return errNotLittleEndian
	}
	if machine := uint16(b[18]) | uint16(b[19])<<8; machine != 0x00B7 {
		return errWrongMachine
	}
	return nil
}

// minimalARM64ELF is just enough header to satisfy the device's validateELF.
func minimalARM64ELF() []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b[16], b[17] = 2, 0    // e_type = ET_EXEC
	b[18], b[19] = 0xB7, 0 // e_machine = EM_AARCH64
	return b
}

// parseINIForTest mirrors the agent's minimal INI parser so the round-trip
// check above does not depend on the agent package (which would be an import
// cycle from the CLI side).
func parseINIForTest(data []byte) map[string]map[string]string {
	result := make(map[string]map[string]string)
	var section string
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := string(bytes.TrimSpace(raw))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = line[1 : len(line)-1]
			if result[section] == nil {
				result[section] = make(map[string]string)
			}
			continue
		}
		if section == "" {
			continue
		}
		if i := bytes.IndexByte([]byte(line), '='); i > 0 {
			k := string(bytes.TrimSpace([]byte(line[:i])))
			v := string(bytes.TrimSpace([]byte(line[i+1:])))
			result[section][k] = v
		}
	}
	return result
}

// Sentinel errors for assertARM64ELF, matching the device's rejection reasons.
var (
	errShortELF        = elfErr("file too short to contain an ELF header")
	errBadELFMagic     = elfErr("not an ELF binary (bad magic)")
	errNot64Bit        = elfErr("not a 64-bit ELF")
	errNotLittleEndian = elfErr("not a little-endian ELF")
	errWrongMachine    = elfErr("e_machine is not EM_AARCH64")
)

type elfErr string

func (e elfErr) Error() string { return string(e) }
