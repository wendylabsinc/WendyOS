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
	if _, err := FindQEMU(ArchARM64, "windows"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing binary: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := FindQEMU(ArchARM64, "windows"); err == nil {
		t.Fatal("accepted a directory as QEMU")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := FindQEMU(ArchARM64, "windows"); err != nil || got != path {
		t.Fatalf("got %q, %v; want %q", got, err, path)
	}
	if _, err := FindQEMU(ArchARM64, "linux"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("used Windows fallback on Linux: %v", err)
	}
	preferred := filepath.Join(t.TempDir(), "qemu-system-aarch64.exe")
	vmLookPath = func(string) (string, error) { return preferred, nil }
	if got, err := FindQEMU(ArchARM64, "windows"); err != nil || got != preferred {
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
		if got, err := FindFirmware(ArchARM64, "windows", ""); err != nil || got.Code != firmware {
			t.Fatalf("firmware=%q err=%v want %q", got.Code, err, firmware)
		}
	}
	// A PATH shim need not have firmware beside it. Keep the default install
	// as a fallback even when the executable lives elsewhere.
	firmware := filepath.Join(root, "qemu", "share", "edk2-aarch64-code.fd")
	stubFirmware(t, map[string]int64{firmware: pflashBytes})
	if got, err := FindFirmware(ArchARM64, "windows", ""); err != nil || got.Code != firmware {
		t.Fatalf("default firmware fallback=%q err=%v want %q", got.Code, err, firmware)
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

// stubHostX86V3 controls whether the fake host CPU has the x86-64-v3 features.
func stubHostX86V3(t *testing.T, v3 bool) {
	t.Helper()
	orig := vmHostX86V3
	t.Cleanup(func() { vmHostX86V3 = orig })
	vmHostX86V3 = func() bool { return v3 }
}

func TestAccelFor(t *testing.T) {
	for _, tc := range []struct {
		arch         Arch
		goos, goarch string
		kvmOpens, v3 bool
		want         Accel
	}{
		{ArchARM64, "darwin", "arm64", true, false, AccelHVF},
		{ArchARM64, "linux", "arm64", true, false, AccelKVM},
		{ArchARM64, "darwin", "amd64", true, false, AccelTCG},
		{ArchARM64, "linux", "amd64", true, false, AccelTCG},
		{ArchARM64, "windows", "amd64", true, false, AccelTCG},
		{Arch(""), "linux", "arm64", true, false, AccelKVM},
		{ArchAMD64, "linux", "amd64", true, true, AccelKVM},
		{ArchAMD64, "linux", "amd64", false, true, AccelTCG}, // /dev/kvm cannot be opened
		{ArchAMD64, "linux", "amd64", true, false, AccelTCG}, // CPU below x86-64-v3
		{ArchAMD64, "darwin", "amd64", false, true, AccelHVF},
		{ArchAMD64, "darwin", "amd64", false, false, AccelTCG},
		{ArchAMD64, "darwin", "arm64", false, false, AccelTCG},
		{ArchAMD64, "linux", "arm64", true, false, AccelTCG},
		{ArchAMD64, "windows", "amd64", false, true, AccelTCG},
		{ArchARM64, "linux", "amd64", true, true, AccelTCG},
	} {
		stubKVM(t, tc.kvmOpens)
		stubHostX86V3(t, tc.v3)
		if got := AccelFor(tc.arch, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AccelFor(%q, %q, %q) with kvm=%t v3=%t = %q, want %q", tc.arch, tc.goos, tc.goarch, tc.kvmOpens, tc.v3, got, tc.want)
		}
	}
}

func TestAccelForFallsBackToTCGWithoutKVM(t *testing.T) {
	// No nested virt, or the user is not in the kvm group. Emulating is slow but
	// works; asking for KVM without it aborts.
	stubKVM(t, false)
	if got := AccelFor(ArchARM64, "linux", "arm64"); got != AccelTCG {
		t.Errorf("AccelFor(linux, arm64) without /dev/kvm = %q, want tcg", got)
	}
}

func TestCPUModelAndMachineType(t *testing.T) {
	// Accelerated hosts pass the physical CPU through; TCG must name a concrete
	// ARMv8 model because there is no host CPU to inherit.
	if got := CPUModel(ArchARM64, AccelHVF); got != "host" {
		t.Errorf("CPUModel(hvf) = %q, want host", got)
	}
	if got := CPUModel(ArchARM64, AccelKVM); got != "host" {
		t.Errorf("CPUModel(kvm) = %q, want host", got)
	}
	if got := CPUModel(ArchARM64, AccelTCG); got != "cortex-a57" {
		t.Errorf("CPUModel(tcg) = %q, want cortex-a57", got)
	}

	// HVF has no GICv2 emulation, so it must pin GICv3. KVM must NOT: an ARM64
	// host with a GICv2 aborts on a GICv3 request, where the default matches.
	if got := MachineType(ArchARM64, AccelHVF); !strings.Contains(got, "gic-version=3") {
		t.Errorf("MachineType(hvf) = %q, want gic-version=3", got)
	}
	if got := MachineType(ArchARM64, AccelKVM); got != "virt" {
		t.Errorf("MachineType(kvm) = %q, want plain virt so the host GIC is matched", got)
	}
	if got := MachineType(ArchARM64, AccelTCG); got != "virt" {
		t.Errorf("MachineType(tcg) = %q, want plain virt", got)
	}
}

func TestX86CPUModelAndMachine(t *testing.T) {
	// The image is built for x86-64-v3. Accelerated, the host CPU (checked to
	// be v3) is passed through. Emulated, a named v3 model is used: -cpu max
	// boots but makes Mesa's software renderer abort.
	for accel, want := range map[Accel]string{AccelKVM: "host", AccelHVF: "host", AccelTCG: "Skylake-Client"} {
		if got := CPUModel(ArchAMD64, accel); got != want {
			t.Errorf("CPUModel(amd64, %s) = %q, want %q", accel, got, want)
		}
		if got := MachineType(ArchAMD64, accel); got != "q35" {
			t.Errorf("MachineType(amd64, %s) = %q, want q35", accel, got)
		}
	}
}

func TestFindQEMUNamesTheEmulatorForTheGuest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", root)
	saved := vmLookPath
	t.Cleanup(func() { vmLookPath = saved })
	var asked []string
	vmLookPath = func(name string) (string, error) {
		asked = append(asked, name)
		return "", fs.ErrNotExist
	}
	if _, err := FindQEMU(ArchAMD64, "linux"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing x86-64 emulator: %v", err)
	}
	if len(asked) != 1 || asked[0] != "qemu-system-x86_64" {
		t.Fatalf("looked up %v, want [qemu-system-x86_64]", asked)
	}
	path := filepath.Join(root, "qemu", "qemu-system-x86_64.exe")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := FindQEMU(ArchAMD64, "windows"); err != nil || got != path {
		t.Fatalf("windows fallback = %q, %v; want %q", got, err, path)
	}
}

func TestFirmwareCandidatesUsesBrewPrefixFirstOnDarwin(t *testing.T) {
	got := FirmwareCandidates(ArchARM64, "darwin", "/opt/homebrew")
	if len(got) == 0 {
		t.Fatal("no candidates for darwin")
	}
	if got[0].Code != "/opt/homebrew/share/qemu/edk2-aarch64-code.fd" {
		t.Errorf("first darwin candidate = %q, want the brew-prefix path", got[0].Code)
	}
}

func TestFirmwareCandidatesCoversDebianAndFedoraLayoutsOnLinux(t *testing.T) {
	got := strings.Join(firmwareCodes(FirmwareCandidates(ArchARM64, "linux", "")), "\n")
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

	got, err := FindFirmware(ArchARM64, "linux", "")
	if err != nil {
		t.Fatalf("FindFirmware() error = %v", err)
	}
	if got != (Firmware{Code: want}) {
		t.Errorf("FindFirmware() = %+v, want code %q and no template", got, want)
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

	got, err := FindFirmware(ArchARM64, "linux", "")
	if err != nil {
		t.Fatalf("FindFirmware() error = %v", err)
	}
	if got.Code != padded {
		t.Errorf("FindFirmware() = %q, want the padded %q", got.Code, padded)
	}
}

func TestFindFirmwareRejectsAnUnpaddedOnlyHost(t *testing.T) {
	stubFirmware(t, map[string]int64{"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd": 2 << 20})
	if _, err := FindFirmware(ArchARM64, "linux", ""); !errors.Is(err, ErrFirmwareNotFound) {
		t.Errorf("unpadded-only host should report firmware not found; got %v", err)
	}
}

func TestFindFirmwareErrorNamesTheInstallCommand(t *testing.T) {
	stubFirmware(t, nil)

	_, err := FindFirmware(ArchARM64, "darwin", "/opt/homebrew")
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

// x86FirmwarePairs is the packaged OVMF pairs, checked against Debian 12,
// Ubuntu 24.04, Fedora, Arch and openSUSE Tumbleweed packages.
var x86FirmwarePairs = []struct{ goos, prefix, code, vars string }{
	{"linux", "", "/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	{"linux", "", "/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
	{"linux", "", "/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	{"linux", "", "/usr/share/edk2/x64/OVMF_CODE.4m.fd", "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
	{"linux", "", "/usr/share/qemu/ovmf-x86_64-4m-code.bin", "/usr/share/qemu/ovmf-x86_64-4m-vars.bin"},
	{"linux", "", "/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"},
	{"linux", "", "/usr/share/qemu/edk2-x86_64-code.fd", "/usr/share/qemu/edk2-i386-vars.fd"},
	{"darwin", "/usr/local/Cellar-prefix", "/usr/local/Cellar-prefix/share/qemu/edk2-x86_64-code.fd", "/usr/local/Cellar-prefix/share/qemu/edk2-i386-vars.fd"},
	{"darwin", "", "/opt/homebrew/share/qemu/edk2-x86_64-code.fd", "/opt/homebrew/share/qemu/edk2-i386-vars.fd"},
	{"darwin", "", "/usr/local/share/qemu/edk2-x86_64-code.fd", "/usr/local/share/qemu/edk2-i386-vars.fd"},
}

func TestX86FirmwareFindsEachPackagedPair(t *testing.T) {
	for _, pair := range x86FirmwarePairs {
		stubFirmware(t, map[string]int64{pair.code: 1, pair.vars: 1})
		got, err := FindFirmware(ArchAMD64, pair.goos, pair.prefix)
		if err != nil || got != (Firmware{Code: pair.code, VarsTemplate: pair.vars}) {
			t.Errorf("%s %s: got %+v, %v; want code %q with template %q", pair.goos, pair.code, got, err, pair.code, pair.vars)
		}
	}
}

func TestX86FirmwareNeedsBothFiles(t *testing.T) {
	for _, pair := range x86FirmwarePairs {
		stubFirmware(t, map[string]int64{pair.code: 1})
		if got, err := FindFirmware(ArchAMD64, pair.goos, pair.prefix); !errors.Is(err, ErrFirmwareNotFound) {
			t.Errorf("%s without its variables template: got %+v, %v; want not found", pair.code, got, err)
		}
		stubFirmware(t, map[string]int64{pair.code: 1, pair.vars: 0})
		if got, err := FindFirmware(ArchAMD64, pair.goos, pair.prefix); !errors.Is(err, ErrFirmwareNotFound) {
			t.Errorf("%s with an empty template: got %+v, %v; want not found", pair.code, got, err)
		}
	}
}

func TestX86FirmwareNeverOffersSecureBootBuilds(t *testing.T) {
	// Secure Boot builds refuse WendyOS's unsigned GRUB.
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, c := range FirmwareCandidates(ArchAMD64, goos, "/opt/homebrew") {
			for _, path := range []string{c.Code, c.VarsTemplate} {
				for _, marker := range []string{"secboot", ".ms.", "_MS", "-ms-", "snakeoil", "secure"} {
					if strings.Contains(path, marker) {
						t.Errorf("%s candidate %q looks like a Secure Boot build (%q)", goos, path, marker)
					}
				}
			}
		}
	}
	stubFirmware(t, map[string]int64{"/usr/share/edk2/x64/OVMF_CODE.secboot.4m.fd": 1, "/usr/share/edk2/x64/OVMF_VARS.4m.fd": 1})
	if got, err := FindFirmware(ArchAMD64, "linux", ""); !errors.Is(err, ErrFirmwareNotFound) {
		t.Errorf("a host with only the Secure Boot code image: got %+v, %v; want not found", got, err)
	}
}

func TestX86FirmwareErrorNamesTheGuestAndThePackages(t *testing.T) {
	stubFirmware(t, nil)
	_, err := FindFirmware(ArchAMD64, "linux", "")
	if !errors.Is(err, ErrFirmwareNotFound) {
		t.Fatalf("linux: %v, want ErrFirmwareNotFound", err)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "no x86-64 UEFI firmware found") || !strings.Contains(msg, "apt install ovmf (or dnf install edk2-ovmf)") {
		t.Errorf("linux error = %q", msg)
	}
	_, err = FindFirmware(ArchAMD64, "darwin", "/opt/homebrew")
	if !errors.Is(err, ErrFirmwareNotFound) || !strings.Contains(err.Error(), "brew install qemu") || !strings.HasPrefix(err.Error(), "no x86-64 UEFI firmware found") {
		t.Errorf("darwin error = %v", err)
	}
	if len(FirmwareCandidates(ArchAMD64, "windows", "")) != 0 {
		t.Error("native Windows has x86-64 firmware candidates; the x86-64 guest is not offered there")
	}
}

func TestARM64FirmwareIsUnchanged(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", root)
	saved := vmLookPath
	t.Cleanup(func() { vmLookPath = saved })
	vmLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
	winDir := filepath.Join(root, "qemu")
	for _, tc := range []struct {
		goos, prefix string
		want         []string
	}{
		{"linux", "", []string{
			"/usr/share/AAVMF/AAVMF_CODE.fd",
			"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
			"/usr/share/edk2/aarch64/QEMU_EFI.silent.fd",
			"/usr/share/qemu/edk2-aarch64-code.fd",
		}},
		{"darwin", "/brew", []string{
			"/brew/share/qemu/edk2-aarch64-code.fd",
			"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
			"/usr/local/share/qemu/edk2-aarch64-code.fd",
		}},
		{"windows", "", []string{
			filepath.Join(winDir, "share", "edk2-aarch64-code.fd"),
			filepath.Join(winDir, "edk2-aarch64-code.fd"),
		}},
	} {
		for _, arch := range []Arch{ArchARM64, Arch("")} {
			got := FirmwareCandidates(arch, tc.goos, tc.prefix)
			if strings.Join(firmwareCodes(got), "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("%s ARM64 candidates = %v, want %v", tc.goos, firmwareCodes(got), tc.want)
			}
			for _, c := range got {
				if c.VarsTemplate != "" {
					t.Errorf("%s ARM64 candidate %q has a variables template %q", tc.goos, c.Code, c.VarsTemplate)
				}
			}
		}
	}
	stubFirmware(t, nil)
	if _, err := FindFirmware(ArchARM64, "linux", ""); err == nil || !strings.HasPrefix(err.Error(), "no ARM64 UEFI firmware found; install it with apt install qemu-efi-aarch64 (or dnf install edk2-aarch64) (looked in: ") {
		t.Errorf("ARM64 not-found text changed: %v", err)
	}
}
