package vm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Defaults match the Builder's own run-vm.sh, so a VM started by this launcher
// has the same shape the image was built and tested against.
const (
	DefaultMemoryMiB = 4096
	DefaultCPUs      = 4
)

// Spec is everything needed to launch one VM.
type Spec struct {
	Name         string
	DiskPath     string
	FirmwareCode string
	FirmwareVars string
	MemoryMiB    int
	CPUs         int
	Accel        Accel
	Net          NetConfig

	// ConsoleLog, when set, sends the guest console and QEMU's own diagnostics
	// to this file instead of the terminal. A detached VM has no terminal to
	// attach to, and the log is the only record of a boot that failed.
	ConsoleLog string
	// QMPPath is a private control socket in the VM's owner-only directory.
	QMPPath string
}

// Binary names the emulator. Always the aarch64 system emulator: the guest is
// ARM64 whatever the host is, because real WendyOS devices are ARM64.
func (s Spec) Binary() string { return "qemu-system-aarch64" }

// Args returns the full argument list for Binary().
func (s Spec) Args() ([]string, error) {
	if s.Name == "" {
		return nil, errors.New("no VM name")
	}
	if s.DiskPath == "" {
		return nil, errors.New("no disk image")
	}
	if s.FirmwareCode == "" {
		return nil, errors.New("no UEFI firmware image")
	}
	if s.FirmwareVars == "" {
		return nil, errors.New("no UEFI variable store")
	}

	mem := s.MemoryMiB
	if mem == 0 {
		mem = DefaultMemoryMiB
	}
	cpus := s.CPUs
	if cpus == 0 {
		cpus = DefaultCPUs
	}

	args := []string{
		// Names the guest in ps, so one VM can be found and stopped without
		// matching on the emulator binary.
		"-name", s.Name,
		"-machine", MachineType(s.Accel),
		"-accel", string(s.Accel),
		"-cpu", CPUModel(s.Accel),
		"-smp", strconv.Itoa(cpus),
		"-m", strconv.Itoa(mem),

		// The shared firmware code stays read-only; only the per-VM variable
		// store is writable. Order matters: edk2 expects code then vars, and
		// QEMU maps pflash units in the order given.
		"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", escapeQEMUValue(s.FirmwareCode)),
		"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", escapeQEMUValue(s.FirmwareVars)),

		// No -kernel: the image boots itself through its own ESP and GRUB, which
		// is what makes it a single downloadable artifact.
		"-drive", fmt.Sprintf("file=%s,if=none,format=raw,id=hd0", escapeQEMUValue(s.DiskPath)),
		"-device", "virtio-blk-pci,drive=hd0",
	}
	args = append(args, s.consoleArgs()...)
	if s.QMPPath != "" {
		args = append(args, "-qmp", "unix:"+escapeQEMUValue(s.QMPPath)+",server=on,wait=off")
	}

	netArgs, err := s.Net.Args()
	if err != nil {
		return nil, err
	}
	return append(args, netArgs...), nil
}

// consoleArgs routes the guest console, either to the terminal or to a file.
//
// The detached form spells out -display, -serial and -monitor rather than using
// -nographic, which implies "serial and monitor to stdio unless overridden":
// leaving the monitor pointed at a closed stdio kills the VM on its first
// monitor write.
func (s Spec) consoleArgs() []string {
	if s.ConsoleLog == "" {
		// mon:stdio keeps the QEMU monitor reachable with Ctrl-A x, the only
		// way out of a headless guest.
		return []string{"-nographic", "-serial", "mon:stdio"}
	}
	// append=on, because the caller also points QEMU's own stdout and stderr at
	// this file: without it QEMU truncates and writes from offset 0, clobbering
	// the diagnostics that explain a failed boot.
	return []string{
		"-display", "none",
		"-chardev", "file,id=console,append=on,path=" + escapeQEMUValue(s.ConsoleLog),
		"-serial", "chardev:console",
		"-monitor", "none",
	}
}

// escapeQEMUValue doubles commas, which separate QEMU option parameters. A home
// directory may contain one, and an unescaped comma silently truncates the path.
func escapeQEMUValue(v string) string { return strings.ReplaceAll(v, ",", ",,") }
