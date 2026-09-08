package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// stubVMStatuses describes the local VM store for the discovery probe.
func stubVMStatuses(t *testing.T, statuses ...vm.Status) {
	t.Helper()
	saved := vmStatusesFn
	vmStatusesFn = func() ([]vm.Status, error) { return statuses, nil }
	t.Cleanup(func() { vmStatusesFn = saved })
}

func runningVM(name string, mode vm.NetMode, port int) vm.Status {
	return vm.Status{
		Name:    name,
		Exists:  true,
		Running: true,
		State:   vm.State{Name: name, PID: 1234, AgentPort: port, NetMode: mode},
	}
}

func TestProbeSkipsStoppedVMs(t *testing.T) {
	stubVMStatuses(t, vm.Status{Name: "stopped", Exists: true})

	if got := probeLocalVMDevices(context.Background()); len(got) != 0 {
		t.Errorf("probeLocalVMDevices() = %+v, want none for a stopped VM", got)
	}
}

func TestProbeSkipsSharedModeVMs(t *testing.T) {
	// A shared-mode VM is on a real segment where mDNS already finds it;
	// synthesising a row would list it twice.
	stubVMStatuses(t, runningVM("shared", vm.NetShared, 0))

	if got := probeLocalVMDevices(context.Background()); len(got) != 0 {
		t.Errorf("probeLocalVMDevices() = %+v, want none for a shared-mode VM", got)
	}
}

func TestProbeSkipsAVMWhoseAgentDoesNotAnswer(t *testing.T) {
	// Nothing is listening on this port, so the pre-dial gate rejects it.
	stubVMStatuses(t, runningVM("booting", vm.NetUser, 59999))

	if got := probeLocalVMDevices(context.Background()); len(got) != 0 {
		t.Errorf("probeLocalVMDevices() = %+v, want none while the agent is down", got)
	}
}

func TestVMRowLeavesHostnameEmptySoTwoVMsStayDistinct(t *testing.T) {
	// Every VM from the same image reports the same guest hostname, and the
	// dedup key prefers hostname over display name -- so a populated Hostname
	// would collapse two VMs into one row.
	// Empty host key (what the probe sets), distinct display names.
	one := deviceDedupKey("", "one")
	two := deviceDedupKey("", "two")
	if one == two {
		t.Errorf("two VMs share the dedup key %q", one)
	}
	// And the failure mode being guarded against: a shared guest hostname.
	if deviceDedupKey("wendyos.local", "one") != deviceDedupKey("wendyos.local", "two") {
		t.Error("expected a shared hostname to collapse rows; the guard is why Hostname stays empty")
	}
}

func TestProbeSurvivesAStoreItCannotRead(t *testing.T) {
	saved := vmStatusesFn
	vmStatusesFn = func() ([]vm.Status, error) { return nil, errors.New("permission denied") }
	t.Cleanup(func() { vmStatusesFn = saved })

	if got := probeLocalVMDevices(context.Background()); got != nil {
		t.Errorf("probeLocalVMDevices() = %+v, want nil when the store is unreadable", got)
	}
}

func TestVMBoardsHaveHumanReadableDeviceTypes(t *testing.T) {
	// Without an entry the discover table shows the raw board id from
	// /etc/wendyos/device-type.
	for _, board := range []string{"vm-arm64", "vm-x86-64"} {
		if got := humanReadableDeviceType(board); got == board {
			t.Errorf("%s has no display name in deviceTypeNames", board)
		}
	}
}

func TestVMDeviceKeyMatchesThePublishedBoardID(t *testing.T) {
	// The key is the image's WENDYOS_BOARD_ID, not a name chosen here: it is
	// what the Builder publishes under and what the guest reports.
	if vmDeviceKey != "vm-arm64" {
		t.Errorf("vmDeviceKey = %q, want the published board id vm-arm64", vmDeviceKey)
	}
	if got := humanReadableDeviceType(vmDeviceKey); got == vmDeviceKey {
		t.Errorf("the VM board %q the CLI downloads has no display name", vmDeviceKey)
	}
}
