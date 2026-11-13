package vm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindQEMUOnWindows(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", root)
	path := filepath.Join(root, "qemu", "qemu-system-aarch64.exe")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := vmLookPath
	t.Cleanup(func() { vmLookPath = saved })
	vmLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
	if _, err := FindQEMU("windows"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing binary: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := FindQEMU("windows"); err == nil {
		t.Fatal("accepted a directory as QEMU")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := FindQEMU("windows"); err != nil || got != path {
		t.Fatalf("got %q, %v; want %q", got, err, path)
	}
	if _, err := FindQEMU("linux"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("used Windows fallback on Linux: %v", err)
	}
	preferred := filepath.Join(t.TempDir(), "qemu-system-aarch64.exe")
	vmLookPath = func(string) (string, error) { return preferred, nil }
	if got, err := FindQEMU("windows"); err != nil || got != preferred {
		t.Fatalf("PATH did not take precedence: %q, %v", got, err)
	}
}

func TestWindowsFirmwareFollowsQEMUInstallation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", root)
	saved := vmLookPath
	t.Cleanup(func() { vmLookPath = saved })
	for _, fromPath := range []bool{false, true} {
		dir := filepath.Join(root, "qemu")
		vmLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
		if fromPath {
			dir = t.TempDir()
			vmLookPath = func(string) (string, error) { return filepath.Join(dir, "qemu-system-aarch64.exe"), nil }
		}
		firmware := filepath.Join(dir, "share", "edk2-aarch64-code.fd")
		stubFirmware(t, map[string]int64{firmware: pflashBytes})
		if got, err := FindFirmware("windows", ""); err != nil || got != firmware {
			t.Fatalf("firmware=%q err=%v want %q", got, err, firmware)
		}
	}
	// A PATH shim need not have firmware beside it. Keep the default install
	// as a fallback even when the executable lives elsewhere.
	firmware := filepath.Join(root, "qemu", "share", "edk2-aarch64-code.fd")
	stubFirmware(t, map[string]int64{firmware: pflashBytes})
	if got, err := FindFirmware("windows", ""); err != nil || got != firmware {
		t.Fatalf("default firmware fallback=%q err=%v want %q", got, err, firmware)
	}
}

// stubKVM controls whether the fake host has a usable /dev/kvm.
func stubKVM(t *testing.T, present bool) {
	t.Helper()
	orig := vmOpen
	t.Cleanup(func() { vmOpen = orig })
	vmOpen = func(name string) (*os.File, error) {
		if present && name == "/dev/kvm" {
			return os.NewFile(0, name), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestAccelFor(t *testing.T) {
	stubKVM(t, true)
	for _, tc := range []struct {
		goos, goarch string
		want         Accel
	}{
		{"darwin", "arm64", AccelHVF},
		{"linux", "arm64", AccelKVM},
		{"darwin", "amd64", AccelTCG},
		{"linux", "amd64", AccelTCG},
		{"windows", "amd64", AccelTCG},
	} {
		if got := AccelFor(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AccelFor(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestAccelForFallsBackToTCGWithoutKVM(t *testing.T) {
	// No nested virt, or the user is not in the kvm group. Emulating is slow but
	// works; asking for KVM without it aborts.
	stubKVM(t, false)
	if got := AccelFor("linux", "arm64"); got != AccelTCG {
		t.Errorf("AccelFor(linux, arm64) without /dev/kvm = %q, want tcg", got)
	}
}

func TestCPUModelAndMachineType(t *testing.T) {
	// Accelerated hosts pass the physical CPU through; TCG must name a concrete
	// ARMv8 model because there is no host CPU to inherit.
	if got := CPUModel(AccelHVF); got != "host" {
		t.Errorf("CPUModel(hvf) = %q, want host", got)
	}
	if got := CPUModel(AccelKVM); got != "host" {
		t.Errorf("CPUModel(kvm) = %q, want host", got)
	}
	if got := CPUModel(AccelTCG); got != "cortex-a57" {
		t.Errorf("CPUModel(tcg) = %q, want cortex-a57", got)
	}

	// HVF has no GICv2 emulation, so it must pin GICv3. KVM must NOT: an ARM64
	// host with a GICv2 aborts on a GICv3 request, where the default matches.
	if got := MachineType(AccelHVF); !strings.Contains(got, "gic-version=3") {
		t.Errorf("MachineType(hvf) = %q, want gic-version=3", got)
	}
	if got := MachineType(AccelKVM); got != "virt" {
		t.Errorf("MachineType(kvm) = %q, want plain virt so the host GIC is matched", got)
	}
	if got := MachineType(AccelTCG); got != "virt" {
		t.Errorf("MachineType(tcg) = %q, want plain virt", got)
	}
}

func TestFirmwareCandidatesUsesBrewPrefixFirstOnDarwin(t *testing.T) {
	got := FirmwareCandidates("darwin", "/opt/homebrew")
	if len(got) == 0 {
		t.Fatal("no candidates for darwin")
	}
	if got[0] != "/opt/homebrew/share/qemu/edk2-aarch64-code.fd" {
		t.Errorf("first darwin candidate = %q, want the brew-prefix path", got[0])
	}
}

func TestFirmwareCandidatesCoversDebianAndFedoraLayoutsOnLinux(t *testing.T) {
	got := strings.Join(FirmwareCandidates("linux", ""), "\n")
	for _, want := range []string{
		"/usr/share/AAVMF/AAVMF_CODE.fd",
		"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
		"/usr/share/edk2/aarch64/QEMU_EFI.silent.fd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("linux candidates missing %q; got:\n%s", want, got)
		}
	}
}

// fakeFileInfo reports only a size; FindFirmware consults nothing else.
type fakeFileInfo struct {
	os.FileInfo
	size int64
}

func (f fakeFileInfo) Size() int64 { return f.size }

// stubFirmware makes vmStat report the given paths at the given sizes.
func stubFirmware(t *testing.T, sizes map[string]int64) {
	t.Helper()
	orig := vmStat
	t.Cleanup(func() { vmStat = orig })
	vmStat = func(name string) (os.FileInfo, error) {
		if s, ok := sizes[name]; ok {
			return fakeFileInfo{size: s}, nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestFindFirmwareReturnsTheFirstUsableCandidate(t *testing.T) {
	want := "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd"
	stubFirmware(t, map[string]int64{want: pflashBytes})

	got, err := FindFirmware("linux", "")
	if err != nil {
		t.Fatalf("FindFirmware() error = %v", err)
	}
	if got != want {
		t.Errorf("FindFirmware() = %q, want %q", got, want)
	}
}

func TestFindFirmwareSkipsUnpaddedFirmware(t *testing.T) {
	// Debian's qemu-efi-aarch64 ships both: a padded AAVMF_CODE.fd and a 2 MiB
	// QEMU_EFI.fd that QEMU rejects as pflash. Picking the wrong one fails at
	// start, long after create reported success.
	padded := "/usr/share/AAVMF/AAVMF_CODE.fd"
	stubFirmware(t, map[string]int64{
		"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd": 2 << 20,
		padded: pflashBytes,
	})

	got, err := FindFirmware("linux", "")
	if err != nil {
		t.Fatalf("FindFirmware() error = %v", err)
	}
	if got != padded {
		t.Errorf("FindFirmware() = %q, want the padded %q", got, padded)
	}
}

func TestFindFirmwareRejectsAnUnpaddedOnlyHost(t *testing.T) {
	stubFirmware(t, map[string]int64{"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd": 2 << 20})
	if _, err := FindFirmware("linux", ""); !errors.Is(err, ErrFirmwareNotFound) {
		t.Errorf("unpadded-only host should report firmware not found; got %v", err)
	}
}

func TestFindFirmwareErrorNamesTheInstallCommand(t *testing.T) {
	stubFirmware(t, nil)

	_, err := FindFirmware("darwin", "/opt/homebrew")
	if err == nil {
		t.Fatal("expected an error when no firmware is installed")
	}
	if !strings.Contains(err.Error(), "brew install qemu") {
		t.Errorf("error should tell the user how to fix it; got %q", err)
	}
	if !errors.Is(err, ErrFirmwareNotFound) {
		t.Errorf("error should wrap ErrFirmwareNotFound; got %#v", err)
	}
}
