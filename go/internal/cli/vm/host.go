// Package vm runs a WendyOS ARM64 virtual machine on the developer's own
// machine, so WendyOS can be evaluated and developed against without a Jetson
// or a Raspberry Pi on the desk.
package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Seams so tests can describe a host they are not running on.
var (
	vmStat        = os.Stat
	vmOpen        = os.Open
	vmLookPath    = exec.LookPath
	vmBrewCommand = exec.Command
)

// ErrFirmwareNotFound reports that no host UEFI firmware image was located.
// oe-core's only firmware recipe, ovmf, is x86_64-only, so aarch64 firmware
// always comes from the host's QEMU installation.
var ErrFirmwareNotFound = errors.New("no ARM64 UEFI firmware found")

// pflashBytes is the size the virt machine's pflash unit maps, and therefore the
// size both the firmware image and its variable store must be.
const pflashBytes = 64 << 20

// Accel names the CPU acceleration available to an ARM64 guest.
type Accel string

const (
	AccelHVF Accel = "hvf" // Apple Silicon, via Hypervisor.framework
	AccelKVM Accel = "kvm" // ARM64 Linux
	AccelTCG Accel = "tcg" // software emulation: correct everywhere, slow
)

// AccelFor reports the best acceleration for an ARM64 guest on the given host.
// Acceleration needs the host and guest architectures to match, so only ARM64
// hosts get it; everything else emulates.
func AccelFor(goos, goarch string) Accel {
	if goarch != "arm64" {
		return AccelTCG
	}
	switch goos {
	case "darwin":
		return AccelHVF
	case "linux":
		// /dev/kvm may be absent or unreadable. Emulating is slow but works;
		// asking for KVM without it aborts QEMU.
		if f, err := vmOpen("/dev/kvm"); err == nil {
			_ = f.Close()
			return AccelKVM
		}
		return AccelTCG
	default:
		return AccelTCG
	}
}

// CPUModel returns the -cpu value for an acceleration mode. An emulated guest
// has no physical CPU to pass through, so it gets the concrete model upstream
// qemuarm64.conf uses.
func CPUModel(a Accel) string {
	if a == AccelTCG {
		return "cortex-a57"
	}
	return "host"
}

// MachineType returns the -machine value for an acceleration mode. Only HVF
// pins the GIC, having no GICv2 implementation; KVM must be left free to match
// a host that may be GICv2.
func MachineType(a Accel) string {
	if a == AccelHVF {
		return "virt,gic-version=3"
	}
	return "virt"
}

// BrewPrefix reports the Homebrew prefix, or "" when brew is not installed.
// Used only to locate QEMU's bundled firmware on macOS.
func BrewPrefix() string {
	if _, err := vmLookPath("brew"); err != nil {
		return ""
	}
	out, err := vmBrewCommand("brew", "--prefix").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// FirmwareCandidates lists the UEFI code images to try, most likely first.
// Layouts differ per packager, so the common ones are named outright.
func FirmwareCandidates(goos, brewPrefix string) []string {
	switch goos {
	case "darwin":
		var out []string
		if brewPrefix != "" {
			out = append(out, brewPrefix+"/share/qemu/edk2-aarch64-code.fd")
		}
		return append(out,
			"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
			"/usr/local/share/qemu/edk2-aarch64-code.fd",
		)
	case "windows":
		return []string{
			`C:\Program Files\qemu\share\edk2-aarch64-code.fd`,
			`C:\Program Files\qemu\edk2-aarch64-code.fd`,
		}
	default:
		return []string{
			"/usr/share/AAVMF/AAVMF_CODE.fd",
			"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
			"/usr/share/edk2/aarch64/QEMU_EFI.silent.fd",
			"/usr/share/qemu/edk2-aarch64-code.fd",
		}
	}
}

// FindFirmware returns the first firmware image padded to the pflash size the
// virt machine maps. Distributions ship unpadded images under similar names,
// and QEMU rejects those only at boot, long after `vm create` succeeded.
func FindFirmware(goos, brewPrefix string) (string, error) {
	candidates := FirmwareCandidates(goos, brewPrefix)
	for _, path := range candidates {
		if info, err := vmStat(path); err == nil && info != nil && info.Size() == pflashBytes {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w; install it with %s (looked in: %s)",
		ErrFirmwareNotFound, firmwareInstallHint(goos), strings.Join(candidates, ", "))
}

// firmwareInstallHint names the command that installs the firmware, so the
// error says how to fix it.
func firmwareInstallHint(goos string) string {
	switch goos {
	case "darwin":
		return "brew install qemu"
	case "windows":
		return "the QEMU for Windows installer, which bundles it"
	default:
		return "apt install qemu-efi-aarch64 (or dnf install edk2-aarch64)"
	}
}
