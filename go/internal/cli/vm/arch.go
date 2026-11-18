package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// GuestArchFile is a sidecar beside meta.json that records the guest
// architecture again. CLIs released before the arch field read meta.json into a
// struct without it and write it back, dropping it; they never touch this file,
// so a VM keeps its architecture after an older CLI updated its record.
const GuestArchFile = "guest-arch.json"

type guestArchRecord struct {
	Arch Arch `json:"arch"`
}

// GuestArchPath is the VM's guest-architecture sidecar.
func (s *Store) GuestArchPath(name string) string { return filepath.Join(s.Dir(name), GuestArchFile) }

// recordedGuestArch is the architecture in the VM's sidecar, or "" when it has
// none. An unknown value is returned as is, so starting the VM refuses it.
func (s *Store) recordedGuestArch(name string) Arch {
	data, err := os.ReadFile(s.GuestArchPath(name))
	if err != nil {
		return ""
	}
	var r guestArchRecord
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	return r.Arch
}

// fillGuestArch restores an architecture an older CLI dropped from meta.json.
func (s *Store) fillGuestArch(name string, m *Meta) {
	if m.Arch == "" && !IsLiteProfile(m.Profile) {
		m.Arch = s.recordedGuestArch(name)
	}
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
