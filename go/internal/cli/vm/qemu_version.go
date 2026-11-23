package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrQEMUTooOld reports an emulator that cannot run the x86-64 guest without
// acceleration: emulated AVX2, which the image is built for, arrived in 7.2.
var ErrQEMUTooOld = errors.New("QEMU is too old to emulate the x86-64 WendyOS guest")

const minEmulatedX86Major, minEmulatedX86Minor = 7, 2

var qemuVersionBanner = regexp.MustCompile(`QEMU emulator version (\d+)\.(\d+)`)

// QEMUVersion runs the guest's emulator with --version and returns its major
// and minor version.
func QEMUVersion(arch Arch, goos string) (major, minor int, err error) {
	path, err := FindQEMU(arch, goos)
	if err != nil {
		return 0, 0, err
	}
	out, err := vmCommandOutput(path, "--version")
	if err != nil {
		return 0, 0, fmt.Errorf("running %s --version: %w", path, err)
	}
	m := qemuVersionBanner.FindSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("%s --version printed no QEMU version", path)
	}
	major, _ = strconv.Atoi(string(m[1]))
	minor, _ = strconv.Atoi(string(m[2]))
	return major, minor, nil
}

// CheckEmulatedGuest refuses an x86-64 guest that would run emulated on a QEMU
// known to be older than 7.2, which would boot and then crash on the first
// AVX2 instruction. An accelerated or ARM64 guest is not checked, and neither
// is an emulator whose version cannot be read: QEMU itself then decides.
func CheckEmulatedGuest(arch Arch, accel Accel, goos string) error {
	if !arch.isAMD64() || accel != AccelTCG {
		return nil
	}
	major, minor, err := QEMUVersion(arch, goos)
	if err != nil {
		return nil
	}
	if major > minEmulatedX86Major || (major == minEmulatedX86Major && minor >= minEmulatedX86Minor) {
		return nil
	}
	ways := "Update QEMU, or create the simulator with --arch arm64"
	if goos == "linux" {
		ways = "Enable KVM (check that /dev/kvm exists and your user may open it), update QEMU, or create the simulator with --arch arm64"
	}
	return fmt.Errorf("%w: found QEMU %d.%d, and emulating this guest needs %d.%d or newer. %s",
		ErrQEMUTooOld, major, minor, minEmulatedX86Major, minEmulatedX86Minor, ways)
}
