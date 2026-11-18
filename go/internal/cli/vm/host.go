// Package vm runs a WendyOS virtual machine on the developer's own machine, so
// WendyOS can be evaluated and developed against without a Jetson or a
// Raspberry Pi on the desk. The guest is ARM64, or x86-64 when the VM was
// created for it.
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

// ErrFirmwareNotFound reports that no host UEFI firmware image was located,
// for either guest architecture. Its own text names ARM64, which is what it
// has always reported; FindFirmware's errors match it with errors.Is and name
// the guest they were looking for. Firmware always comes from the host: the
// WendyOS image carries none.
var ErrFirmwareNotFound = errors.New("no ARM64 UEFI firmware found")

// firmwareNotFoundError is FindFirmware's error: it names the guest, how to
// install the firmware and where it looked.
type firmwareNotFoundError struct {
	arch   Arch
	detail string
}

func (e *firmwareNotFoundError) Error() string {
	return fmt.Sprintf("no %s UEFI firmware found; %s", e.arch.Label(), e.detail)
}

func (e *firmwareNotFoundError) Is(target error) bool { return target == ErrFirmwareNotFound }

// Firmware is a UEFI code image and, for an x86-64 guest, the variables
// template its per-VM store is copied from.
type Firmware struct {
	Code         string
	VarsTemplate string // empty for ARM64, whose store starts zero-filled
}

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

// FirmwareCandidates lists the UEFI firmware to try for a guest, most likely
// first. Layouts differ per packager, so the common ones are named outright.
func FirmwareCandidates(arch Arch, goos, brewPrefix string) []Firmware {
	if arch.isAMD64() {
		return x86FirmwareCandidates(goos, brewPrefix)
	}
	var codes []string
	switch goos {
	case "darwin":
		for _, prefix := range brewPrefixes(brewPrefix) {
			codes = append(codes, prefix+"/share/qemu/edk2-aarch64-code.fd")
		}
	case "windows":
		dir := windowsQEMUDir()
		codes = []string{filepath.Join(dir, "share", "edk2-aarch64-code.fd"), filepath.Join(dir, "edk2-aarch64-code.fd")}
		if binary, err := FindQEMU(ArchARM64, goos); err == nil {
			if installed := filepath.Dir(binary); installed != dir {
				codes = append([]string{filepath.Join(installed, "share", "edk2-aarch64-code.fd"), filepath.Join(installed, "edk2-aarch64-code.fd")}, codes...)
			}
		}
	default:
		codes = []string{
			"/usr/share/AAVMF/AAVMF_CODE.fd",
			"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
			"/usr/share/edk2/aarch64/QEMU_EFI.silent.fd",
			"/usr/share/qemu/edk2-aarch64-code.fd",
		}
	}
	out := make([]Firmware, 0, len(codes))
	for _, code := range codes {
		out = append(out, Firmware{Code: code})
	}
	return out
}

// x86FirmwareCandidates lists OVMF code images with the variables template
// each pairs with: Debian and Ubuntu, Fedora, Arch, openSUSE, then QEMU's own
// install, which Homebrew also uses. Secure Boot builds are never listed: they
// refuse WendyOS's unsigned GRUB. Native Windows has none: the x86-64 guest is
// not offered there.
func x86FirmwareCandidates(goos, brewPrefix string) []Firmware {
	switch goos {
	case "linux":
		return []Firmware{
			{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
			{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
			{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
			{"/usr/share/edk2/x64/OVMF_CODE.4m.fd", "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
			{"/usr/share/qemu/ovmf-x86_64-4m-code.bin", "/usr/share/qemu/ovmf-x86_64-4m-vars.bin"},
			{"/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"},
			{"/usr/share/qemu/edk2-x86_64-code.fd", "/usr/share/qemu/edk2-i386-vars.fd"},
		}
	case "darwin":
		var out []Firmware
		for _, prefix := range brewPrefixes(brewPrefix) {
			out = append(out, Firmware{prefix + "/share/qemu/edk2-x86_64-code.fd", prefix + "/share/qemu/edk2-i386-vars.fd"})
		}
		return out
	default:
		return nil
	}
}

// brewPrefixes is where Homebrew may keep QEMU: the reported prefix, then the
// Apple Silicon and Intel defaults.
func brewPrefixes(brewPrefix string) []string {
	var out []string
	if brewPrefix != "" {
		out = append(out, brewPrefix)
	}
	return append(out, "/opt/homebrew", "/usr/local")
}

// FindFirmware returns the first usable firmware for the guest. An ARM64 code
// image must be padded to the pflash size the virt machine maps: distributions
// ship unpadded images under similar names, and QEMU rejects those only at
// boot, long after `vm create` succeeded. An x86-64 code image is usable when
// it and its variables template both exist and are not empty.
func FindFirmware(arch Arch, goos, brewPrefix string) (Firmware, error) {
	candidates := FirmwareCandidates(arch, goos, brewPrefix)
	for _, c := range candidates {
		if arch.isAMD64() {
			if nonEmptyFile(c.Code) && nonEmptyFile(c.VarsTemplate) {
				return c, nil
			}
			continue
		}
		if info, err := vmStat(c.Code); err == nil && info != nil && info.Size() == pflashBytes {
			return c, nil
		}
	}
	if arch.isAMD64() && goos == "windows" {
		return Firmware{}, &firmwareNotFoundError{arch: arch, detail: "the x86-64 guest is not available on native Windows"}
	}
	return Firmware{}, &firmwareNotFoundError{arch: arch, detail: fmt.Sprintf("install it with %s (looked in: %s)",
		firmwareInstallHint(arch, goos), strings.Join(firmwareCodes(candidates), ", "))}
}

func nonEmptyFile(path string) bool {
	info, err := vmStat(path)
	return err == nil && info != nil && info.Size() > 0
}

func firmwareCodes(candidates []Firmware) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Code)
	}
	return out
}

// firmwareInstallHint names the command that installs the firmware, so the
// error says how to fix it.
func firmwareInstallHint(arch Arch, goos string) string {
	switch goos {
	case "darwin":
		return "brew install qemu"
	case "windows":
		return "the QEMU for Windows installer, which bundles it"
	}
	if arch.isAMD64() {
		return "apt install ovmf (or dnf install edk2-ovmf)"
	}
	return "apt install qemu-efi-aarch64 (or dnf install edk2-aarch64)"
}
