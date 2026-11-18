package vm

import (
	"fmt"
	"runtime"
	"strings"
)

// Arch is a simulator's guest architecture. It is recorded in meta.json when
// the VM is created and never changes. The zero value means ARM64, which is
// what every VM created before the field existed runs.
type Arch string

const (
	ArchARM64 Arch = "arm64"
	ArchAMD64 Arch = "amd64"
)

// ParseArch accepts the usual spellings of the two guest architectures,
// ignoring case and surrounding space.
func ParseArch(s string) (Arch, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "arm64", "aarch64":
		return ArchARM64, nil
	case "amd64", "x86_64", "x86-64", "x64":
		return ArchAMD64, nil
	}
	return "", fmt.Errorf("unknown guest architecture %q: use arm64 or amd64", s)
}

// Valid reports whether a is a guest architecture this CLI can run. The zero
// value is valid and means ARM64.
func (a Arch) Valid() bool {
	return a == "" || a == ArchARM64 || a == ArchAMD64
}

func (a Arch) isAMD64() bool { return a == ArchAMD64 }

// BoardID is the WendyOS board that publishes this guest's VM image, and the
// device type its agent reports.
func (a Arch) BoardID() string {
	if a.isAMD64() {
		return "vm-x86-64"
	}
	return "vm-arm64"
}

// QEMUBinary is the system emulator that runs this guest.
func (a Arch) QEMUBinary() string {
	if a.isAMD64() {
		return "qemu-system-x86_64"
	}
	return "qemu-system-aarch64"
}

// Label is the architecture's name in user-facing text.
func (a Arch) Label() string {
	if a.isAMD64() {
		return "x86-64"
	}
	return "ARM64"
}

// ArchForBoard maps an agent's reported device type to the guest architecture
// of that WendyOS VM board. Only the two VM boards are known; physical
// devices, including x86 ones, are not.
func ArchForBoard(deviceType string) (Arch, bool) {
	switch deviceType {
	case "vm-arm64":
		return ArchARM64, true
	case "vm-x86-64":
		return ArchAMD64, true
	}
	return "", false
}

// GuestArch is the VM's recorded guest architecture. A record written before
// the field existed is ARM64.
func (m Meta) GuestArch() Arch {
	if m.Arch == "" {
		return ArchARM64
	}
	return m.Arch
}

// DefaultArch is the guest architecture a new simulator gets on this host.
func DefaultArch() Arch {
	return DefaultArchFor(runtime.GOOS, runtime.GOARCH)
}

// DefaultArchFor is the guest architecture a new simulator gets on the given
// host. Every host gets ARM64 until the x86-64 guest ships as a default.
func DefaultArchFor(goos, goarch string) Arch {
	return ArchARM64
}
