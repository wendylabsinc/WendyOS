package vm

import (
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{
		Name:         "dev",
		DiskPath:     "/vms/dev/disk.img",
		FirmwareCode: "/fw/code.fd",
		FirmwareVars: "/vms/dev/efivars.fd",
		MemoryMiB:    4096,
		CPUs:         4,
		Accel:        AccelHVF,
		Net:          NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("dev")},
	}
}

func TestBinaryIsTheAArch64SystemEmulator(t *testing.T) {
	if got := testSpec().Binary(); got != "qemu-system-aarch64" {
		t.Errorf("Binary() = %q, want qemu-system-aarch64", got)
	}
}

func TestArgsCarryTheAcceleratedMachineAndCPU(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-machine virt,gic-version=3",
		"-accel hvf",
		"-cpu host",
		"-smp 4",
		"-m 4096",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %q", want, joined)
		}
	}
}

func TestArgsMountFirmwareReadOnlyAndVarsWritable(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// The code image is shared with every other VM on the host; only the
	// variable store is per-VM and writable.
	if !strings.Contains(joined, "if=pflash,format=raw,readonly=on,file=/fw/code.fd") {
		t.Errorf("firmware code should be read-only pflash; got %q", joined)
	}
	if !strings.Contains(joined, "if=pflash,format=raw,file=/vms/dev/efivars.fd") {
		t.Errorf("variable store should be writable pflash; got %q", joined)
	}
	if strings.Contains(joined, "-kernel") {
		t.Errorf("the image is self-booting; args must not pass an external kernel: %q", joined)
	}
}

func TestArgsOrderFirmwareCodeBeforeVars(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatal(err)
	}
	// QEMU maps pflash units in the order given and edk2 expects code first;
	// swapping them boots to a blank firmware with no error message.
	joined := strings.Join(args, " ")
	code := strings.Index(joined, "readonly=on,file=/fw/code.fd")
	vars := strings.Index(joined, "file=/vms/dev/efivars.fd")
	if code < 0 || vars < 0 {
		t.Fatalf("both pflash drives should be present; got %q", joined)
	}
	if code > vars {
		t.Errorf("firmware code must precede the variable store; got %q", joined)
	}
}

func TestArgsAttachTheDiskAsVirtioBlock(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "file=/vms/dev/disk.img") {
		t.Errorf("args missing the disk; got %q", joined)
	}
	if !strings.Contains(joined, "virtio-blk-pci") {
		t.Errorf("disk should be virtio-blk-pci; got %q", joined)
	}
}

func TestArgsPutTheConsoleOnStdio(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-nographic") {
		t.Errorf("args should run headless; got %q", joined)
	}
	// mon:stdio keeps the QEMU monitor escape (Ctrl-A x) available, which is
	// the only way out of a headless guest.
	if !strings.Contains(joined, "-serial mon:stdio") {
		t.Errorf("args should multiplex the monitor onto stdio; got %q", joined)
	}
}

func TestArgsExposeQMPOnlyOnUnixSocket(t *testing.T) {
	for _, log := range []string{"", "/vms/dev/console.log"} {
		s := testSpec()
		s.ConsoleLog = log
		s.QMPPath = "/vms/with,comma/dev/qmp.sock"
		args, err := s.Args()
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-qmp unix:/vms/with,,comma/dev/qmp.sock,server=on,wait=off") {
			t.Fatalf("missing Unix control socket: %s", joined)
		}
		if strings.Contains(joined, "-qmp tcp:") {
			t.Fatalf("unexpected TCP control listener: %s", joined)
		}
	}
}

func TestArgsUseTCGSettingsOnAnUnacceleratedHost(t *testing.T) {
	s := testSpec()
	s.Accel = AccelTCG
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-accel tcg") || !strings.Contains(joined, "-cpu cortex-a57") {
		t.Errorf("TCG spec should emit tcg + cortex-a57; got %q", joined)
	}
	if strings.Contains(joined, "gic-version=3") {
		t.Errorf("TCG should use the default GIC; got %q", joined)
	}
}

func TestArgsApplyDefaultsForMemoryAndCPUs(t *testing.T) {
	s := testSpec()
	s.MemoryMiB, s.CPUs = 0, 0
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-m 4096") || !strings.Contains(joined, "-smp 4") {
		t.Errorf("zero values should fall back to the machine conf defaults; got %q", joined)
	}
}

func TestArgsRejectAnIncompleteSpec(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"no disk":     func(s *Spec) { s.DiskPath = "" },
		"no firmware": func(s *Spec) { s.FirmwareCode = "" },
		"no varstore": func(s *Spec) { s.FirmwareVars = "" },
	} {
		s := testSpec()
		mutate(&s)
		if _, err := s.Args(); err == nil {
			t.Errorf("%s: Args() succeeded, want an error", name)
		}
	}
}

func TestArgsPropagateANetworkError(t *testing.T) {
	s := testSpec()
	// Shared mode with no socket path: the error has to reach the caller rather
	// than degrade to a network the user did not ask for.
	s.Net = NetConfig{Mode: NetShared, MAC: MACFor("dev")}
	if _, err := s.Args(); err == nil {
		t.Fatal("Args() should surface the network configuration error")
	}
}

func TestArgsNameTheGuest(t *testing.T) {
	// Without a name the only way to find one VM among several is to match the
	// emulator binary, which cannot tell them apart.
	args, err := testSpec().Args()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "-name dev") {
		t.Errorf("args should name the guest; got %v", args)
	}
	s := testSpec()
	s.Name = ""
	if _, err := s.Args(); err == nil {
		t.Error("Args() accepted a spec with no name")
	}
}

func TestForegroundConsoleKeepsTheTerminal(t *testing.T) {
	args, err := testSpec().Args()
	if err != nil {
		t.Fatalf("Args() = %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-nographic", "-serial mon:stdio"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Args() = %q, want it to contain %q", joined, want)
		}
	}
}

func TestDetachedConsoleGoesToTheLogAndNotStdio(t *testing.T) {
	s := testSpec()
	s.ConsoleLog = "/tmp/vm/console.log"
	args, err := s.Args()
	if err != nil {
		t.Fatalf("Args() = %v", err)
	}
	joined := strings.Join(args, " ")
	// append=on matters: the caller also points QEMU's stdout and stderr at
	// this file, and a truncating chardev would clobber them.
	for _, want := range []string{"-display none", "-chardev file,id=console,append=on,path=/tmp/vm/console.log", "-serial chardev:console", "-monitor none"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Args() = %q, want it to contain %q", joined, want)
		}
	}
	// -nographic would re-point the monitor at a closed stdio and kill the VM.
	for _, unwanted := range []string{"-nographic", "mon:stdio"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("Args() = %q, want it NOT to contain %q", joined, unwanted)
		}
	}
}

func TestDetachedConsolePathEscapesCommas(t *testing.T) {
	s := testSpec()
	s.ConsoleLog = "/home/a,b/vms/sim/console.log"
	args, err := s.Args()
	if err != nil {
		t.Fatalf("Args() = %v", err)
	}
	want := "path=/home/a,,b/vms/sim/console.log"
	if joined := strings.Join(args, " "); !strings.Contains(joined, want) {
		t.Errorf("Args() = %q, want it to contain %q", joined, want)
	}
}

func TestEveryPathArgumentEscapesCommas(t *testing.T) {
	// QEMU splits option strings on commas, so an unescaped one in any path
	// silently truncates it and the rest parses as further options.
	s := Spec{
		Name:         "dev",
		DiskPath:     "/home/a,b/disk.img",
		FirmwareCode: "/opt/a,b/code.fd",
		FirmwareVars: "/home/a,b/efivars.fd",
		Net:          NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("dev")},
	}
	args, err := s.Args()
	if err != nil {
		t.Fatalf("Args() = %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"/home/a,,b/disk.img", "/opt/a,,b/code.fd", "/home/a,,b/efivars.fd"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Args() = %q, want it to contain the escaped %q", joined, want)
		}
	}
}
