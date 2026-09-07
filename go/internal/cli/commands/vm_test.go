package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func vmSubcommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range newVMCmd().Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("wendy vm has no %q subcommand", name)
	return nil
}

func TestVMStartDocumentsTheDiscoveryTradeoffInHelp(t *testing.T) {
	start := vmSubcommand(t, "start")
	f := start.Flags().Lookup("net")
	if f == nil {
		t.Fatal("start has no --net flag")
	}
	// The default must be the tier that needs no privileges, and the help has to
	// say what it costs, because a silently undiscoverable VM looks broken.
	if f.DefValue != "user" {
		t.Errorf("--net default = %q, want user", f.DefValue)
	}
	if !strings.Contains(f.Usage, "discover") {
		t.Errorf("--net help should mention discovery; got %q", f.Usage)
	}
}

func TestVMCreateAcceptsNoImageAndOffersVersionFlags(t *testing.T) {
	// --image is now the escape hatch, not the requirement: with nothing passed,
	// create resolves the published image. That is what makes the feature usable
	// by someone who has never built WendyOS.
	create := vmSubcommand(t, "create")
	if f := create.Flags().Lookup("image"); f == nil || f.DefValue != "" {
		t.Fatal("create should have an optional --image")
	}
	for _, name := range []string{"version", "nightly"} {
		if create.Flags().Lookup(name) == nil {
			t.Errorf("create has no --%s flag", name)
		}
	}
}

func TestVMCreateRejectsALocalImageCombinedWithAVersion(t *testing.T) {
	// A local file is already a specific build; honouring --version alongside it
	// would silently ignore one of the two.
	cmd := newVMCmd()
	cmd.SetArgs([]string{"create", "dev", "--image", "/nonexistent.wic", "--version", "1.2.3"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("create accepted --image with --version")
	}
	if !strings.Contains(err.Error(), "--version") {
		t.Errorf("error should name the conflicting flag; got %q", err)
	}
}

func TestVMInteractiveSubcommandsAcceptAnOptionalName(t *testing.T) {
	for _, name := range []string{"start", "stop", "logs", "rm"} {
		c := vmSubcommand(t, name)
		if err := c.Args(c, nil); err != nil {
			t.Errorf("%q rejected an omitted name: %v", name, err)
		}
		if err := c.Args(c, []string{"dev"}); err != nil {
			t.Errorf("%q rejected a single name: %v", name, err)
		}
		if err := c.Args(c, []string{"one", "two"}); err == nil {
			t.Errorf("%q accepted two names", name)
		}
	}

	create := vmSubcommand(t, "create")
	if err := create.Args(create, nil); err == nil {
		t.Error("create accepted an omitted name; it is not an interactive lifecycle command")
	}
}

func TestResolveVMNameUsesTheExplicitNameWithoutAPicker(t *testing.T) {
	originalPicker := pickVMNameFn
	pickVMNameFn = func(string) (string, error) {
		t.Fatal("explicit VM name unexpectedly opened the picker")
		return "", nil
	}
	t.Cleanup(func() { pickVMNameFn = originalPicker })

	got, err := resolveVMName([]string{"dev"}, "stop")
	if err != nil || got != "dev" {
		t.Fatalf("resolveVMName(explicit) = %q, %v; want dev, nil", got, err)
	}
}

func TestResolveVMNamePicksWhenInteractive(t *testing.T) {
	originalInteractive, originalPicker := isInteractiveTerminalFn, pickVMNameFn
	isInteractiveTerminalFn = func() bool { return true }
	pickVMNameFn = func(action string) (string, error) {
		if action != "stop" {
			t.Errorf("picker action = %q, want stop", action)
		}
		return "dev", nil
	}
	t.Cleanup(func() {
		isInteractiveTerminalFn = originalInteractive
		pickVMNameFn = originalPicker
	})

	got, err := resolveVMName(nil, "stop")
	if err != nil || got != "dev" {
		t.Fatalf("resolveVMName(interactive) = %q, %v; want dev, nil", got, err)
	}
}

func TestResolveVMNameRequiresANameWhenNonInteractive(t *testing.T) {
	originalInteractive := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = originalInteractive })

	_, err := resolveVMName(nil, "stop")
	if err == nil || !strings.Contains(err.Error(), "pass the VM name") {
		t.Fatalf("resolveVMName(non-interactive) error = %v, want argument guidance", err)
	}
}

func TestResolveVMNamePropagatesPickerCancellation(t *testing.T) {
	originalInteractive, originalPicker := isInteractiveTerminalFn, pickVMNameFn
	isInteractiveTerminalFn = func() bool { return true }
	pickVMNameFn = func(string) (string, error) { return "", ErrUserCancelled }
	t.Cleanup(func() {
		isInteractiveTerminalFn = originalInteractive
		pickVMNameFn = originalPicker
	})

	_, err := resolveVMName(nil, "remove")
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("resolveVMName(cancelled) = %v, want ErrUserCancelled", err)
	}
}

func TestPickVMNameExplainsAnEmptyStore(t *testing.T) {
	stubVMStatuses(t)

	_, err := pickVMName("stop")
	if err == nil || !strings.Contains(err.Error(), "wendy vm create") {
		t.Fatalf("pickVMName(empty) error = %v, want create guidance", err)
	}
}

func TestVMStartHasAPortFlagWithAWorkingDefault(t *testing.T) {
	// The default is what makes `wendy vm start dev` work with no arguments;
	// the flag exists only for the machine that already has 50051 taken.
	start := vmSubcommand(t, "start")
	f := start.Flags().Lookup("port")
	if f == nil {
		t.Fatal("start has no --port flag; a host with 50051 taken cannot start a VM")
	}
	if f.DefValue != "50051" {
		t.Errorf("--port default = %q, want 50051 so the flag is optional", f.DefValue)
	}
	if !strings.Contains(f.Usage, "50051") {
		t.Errorf("--port help should name the guest port it maps to; got %q", f.Usage)
	}
}

func TestVMCreateHasAPRFlag(t *testing.T) {
	// The VM is the one target a reviewer can run without hardware, which is why
	// it builds on every PR. Without this flag that build is unreachable.
	create := vmSubcommand(t, "create")
	f := create.Flags().Lookup("pr")
	if f == nil {
		t.Fatal("create has no --pr flag; a PR's VM image cannot be run")
	}
	if f.DefValue != "0" {
		t.Errorf("--pr default = %q, want 0 (unset)", f.DefValue)
	}
}

func TestVMCreateRejectsPRCombinedWithAChannel(t *testing.T) {
	// A PR publishes exactly one build, so a version or channel alongside it is
	// a contradiction rather than a refinement.
	for _, extra := range [][]string{
		{"--version", "1.2.3"},
		{"--nightly"},
		{"--image", "/nonexistent.wic"},
	} {
		args := append([]string{"create", "dev", "--pr", "246"}, extra...)
		cmd := newVMCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		err := cmd.Execute()
		if err == nil {
			t.Errorf("create accepted --pr with %v", extra)
			continue
		}
		if !strings.Contains(err.Error(), "--pr") {
			t.Errorf("error for --pr with %v should name --pr; got %q", extra, err)
		}
	}
}

func TestVMCommandHasBackgroundLifecycleSubcommands(t *testing.T) {
	want := map[string]bool{"create": false, "start": false, "stop": false, "logs": false, "list": false, "rm": false}
	for _, sub := range newVMCmd().Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("wendy vm is missing the %q subcommand", name)
		}
	}
}

func TestVMStartOffersDetach(t *testing.T) {
	for _, sub := range newVMCmd().Commands() {
		if sub.Name() != "start" {
			continue
		}
		if sub.Flags().Lookup("detach") == nil {
			t.Error("wendy vm start has no --detach flag")
		}
		if sub.Flags().ShorthandLookup("d") == nil {
			t.Error("wendy vm start has no -d shorthand for --detach")
		}
		return
	}
	t.Fatal("no start subcommand")
}

func TestVMStateLabelDistinguishesStartingFromRunning(t *testing.T) {
	// The run lock is taken before the emulator has a pid; that window must
	// read as "starting", not as a stale record.
	cases := []struct {
		name string
		st   vm.Status
		want string
	}{
		{"stopped", vm.Status{Exists: true}, "stopped"},
		{"starting", vm.Status{Exists: true, Running: true}, "starting"},
		{"running", vm.Status{Exists: true, Running: true, State: vm.State{PID: 42}}, "running"},
	}
	for _, tc := range cases {
		if got := vmStateLabel(tc.st); got != tc.want {
			t.Errorf("vmStateLabel(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestVMAddressOnlyForARunningForwardedVM(t *testing.T) {
	// Empty, not a display dash: callers treat the result as a dial target.
	if got := vmAddress(vm.Status{Exists: true}); got != "" {
		t.Errorf("vmAddress(stopped) = %q, want empty", got)
	}
	// Shared networking forwards no port, so there is no loopback address.
	if got := vmAddress(vm.Status{Running: true, State: vm.State{PID: 1}}); got != "" {
		t.Errorf("vmAddress(shared) = %q, want empty", got)
	}
	running := vm.Status{Running: true, State: vm.State{PID: 1, AgentPort: 50053}}
	if got := vmAddress(running); got != "127.0.0.1:50053" {
		t.Errorf("vmAddress(running) = %q, want 127.0.0.1:50053", got)
	}
}

func TestVMImageSourceNamesTheChannel(t *testing.T) {
	cases := []struct {
		image   string
		nightly bool
		pr      int
		want    string
	}{
		{"", false, 0, "release"},
		{"", true, 0, "nightly"},
		{"", false, 1834, "pr/1834"},
		{"/tmp/x.wic", false, 0, "local"},
	}
	for _, tc := range cases {
		if got := vmImageSource(tc.image, tc.nightly, tc.pr); got != tc.want {
			t.Errorf("vmImageSource(%q, %v, %d) = %q, want %q", tc.image, tc.nightly, tc.pr, got, tc.want)
		}
	}
}

func TestSharedNetworkingRecordsNoAgentPort(t *testing.T) {
	// Shared mode forwards nothing, so a recorded port would be a loopback
	// address nothing listens on -- handed out by vm list, the simulator tab
	// and ensureSimulatorRunning alike.
	if !strings.Contains(newVMCmd().Long, "QEMU") {
		t.Skip("vm command shape changed")
	}
	st := vm.Status{Running: true, State: vm.State{PID: 1, NetMode: vm.NetShared}}
	if got := vmAddress(st); got != "" {
		t.Errorf("vmAddress(shared) = %q, want empty", got)
	}
}

func TestConsoleTailStripsEightBitControlIntroducers(t *testing.T) {
	// \u009b and \u009d are the 8-bit CSI and OSC introducers. Stripping only
	// the 7-bit ESC form leaves the same escape route open from guest output,
	// which `wendy vm logs` and a failed boot both print to the terminal.
	got := sanitizeConsoleLine("a\u009b2Jb\u009d0;xc\x1b[2Jd")
	for _, bad := range []string{"\u009b", "\u009d", "\x1b"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeConsoleLine() = %q, still contains %q", got, bad)
		}
	}
	for _, keep := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(got, keep) {
			t.Errorf("sanitizeConsoleLine() = %q, dropped the printable %q", got, keep)
		}
	}
	// Multi-byte text must survive: the guest prints UTF-8 and box drawing.
	if got := sanitizeConsoleLine("na\u00efve \u2713 caf\u00e9"); got != "na\u00efve \u2713 caf\u00e9" {
		t.Errorf("sanitizeConsoleLine() = %q, want multi-byte text kept intact", got)
	}
}
