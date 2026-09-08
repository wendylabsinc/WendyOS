package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// errSimulatorUnavailable marks a failure to bring up the selected simulator.
// resolveWithCloudFallback must not answer it with a cloud tunnel: the user
// named a local VM, and quietly deploying to a cloud device instead would land
// the app on an entirely different machine.
var errSimulatorUnavailable = errors.New("simulator unavailable")

// markSimulatorUnavailable tags err so resolveWithCloudFallback will not answer
// it by deploying to a cloud device -- without stacking a second copy of the
// prefix onto an error that already carries one.
func markSimulatorUnavailable(err error) error {
	if errors.Is(err, errSimulatorUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", errSimulatorUnavailable, err)
}

// simulatorPortSearchTries bounds the scan for a free forward. Eight spans is
// enough to step past a busy development machine without hunting forever.
const simulatorPortSearchTries = 8

// simulatorBootTimeout scales the wait with the host's acceleration: an ARM64
// host runs the guest natively, while anything else emulates every instruction
// and a cold WendyOS boot takes minutes rather than seconds.
func simulatorBootTimeout() time.Duration {
	if vm.AccelFor(runtime.GOOS, runtime.GOARCH) == vm.AccelTCG {
		return 5 * time.Minute
	}
	return 90 * time.Second
}

// ensureSimulatorRunning brings name to a running state and returns the address
// its agent is forwarded to, plus whether this call is what started it.
//
// Starting a VM only gets QEMU running; the guest still has to boot, so a
// caller must wait for the agent before dialling.
func ensureSimulatorRunning(ctx context.Context, name string) (addr string, started bool, err error) {
	store, err := vm.NewStore()
	if err != nil {
		return "", false, err
	}
	st, err := store.Status(name)
	if err != nil {
		return "", false, err
	}
	if st.Running {
		if st.State.AgentPort != 0 {
			return fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort), false, nil
		}
		// Running with no forwarded port: either shared networking, which has no
		// loopback address at all, or a run that has not recorded one yet.
		// Restarting it would only fail on the lock it already holds.
		if st.State.NetMode == vm.NetShared {
			return "", false, fmt.Errorf("%w: %s uses shared networking, so it has no loopback address; "+
				"find it with 'wendy discover'", errSimulatorUnavailable, name)
		}
		return "", false, fmt.Errorf("VM %q is starting; retry in a moment", name)
	}

	if !st.Exists {
		// Ahead of the port scan below, which costs a bind and a close per
		// candidate and would otherwise run for a VM that was never created.
		return "", false, fmt.Errorf("%w: no VM named %q; create it with 'wendy vm create %s'",
			errSimulatorUnavailable, name, name)
	}

	// resolveVMSpec re-checks the winner, so this only has to find a candidate.
	port, err := vm.PickHostPort(st.Meta.AgentPort, simulatorPortSearchTries)
	if err != nil {
		return "", false, err
	}
	spec, store, err := resolveVMSpec(name, vmStartOptions{
		netMode:   string(vm.NetUser),
		hostPort:  port,
		memoryMiB: vm.DefaultMemoryMiB,
		cpus:      vm.DefaultCPUs,
	})
	if err != nil {
		return "", false, err
	}
	if _, err := store.StartDetached(spec); err != nil {
		if !errors.Is(err, vm.ErrAlreadyRunning) {
			return "", false, err
		}
		// Another process won the race. Its forward is the live one, so re-read
		// the record rather than returning the port we picked and never bound.
		st, err := store.Status(name)
		if err != nil {
			return "", false, err
		}
		if !st.Running || st.State.AgentPort == 0 {
			return "", false, fmt.Errorf("VM %q is starting elsewhere; retry in a moment", name)
		}
		return fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort), false, nil
	}

	// Sticky, so a VM pushed off the default port keeps the address next time.
	// Skipped when the record could not be read: writing it back would persist
	// an empty one and lose the VM's provenance.
	_ = store.RecordAgentPort(name, port)
	return fmt.Sprintf("127.0.0.1:%d", port), true, nil
}

// waitForSimulatorAgent polls addr until the guest agent answers or the budget
// expires; the budget is long because a cold boot under emulation is minutes.
//
// Readiness is a completed GetAgentVersion, not a dial: gRPC connects lazily,
// so dialling a still-booting guest succeeds and fails only on the first call.
func waitForSimulatorAgent(ctx context.Context, name, addr string, budget time.Duration) (*grpcclient.AgentConnection, error) {
	deadline := time.Now().Add(budget)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, resp, err := connectSimulatorAgent(waitCtx, name, addr)
		if err == nil {
			_ = vmRecordHostnameFn(name, resp.GetHostname())
			return conn, nil
		}
		if blocksUnauthenticatedFallback(err) {
			return nil, err
		}
		// Under emulation the budget is five minutes. Without this, a guest that
		// died on its first instruction is waited out in full.
		if !simulatorStillRunning(name) {
			return nil, fmt.Errorf("VM %q stopped while waiting for its agent", name)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the simulator did not answer on %s within %s: %w", addr, budget, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// VM aliases carry identity separately from their current loopback port. Never
// consult a localhost pin or session cached for an unrelated VM/container.
func connectSimulatorAgent(ctx context.Context, name, addr string) (*grpcclient.AgentConnection, *agentpb.GetAgentVersionResponse, error) {
	conn, resp, err := probeSimulatorAgent(ctx, name, addr)
	if err != nil {
		return nil, nil, err
	}
	if err := enforceDevicePin(vmDeviceIDPrefix+name, conn); err != nil {
		conn.Close()
		return nil, nil, err
	}
	conn.SimulatorName = name
	return conn, resp, nil
}

// Discovery honors existing pins through the ladder, but does not enroll a
// merely-listed VM into the user's device-pin config as a side effect of scan.
func probeSimulatorAgent(ctx context.Context, name, addr string) (*grpcclient.AgentConnection, *agentpb.GetAgentVersionResponse, error) {
	key := vmDeviceIDPrefix + name
	conn, _, err := dialAgentLadderFn(ctx, newDialTarget(key, addr))
	if err != nil {
		return nil, nil, err
	}
	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, resp, nil
}

func waitForSimulatorReady(ctx context.Context, name, addr string, budget time.Duration) error {
	conn, err := waitForSimulatorAgent(ctx, name, addr, budget)
	if err == nil {
		conn.Close()
	}
	return err
}

// simulatorStillRunning reports whether name is up. A store it cannot read
// answers true: an unreadable store is no evidence the VM died, and giving up
// on it would abort a boot that is going fine.
func simulatorStillRunning(name string) bool {
	store, err := vm.NewStore()
	if err != nil {
		return true
	}
	st, err := store.Status(name)
	if err != nil {
		return true
	}
	// Only a VM the store knows about can be known to have died. No record at
	// all is inconclusive, not evidence of death.
	return !st.Exists || st.Running
}

// connectSimulatorChoice turns a Simulator-tab selection into a live agent
// connection, provisioning and booting the VM first when needed.
//
// Runs AFTER the picker's tea.Program exits: creating a VM shows downloadImage's
// own progress program, and two Bubble Tea programs cannot share a terminal.
var connectSimulatorChoiceFn = connectSimulatorChoice

func connectSimulatorChoice(ctx context.Context, choice *simulatorChoice, suppressUpdateCheck bool) (*SelectedDevice, error) {
	if choice == nil {
		return nil, fmt.Errorf("no simulator selected")
	}
	if err := ensureQEMUFn(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", errSimulatorUnavailable, err)
	}

	if choice.Create {
		if err := createSimulator(choice.Name); err != nil {
			return nil, err
		}
	}

	addr, started, err := ensureSimulatorRunning(ctx, choice.Name)
	if err != nil {
		// Not re-wrapped: ensureSimulatorRunning marks its own failures, and
		// doing it twice printed "simulator unavailable: simulator
		// unavailable: ...".
		return nil, markSimulatorUnavailable(err)
	}
	conn, err := awaitSimulator(ctx, choice.Name, addr, started)
	if err != nil {
		return nil, err
	}

	picked := &SelectedDevice{Agent: conn, PinKey: vmDeviceIDPrefix + choice.Name}
	if !suppressUpdateCheck {
		picked.Agent, err = checkAndOfferUpdateFn(ctx, picked.Agent)
		if err != nil {
			return nil, err
		}
	}
	return picked, nil
}

// createSimulator provisions name at the latest release, after asking. Shared
// by the run picker and by discover so pressing "c" does the same thing in
// both: same prompt, same download, same errors.
//
// The caller must not be inside a Bubble Tea program -- the download runs its
// own, and two cannot share a terminal.
var createSimulator = func(name string) error {
	if !vm.DetachSupported() {
		return fmt.Errorf("%w: %w", errSimulatorUnavailable, vm.ErrDetachUnsupported)
	}
	if !isInteractiveTerminalFn() && !vmAssumeYes {
		return fmt.Errorf("%w: no simulator yet; create one with 'wendy vm create %s'",
			errSimulatorUnavailable, name)
	}
	if !vmAssumeYes && !confirmFn("Download the WendyOS simulator image and create a VM? This is a one-time download of a few hundred MB.") {
		return ErrUserCancelled
	}
	cliLogln("Creating simulator %q...", name)
	if err := createVM(os.Stderr, name, "", "", defaultSimulatorDiskGiB, false, 0); err != nil {
		return fmt.Errorf("%w: %w", errSimulatorUnavailable, err)
	}
	return nil
}

// awaitSimulator waits under a spinner for the guest agent to answer. Shared by
// the picker and the --device alias so both wait the same way: "running" means
// QEMU is up, not that the guest has finished booting.
func awaitSimulator(ctx context.Context, name, addr string, booting bool) (*grpcclient.AgentConnection, error) {
	return awaitSimulatorWith(ctx, name, addr, booting,
		func(c context.Context) (*grpcclient.AgentConnection, error) {
			return waitForSimulatorAgent(c, name, addr, simulatorBootTimeout())
		})
}

// awaitSimulatorWith runs wait under a spinner where there is a terminal for
// one, and turns a timeout into the guest's own last words.
func awaitSimulatorWith(ctx context.Context, name, addr string, booting bool,
	wait func(context.Context) (*grpcclient.AgentConnection, error),
) (*grpcclient.AgentConnection, error) {
	label := fmt.Sprintf("Connecting to simulator %s...", name)
	if booting {
		label = fmt.Sprintf("Booting simulator %s (first boot under emulation takes a few minutes)...", name)
	}

	var conn *grpcclient.AgentConnection
	var err error
	if isInteractiveTerminalFn() {
		conn, err = runAgentConnectionSpinner(ctx, label, wait)
	} else {
		// The spinner needs a TTY it cannot open here, and `--device sim` has
		// to work from a script. Wait the same way, just without the animation.
		cliLogln("%s", label)
		conn, err = wait(ctx)
	}
	if err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return nil, err
		}
		// A silent timeout after minutes of waiting is the worst outcome, so
		// hand back what the guest actually printed.
		return nil, fmt.Errorf("%w: %v%s", errSimulatorUnavailable, err, vmConsoleTail(name))
	}
	return conn, nil
}

// awaitSimulatorReady waits for the guest agent without building a connection.
// The --device path dials again through the ordinary ladder, so minting one
// here would mean two handshakes and a connection nobody health-checked.
func awaitSimulatorReady(ctx context.Context, name, addr string, booting bool) error {
	_, err := awaitSimulatorWith(ctx, name, addr, booting,
		func(c context.Context) (*grpcclient.AgentConnection, error) {
			if err := waitForSimulatorReady(c, name, addr, simulatorBootTimeout()); err != nil {
				return nil, err
			}
			return nil, nil
		})
	return err
}

const defaultSimulatorDiskGiB = 16

// simulatorAliases name the implicit simulator without the user having to know
// the VM's name or its forwarded port.
var simulatorAliases = map[string]bool{"sim": true, "simulator": true}

// resolveVMAlias turns "sim", "simulator" or "vm:<name>" into the loopback
// address that VM's agent is forwarded to, starting it if it is not running.
// matched is false for anything that does not name a VM, which is left alone.
func resolveVMAlias(ctx context.Context, device string) (addr string, matched bool, err error) {
	name, matched, err := simulatorName(device)
	if err != nil || !matched {
		return "", matched, err
	}
	addr, started, err := ensureSimulatorRunning(ctx, name)
	if err != nil {
		return "", false, markSimulatorUnavailable(err)
	}
	if err := awaitSimulatorReady(ctx, name, addr, started); err != nil {
		return "", false, err
	}
	return addr, true, nil
}

func simulatorName(device string) (string, bool, error) {
	name := ""
	switch {
	case simulatorAliases[device]:
		name = defaultSimulatorVMName
	case strings.HasPrefix(device, vmDeviceIDPrefix):
		name = strings.TrimPrefix(device, vmDeviceIDPrefix)
	default:
		return "", false, nil
	}
	if err := vm.ValidName(name); err != nil {
		// Wrapped, so resolveWithCloudFallback does not answer a malformed VM
		// name by deploying to a cloud device instead.
		return "", false, fmt.Errorf("%w: %w", errSimulatorUnavailable, err)
	}
	return name, true, nil
}
