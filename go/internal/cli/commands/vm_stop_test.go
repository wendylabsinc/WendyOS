package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// slowestOrdinaryGuestShutdown is how long systemd may take to power a guest
// off without anything being wrong: 90 s for a service that does not stop, 90 s
// more for processes left at the final kill, then unmounting and flushing. A
// guest running an agent without the prompt host-shutdown path took about 3.5
// minutes with apps running.
const slowestOrdinaryGuestShutdown = 90*time.Second + 90*time.Second + 30*time.Second

type recordedStop struct {
	name  string
	force bool
	grace time.Duration
}

// stubVMStop makes name a running VM and records the stop requests a test
// makes, without an emulator.
func stubVMStop(t *testing.T, name string, result error) *[]recordedStop {
	t.Helper()
	savedStatus, savedStop := vmStatusFn, stopVMFn
	t.Cleanup(func() { vmStatusFn, stopVMFn = savedStatus, savedStop })
	vmStatusFn = func(string) (vm.Status, error) {
		return vm.Status{Name: name, Exists: true, Running: true, State: vm.State{Name: name, PID: 4242}}, nil
	}
	var stops []recordedStop
	stopVMFn = func(_ context.Context, name string, force bool, grace time.Duration) error {
		stops = append(stops, recordedStop{name: name, force: force, grace: grace})
		return result
	}
	return &stops
}

func runVMStopCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := vmSubcommand(t, "stop")
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	// Run the subcommand itself: Execute would hand over to the root, which
	// reads the test binary's own arguments.
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	err = cmd.RunE(cmd, cmd.Flags().Args())
	return out.String(), errOut.String(), err
}

func TestVMStopWaitsOutAnOrdinaryGuestShutdown(t *testing.T) {
	stops := stubVMStop(t, "dev", nil)

	stdout, stderr, err := runVMStopCommand(t, "dev")
	if err != nil {
		t.Fatalf("wendy vm stop dev: %v", err)
	}
	if len(*stops) != 1 {
		t.Fatalf("stop requests = %+v, want one", *stops)
	}
	got := (*stops)[0]
	if got.force {
		t.Error("a plain stop cut the power")
	}
	if got.grace < slowestOrdinaryGuestShutdown {
		t.Errorf("stop waits %v, shorter than an ordinary guest shutdown (%v): it would report one as a failure",
			got.grace, slowestOrdinaryGuestShutdown)
	}
	if !strings.Contains(stderr, `Shutting down VM "dev"; waiting for it to power off`) {
		t.Errorf("stderr = %q, want it to say what the command is waiting for", stderr)
	}
	if stdout != "Stopped VM \"dev\".\n" {
		t.Errorf("stdout = %q, want only the result", stdout)
	}
}

func TestVMStopForceStillCutsPowerWithoutWaiting(t *testing.T) {
	stops := stubVMStop(t, "dev", nil)

	_, stderr, err := runVMStopCommand(t, "dev", "--force")
	if err != nil {
		t.Fatalf("wendy vm stop dev --force: %v", err)
	}
	if len(*stops) != 1 || !(*stops)[0].force {
		t.Fatalf("stop requests = %+v, want one forced stop", *stops)
	}
	if strings.Contains(stderr, "waiting") {
		t.Errorf("stderr = %q; a forced stop has no shutdown to wait for", stderr)
	}
}

func TestSimulatorTabStopWaitsAsLongAsVMStop(t *testing.T) {
	stops := stubVMStop(t, "dev", nil)

	flash, isErr := stopSimulatorRow(context.Background(), tui.PickerItem{Name: "dev", Value: &simulatorChoice{Name: "dev"}})
	if isErr {
		t.Errorf("an ordinary shutdown was reported as an error: %q", flash)
	}
	if flash != "Stopped dev." {
		t.Errorf("flash = %q, want %q", flash, "Stopped dev.")
	}
	if len(*stops) != 1 {
		t.Fatalf("stop requests = %+v, want one", *stops)
	}
	if got := (*stops)[0]; got.force || got.grace != vmStopGrace {
		t.Errorf("stop request = %+v, want the same graceful wait as 'wendy vm stop' (%v)", got, vmStopGrace)
	}
}
