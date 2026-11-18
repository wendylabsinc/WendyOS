// Package vm runs a WendyOS ARM64 virtual machine on the developer's own
// machine, so WendyOS can be evaluated and developed against without a Jetson
// or a Raspberry Pi on the desk.
package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Seams so tests can describe a host they are not running on.
var (
	vmStat          = os.Stat
	vmOpen          = os.Open
	vmLookPath      = exec.LookPath
	vmBrewCommand   = exec.Command
	vmHostX86V3     = hostX86V3
	vmCommandOutput = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() }
)

// ErrFirmwareNotFound reports that no host UEFI firmware image was located.
// oe-core's only firmware recipe, ovmf, is x86_64-only, so aarch64 firmware
// always comes from the host's QEMU installation.
var ErrFirmwareNotFound = errors.New("no ARM64 UEFI firmware found")

// FindQEMU locates the system emulator for the guest architecture. It also
// checks the Windows installer's default directory, since the installer does
// not add QEMU to PATH.
func FindQEMU(arch Arch, goos string) (string, error) {
	path, err := vmLookPath(arch.QEMUBinary())
	if err == nil || goos != "windows" {
		return path, err
	}
	path = filepath.Join(windowsQEMUDir(), arch.QEMUBinary()+".exe")
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		return path, nil
	}
	return "", err
}

func windowsQEMUDir() string {
	root := os.Getenv("ProgramFiles")
	if root == "" {
		root = `C:\Program Files`
	}
	return filepath.Join(root, "qemu")
}

// pflashBytes is the size the virt machine's pflash unit maps, and therefore the
// size both the firmware image and its variable store must be.
const pflashBytes = 64 << 20

// Accel names the CPU acceleration available to a guest.
type Accel string

const (
	AccelHVF Accel = "hvf" // macOS, via Hypervisor.framework
	AccelKVM Accel = "kvm" // Linux
	AccelTCG Accel = "tcg" // software emulation: correct everywhere, slow
)

// AccelFor reports the best acceleration for a guest of the given architecture
// on the given host. Acceleration needs the host and guest architectures to
// match; everything else emulates. The x86-64 image is built for x86-64-v3, so
// an x86-64 guest is accelerated only on a host CPU with those features.
func AccelFor(arch Arch, goos, goarch string) Accel {
	if arch.isAMD64() {
		if goarch != "amd64" || !vmHostX86V3() {
			return AccelTCG
		}
		switch goos {
		case "darwin":
			return AccelHVF
		case "linux":
			if kvmUsable() {
				return AccelKVM
			}
		}
		return AccelTCG
	}
	if goarch != "arm64" {
		return AccelTCG
	}
	switch goos {
	case "darwin":
		return AccelHVF
	case "linux":
		if kvmUsable() {
			return AccelKVM
		}
		return AccelTCG
	default:
		return AccelTCG
	}
}

// kvmUsable reports whether this process can open /dev/kvm. It may be absent
// or unreadable. Emulating is slow but works; asking for KVM without it aborts
// QEMU.
func kvmUsable() bool {
	f, err := vmOpen("/dev/kvm")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// CPUModel returns the -cpu value for a guest and acceleration mode. An
// emulated guest has no physical CPU to pass through, so it gets a concrete
// model: for ARM64 the one upstream qemuarm64.conf uses, for x86-64 a model
// with the x86-64-v3 features the image is built for. -cpu max is avoided: it
// boots, but Mesa's software renderer then aborts.
func CPUModel(arch Arch, a Accel) string {
	if a != AccelTCG {
		return "host"
	}
	if arch.isAMD64() {
		return "Skylake-Client"
	}
	return "cortex-a57"
}

// MachineType returns the -machine value for a guest and acceleration mode.
// x86-64 guests use q35. For ARM64 only HVF pins the GIC, having no GICv2
// implementation; KVM must be left free to match a host that may be GICv2.
func MachineType(arch Arch, a Accel) string {
	if arch.isAMD64() {
		return "q35"
	}
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
		dir := windowsQEMUDir()
		candidates := []string{filepath.Join(dir, "share", "edk2-aarch64-code.fd"), filepath.Join(dir, "edk2-aarch64-code.fd")}
		if binary, err := FindQEMU(ArchARM64, goos); err == nil {
			if installed := filepath.Dir(binary); installed != dir {
				candidates = append([]string{filepath.Join(installed, "share", "edk2-aarch64-code.fd"), filepath.Join(installed, "edk2-aarch64-code.fd")}, candidates...)
			}
		}
		return candidates
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
