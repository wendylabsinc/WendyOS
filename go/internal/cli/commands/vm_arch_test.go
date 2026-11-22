package commands

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// archTestVM creates a stopped VM directory whose meta.json carries arch.
func archTestVM(t *testing.T, name string, arch vm.Arch) {
	t.Helper()
	setTempConfig(t, &config.Config{})
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.DiskPath(name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMeta(vm.Meta{Name: name, Profile: "generic", Arch: arch}); err != nil {
		t.Fatal(err)
	}
}

// stubVMStartHost records the architecture each start dependency is asked
// for and answers them without touching the host.
func stubVMStartHost(t *testing.T, emulated error) (qemuArchs, firmwareArchs *[]vm.Arch) {
	t.Helper()
	savedQEMU, savedFirmware, savedCheck := ensureQEMUFn, findQEMUFirmwareFn, checkEmulatedGuestFn
	t.Cleanup(func() { ensureQEMUFn, findQEMUFirmwareFn, checkEmulatedGuestFn = savedQEMU, savedFirmware, savedCheck })
	var q, f []vm.Arch
	ensureQEMUFn = func(_ context.Context, arch vm.Arch) error { q = append(q, arch); return nil }
	findQEMUFirmwareFn = func(arch vm.Arch, _, _ string) (vm.Firmware, error) {
		f = append(f, arch)
		if arch == vm.ArchAMD64 {
			return vm.Firmware{Code: "/fw/code", VarsTemplate: "/fw/vars"}, nil
		}
		return vm.Firmware{Code: "/fw/code"}, nil
	}
	checkEmulatedGuestFn = func(vm.Arch, vm.Accel, string) error { return emulated }
	return &q, &f
}

func archTestStartOptions(t *testing.T) vmStartOptions {
	t.Helper()
	port, err := vm.PickHostPort(vm.DefaultAgentPort, simulatorPortSearchTries)
	if err != nil {
		t.Fatal(err)
	}
	return vmStartOptions{netMode: string(vm.NetUser), hostPort: port, memoryMiB: 1024, cpus: 1}
}

func TestResolveVMSpecFollowsTheRecordedArchitecture(t *testing.T) {
	archTestVM(t, "x86", vm.ArchAMD64)
	qemu, firmware := stubVMStartHost(t, nil)
	spec, _, err := resolveVMSpec("x86", archTestStartOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Arch != vm.ArchAMD64 || spec.FirmwareCode != "/fw/code" || spec.FirmwareVarsTemplate != "/fw/vars" ||
		spec.Accel != vm.AccelFor(vm.ArchAMD64, runtime.GOOS, runtime.GOARCH) {
		t.Errorf("x86-64 spec = arch %q, code %q, template %q, accel %q", spec.Arch, spec.FirmwareCode, spec.FirmwareVarsTemplate, spec.Accel)
	}
	if len(*qemu) != 1 || (*qemu)[0] != vm.ArchAMD64 || len(*firmware) != 1 || (*firmware)[0] != vm.ArchAMD64 {
		t.Errorf("emulator asked for %v, firmware for %v; want amd64 for both", *qemu, *firmware)
	}

	archTestVM(t, "old", "")
	qemu, firmware = stubVMStartHost(t, nil)
	spec, _, err = resolveVMSpec("old", archTestStartOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Arch != vm.ArchARM64 || spec.FirmwareVarsTemplate != "" || (*qemu)[0] != vm.ArchARM64 || (*firmware)[0] != vm.ArchARM64 {
		t.Errorf("VM without arch: spec arch %q template %q, asked %v and %v; want ARM64 throughout", spec.Arch, spec.FirmwareVarsTemplate, *qemu, *firmware)
	}
}

func TestResolveVMSpecRefusesAnEmulatedGuestOnOldQEMU(t *testing.T) {
	archTestVM(t, "x86", vm.ArchAMD64)
	stubVMStartHost(t, vm.ErrQEMUTooOld)
	if _, _, err := resolveVMSpec("x86", archTestStartOptions(t)); !errors.Is(err, vm.ErrQEMUTooOld) {
		t.Fatalf("resolveVMSpec() = %v, want ErrQEMUTooOld", err)
	}
}

func TestResolveVMSpecRejectsAnUnknownRecordedArchitecture(t *testing.T) {
	archTestVM(t, "odd", "riscv64")
	qemu, _ := stubVMStartHost(t, nil)
	_, _, err := resolveVMSpec("odd", archTestStartOptions(t))
	if err == nil || !strings.Contains(err.Error(), `"odd"`) || !strings.Contains(err.Error(), "riscv64") {
		t.Fatalf("resolveVMSpec() = %v, want an error naming the VM and riscv64", err)
	}
	if len(*qemu) != 0 {
		t.Errorf("an unknown architecture still asked for an emulator: %v", *qemu)
	}
}

func TestSimulatorBootTimeoutFollowsTheVMsAcceleration(t *testing.T) {
	saved := vmStatusFn
	t.Cleanup(func() { vmStatusFn = saved })
	for accel, want := range map[vm.Accel]time.Duration{vm.AccelTCG: 5 * time.Minute, vm.AccelKVM: 90 * time.Second, vm.AccelHVF: 90 * time.Second} {
		vmStatusFn = func(string) (vm.Status, error) {
			return vm.Status{Exists: true, Running: true, State: vm.State{PID: 1, Accel: accel}}, nil
		}
		if got := simulatorBootTimeout("dev"); got != want {
			t.Errorf("running under %s: %v, want %v", accel, got, want)
		}
	}
	for _, arch := range []vm.Arch{vm.ArchAMD64, vm.ArchARM64} {
		vmStatusFn = func(string) (vm.Status, error) { return vm.Status{Exists: true, Meta: vm.Meta{Arch: arch}}, nil }
		want := 90 * time.Second
		if vm.AccelFor(arch, runtime.GOOS, runtime.GOARCH) == vm.AccelTCG {
			want = 5 * time.Minute
		}
		if got := simulatorBootTimeout("dev"); got != want {
			t.Errorf("stopped %s VM: %v, want %v", arch, got, want)
		}
	}
}

func TestVMListShowsTheArchitecture(t *testing.T) {
	statuses := []vm.Status{
		{Name: "x86", Exists: true, Meta: vm.Meta{Arch: vm.ArchAMD64}},
		{Name: "old", Exists: true},
		{Name: "lite", Exists: true, Meta: vm.Meta{Profile: "wendy-lite"}},
	}
	got := vmListJSON(statuses)
	for i, want := range []string{"amd64", "arm64", ""} {
		if got[i].Arch != want {
			t.Errorf("%s: arch %q, want %q", got[i].Name, got[i].Arch, want)
		}
	}
	var table strings.Builder
	if err := writeVMListTable(&table, statuses); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	if len(lines) != 4 || strings.Join(strings.Fields(lines[0]), " ") != "NAME STATE ARCH ADDRESS VERSION" {
		t.Fatalf("table = %q", table.String())
	}
	for i, want := range []string{"x86 stopped amd64 - -", "old stopped arm64 - -", "lite stopped - - -"} {
		if row := strings.Join(strings.Fields(lines[i+1]), " "); row != want {
			t.Errorf("row %d = %q, want %q", i+1, row, want)
		}
	}
}

func TestVMStartLineNamesTheGuest(t *testing.T) {
	x86 := vm.Spec{Arch: vm.ArchAMD64, Accel: vm.AccelKVM, Net: vm.NetConfig{Mode: vm.NetUser}}
	if got, want := vmStartLine("dev", x86), "Starting dev (x86-64 guest, kvm acceleration, user networking)."; got != want {
		t.Errorf("x86-64 start line = %q, want %q", got, want)
	}
	arm := vm.Spec{Accel: vm.AccelTCG, Net: vm.NetConfig{Mode: vm.NetUser}}
	if got, want := vmStartLine("dev", arm), "Starting dev (ARM64 guest, tcg acceleration, user networking)."; got != want {
		t.Errorf("ARM64 start line = %q, want %q", got, want)
	}
}
