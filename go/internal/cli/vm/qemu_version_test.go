package vm

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// stubQEMUVersion makes the x86-64 emulator answer --version with banner, or
// fail when banner is empty. It records every command it is asked to run.
func stubQEMUVersion(t *testing.T, banner string) *[][]string {
	t.Helper()
	savedLook, savedOut := vmLookPath, vmCommandOutput
	t.Cleanup(func() { vmLookPath, vmCommandOutput = savedLook, savedOut })
	vmLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	var calls [][]string
	vmCommandOutput = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if banner == "" {
			return nil, errors.New("exit status 1")
		}
		return []byte(banner), nil
	}
	return &calls
}

func TestQEMUVersionParsesDistributionBanners(t *testing.T) {
	for banner, want := range map[string][2]int{
		"QEMU emulator version 7.2.15 (Debian 1:7.2+dfsg-7+deb12u13)\nCopyright (c) 2003-2022 Fabrice Bellard and the QEMU Project developers\n": {7, 2},
		"QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1.4)\n":                                                                           {8, 2},
		"QEMU emulator version 6.2.0 (Debian 1:6.2+dfsg-2ubuntu6.24)\n":                                                                          {6, 2},
		"QEMU emulator version 11.1.3\n": {11, 1},
	} {
		calls := stubQEMUVersion(t, banner)
		major, minor, err := QEMUVersion(ArchAMD64, "linux")
		if err != nil || major != want[0] || minor != want[1] {
			t.Errorf("banner %q: got %d.%d, %v; want %d.%d", banner, major, minor, err, want[0], want[1])
		}
		if len(*calls) != 1 || filepath.Base((*calls)[0][0]) != "qemu-system-x86_64" || strings.Join((*calls)[0][1:], " ") != "--version" {
			t.Errorf("ran %v, want qemu-system-x86_64 --version", *calls)
		}
	}
	for _, banner := range []string{"not qemu", ""} {
		stubQEMUVersion(t, banner)
		if major, minor, err := QEMUVersion(ArchAMD64, "linux"); err == nil {
			t.Errorf("banner %q: got %d.%d and no error", banner, major, minor)
		}
	}
}

func TestCheckEmulatedGuest(t *testing.T) {
	stubQEMUVersion(t, "QEMU emulator version 7.1.0\n")
	err := CheckEmulatedGuest(ArchAMD64, AccelTCG, "linux")
	if !errors.Is(err, ErrQEMUTooOld) {
		t.Fatalf("QEMU 7.1 emulating x86-64: %v, want ErrQEMUTooOld", err)
	}
	for _, want := range []string{"7.1", "7.2", "KVM", "--arch arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("linux error %q does not contain %q", err, want)
		}
	}
	err = CheckEmulatedGuest(ArchAMD64, AccelTCG, "darwin")
	if !errors.Is(err, ErrQEMUTooOld) || strings.Contains(err.Error(), "KVM") {
		t.Errorf("darwin error = %v, want ErrQEMUTooOld without a KVM clause", err)
	}

	for _, banner := range []string{"QEMU emulator version 7.2.0\n", "QEMU emulator version 11.1.3\n", ""} {
		stubQEMUVersion(t, banner)
		if err := CheckEmulatedGuest(ArchAMD64, AccelTCG, "linux"); err != nil {
			t.Errorf("banner %q: %v, want nil", banner, err)
		}
	}

	for _, tc := range []struct {
		arch  Arch
		accel Accel
	}{{ArchAMD64, AccelKVM}, {ArchARM64, AccelTCG}, {Arch(""), AccelTCG}} {
		calls := stubQEMUVersion(t, "QEMU emulator version 6.2.0\n")
		if err := CheckEmulatedGuest(tc.arch, tc.accel, "linux"); err != nil || len(*calls) != 0 {
			t.Errorf("%s/%s: err=%v calls=%v, want nil and no emulator run", tc.arch, tc.accel, err, *calls)
		}
	}
}
