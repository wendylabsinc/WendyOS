package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	// rebootWaitTimeout bounds the wait for a rebooted device. A Jetson is
	// back in about a minute; a slow SD card or a filesystem check takes longer.
	rebootWaitTimeout = 5 * time.Minute
	// rebootProbeInterval is the pause between attempts to reach the device.
	rebootProbeInterval = 3 * time.Second
	// rebootRequestTimeout bounds the Reboot call itself: the agent answers
	// at once and only then starts the reboot.
	rebootRequestTimeout = 15 * time.Second
	// rebootShutdownTimeout bounds the wait for a device to go down. The agent
	// forces the restart a minute after asking systemd for it, so a device
	// still answering from the same boot after two minutes did not reboot.
	rebootShutdownTimeout = 2 * time.Minute
)

func newDeviceRebootCmd() *cobra.Command {
	var assumeYes, noWait, recovery bool
	cmd := &cobra.Command{
		Use:   "reboot",
		Short: "Reboot the target device",
		Long: "Asks the device's agent to reboot the device, then waits until the device answers again from a new boot. " +
			"Running apps stop with the device; apps that are set to restart come back after boot. " +
			"The command asks for confirmation first; pass --yes to skip the question, which is required when no terminal is attached. " +
			"--no-wait returns as soon as the device has accepted the request. " +
			"--recovery reboots an NVIDIA Jetson into forced-recovery (USB recovery) mode, the mode 'wendy os install' flashes it in, " +
			"without pressing its Force Recovery button; the command then waits only until the device has gone down, because it stays off the network " +
			"until it is flashed or power-cycled. " +
			"An unenrolled device accepts a reboot on its setup port, so this also works on a device that cannot finish enrolling. " +
			"The agent refuses to reboot while an agent or OS update is being installed.",
		Example: "  wendy device reboot\n" +
			"  wendy device reboot --yes --device my-device\n" +
			"  wendy device reboot --yes --no-wait --json\n" +
			"  wendy device reboot --recovery --device my-jetson",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// A device that needs a reboot is often one that is misbehaving:
			// do not put an agent update offer in front of it.
			conn, err := connectToAgent(ctx, ExcludeProviders("local", "docker", "wendy-lite"), SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer conn.Close()

			interactive := isInteractiveTerminal() && !jsonOutput
			return runDeviceReboot(ctx, connDeviceRebooter(conn, interactive), deviceRebootOptions{
				assumeYes:   assumeYes,
				interactive: interactive,
				wait:        !noWait,
				recovery:    recovery,
				asJSON:      jsonOutput,
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Reboot without asking for confirmation")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return once the device has accepted the reboot instead of waiting for it to come back")
	cmd.Flags().BoolVar(&recovery, "recovery", false, "Reboot an NVIDIA Jetson into forced-recovery (USB recovery) mode, where it stays until it is flashed or power-cycled")
	return cmd
}

type deviceRebootOptions struct {
	// assumeYes skips the confirmation question.
	assumeYes bool
	// interactive is true when a person at a terminal can answer it.
	interactive bool
	// wait keeps the command running until the device is back from a new
	// boot, or with recovery, until it has gone down.
	wait bool
	// recovery reboots into forced-recovery mode instead of the installed OS.
	recovery bool
	asJSON   bool
	// timeout bounds the wait; zero means rebootWaitTimeout, or
	// rebootShutdownTimeout with recovery.
	timeout time.Duration
}

// deviceRebooter is everything runDeviceReboot does to a device and a
// terminal, so the flow can be tested without either.
type deviceRebooter struct {
	// label names the device in messages.
	label string
	// reboot sends the request for mode; nil means the device accepted it.
	reboot func(context.Context, agentpbv2.RebootMode) error
	// uptime reconnects to the same device and reads how long it has been up.
	// An error is a device that cannot be reached; a nil uptime with a nil
	// error is one that answered without the reading.
	uptime func(context.Context) (*uint64, error)
	now    func() time.Time
	// wait pauses between probes and returns early with the context's error.
	wait    func(context.Context, time.Duration) error
	confirm func(question string) bool
	// spin, when set, shows progress while work runs.
	spin func(title string, work func() error) error
}

// connDeviceRebooter is the deviceRebooter for a live connection. Reconnects
// go through dialOSUpdateAgent, which keeps the same device, transport and
// pinned identity across the reboot.
func connDeviceRebooter(conn *grpcclient.AgentConnection, interactive bool) deviceRebooter {
	r := deviceRebooter{
		label: rebootDeviceLabel(conn),
		reboot: func(ctx context.Context, mode agentpbv2.RebootMode) error {
			callCtx, cancel := context.WithTimeout(ctx, rebootRequestTimeout)
			defer cancel()
			_, err := agentpbv2.NewWendyPowerServiceClient(conn.Conn).Reboot(callCtx, &agentpbv2.RebootRequest{Mode: mode})
			return err
		},
		uptime: func(ctx context.Context) (*uint64, error) {
			attempt := 3 * time.Second
			if isCloudConnection(conn) {
				// The pinned Cloud reconnect waits for the relay and the agent.
				attempt = 15 * time.Second
			}
			dialCtx, cancel := context.WithTimeout(ctx, attempt)
			fresh, err := dialOSUpdateAgent(dialCtx, conn)
			cancel()
			if err != nil {
				return nil, err
			}
			defer fresh.Close()
			infoCtx, cancel := context.WithTimeout(ctx, attempt)
			defer cancel()
			info, err := agentpbv2.NewWendyDeviceInfoServiceClient(fresh.Conn).GetDeviceInfo(infoCtx, &agentpbv2.GetDeviceInfoRequest{})
			if err != nil {
				return nil, err
			}
			return info.UptimeSeconds, nil
		},
		now: time.Now,
		wait: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
		confirm: confirmDefaultNoFn,
	}
	if interactive {
		r.spin = spinWhile
	}
	return r
}

// rebootDeviceLabel names the connected device the way the user addressed it:
// the name they selected, in preference to the address it resolved to.
func rebootDeviceLabel(conn *grpcclient.AgentConnection) string {
	switch {
	case conn == nil:
		return "the device"
	case conn.SimulatorName != "":
		return "vm:" + conn.SimulatorName
	case conn.DevicePinKey != "":
		return conn.DevicePinKey
	case conn.Host != "":
		return conn.Host
	case conn.Addr != "":
		return conn.Addr
	default:
		return "the device"
	}
}

// spinWhile shows a spinner in the terminal until work returns.
func spinWhile(title string, work func() error) error {
	p := tui.NewProgressProgram(tui.NewSpinner(title))
	go func() { p.Send(tui.SpinnerDoneMsg{Err: work()}) }()
	finalModel, err := tui.RunProgram(p)
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	_, workErr := finalModel.(tui.SpinnerModel).Result()
	return workErr
}

// runDeviceReboot confirms, asks the device to reboot and, unless told not to
// wait, returns only once the device answers from a boot that began after the
// request.
func runDeviceReboot(ctx context.Context, r deviceRebooter, opts deviceRebootOptions, out io.Writer) error {
	if opts.recovery {
		return runDeviceRecoveryReboot(ctx, r, opts, out)
	}
	if !opts.assumeYes {
		if !opts.interactive {
			return fmt.Errorf("a reboot stops the apps running on %s until it is back; pass --yes to reboot it", r.label)
		}
		fmt.Fprintf(out, "A reboot stops the apps running on %s until it is back.\n", tui.Value(r.label))
		if !r.confirm(fmt.Sprintf("Reboot %s now?", r.label)) {
			_, err := fmt.Fprintf(out, "Aborted. %s was not rebooted.\n", r.label)
			return err
		}
	}

	requestedAt := r.now()
	if err := r.reboot(ctx, agentpbv2.RebootMode_REBOOT_MODE_UNSPECIFIED); err != nil {
		return rebootRequestError(r.label, err)
	}

	if !opts.wait {
		if opts.asJSON {
			return writeJSON(out, map[string]any{"status": "requested", "device": r.label, "mode": rebootModeNormal})
		}
		_, err := fmt.Fprintln(out, tui.SuccessMessage(fmt.Sprintf("%s is rebooting. It will be unreachable until it has started again, usually about a minute.", r.label)))
		return err
	}

	timeout := opts.timeout
	if timeout <= 0 {
		timeout = rebootWaitTimeout
	}
	var uptime *uint64
	await := func() error {
		var err error
		uptime, err = awaitNewBoot(ctx, r, requestedAt, timeout)
		return err
	}
	var err error
	if r.spin != nil && !opts.asJSON {
		err = r.spin(fmt.Sprintf("Reboot requested. Waiting for %s to come back online...", r.label), await)
	} else {
		if !opts.asJSON {
			fmt.Fprintf(out, "Reboot requested. Waiting for %s to come back online...\n", r.label)
		}
		err = await()
	}
	if err != nil {
		return err
	}

	if opts.asJSON {
		result := map[string]any{"status": "rebooted", "device": r.label, "mode": rebootModeNormal}
		if uptime != nil {
			result["uptimeSeconds"] = *uptime
		}
		return writeJSON(out, result)
	}
	message := fmt.Sprintf("%s is back online.", r.label)
	if uptime != nil {
		message = fmt.Sprintf("%s is back online (up %s).", r.label, time.Duration(*uptime)*time.Second)
	}
	_, err = fmt.Fprintln(out, tui.SuccessMessage(message))
	return err
}

// The "mode" values of the command's JSON output.
const (
	rebootModeNormal         = "normal"
	rebootModeForcedRecovery = "forced-recovery"
)

// runDeviceRecoveryReboot is runDeviceReboot for forced-recovery mode. A device
// in that mode is off the network until it is flashed or power-cycled, so the
// wait ends when it has gone down rather than when it is back.
func runDeviceRecoveryReboot(ctx context.Context, r deviceRebooter, opts deviceRebootOptions, out io.Writer) error {
	if !opts.assumeYes {
		if !opts.interactive {
			return fmt.Errorf("a reboot into forced-recovery mode stops the apps running on %s and takes it off the network until it is flashed or power-cycled; pass --yes to reboot it", r.label)
		}
		fmt.Fprintf(out, "A reboot into forced-recovery mode stops the apps running on %s and takes it off the network.\n", tui.Value(r.label))
		fmt.Fprintln(out, "It stays in recovery mode until it is flashed with 'wendy os install' or power-cycled.")
		if !r.confirm(fmt.Sprintf("Reboot %s into forced-recovery mode now?", r.label)) {
			_, err := fmt.Fprintf(out, "Aborted. %s was not rebooted.\n", r.label)
			return err
		}
	}

	requestedAt := r.now()
	if err := r.reboot(ctx, agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY); err != nil {
		return rebootRequestError(r.label, err)
	}
	const leave = "Flash it with 'wendy os install', or power-cycle it to leave recovery mode."

	if !opts.wait {
		if opts.asJSON {
			return writeJSON(out, map[string]any{"status": "requested", "device": r.label, "mode": rebootModeForcedRecovery})
		}
		_, err := fmt.Fprintln(out, tui.SuccessMessage(fmt.Sprintf("%s is rebooting into forced-recovery mode. %s", r.label, leave)))
		return err
	}

	timeout := opts.timeout
	if timeout <= 0 {
		timeout = rebootShutdownTimeout
	}
	await := func() error { return awaitShutdown(ctx, r, requestedAt, timeout) }
	var err error
	if r.spin != nil && !opts.asJSON {
		err = r.spin(fmt.Sprintf("Reboot into forced-recovery mode requested. Waiting for %s to go down...", r.label), await)
	} else {
		if !opts.asJSON {
			fmt.Fprintf(out, "Reboot into forced-recovery mode requested. Waiting for %s to go down...\n", r.label)
		}
		err = await()
	}
	if err != nil {
		return err
	}

	if opts.asJSON {
		return writeJSON(out, map[string]any{"status": "offline", "device": r.label, "mode": rebootModeForcedRecovery})
	}
	_, err = fmt.Fprintln(out, tui.SuccessMessage(fmt.Sprintf("%s has gone down to boot into forced-recovery mode. %s", r.label, leave)))
	return err
}

// awaitShutdown polls until the device stops answering. Going down is the last
// thing the CLI can see of a device bound for forced-recovery mode: the mode
// has no network, so whether the device reached it is visible only on the USB
// side. A device that answers from a new boot booted into its OS instead.
func awaitShutdown(ctx context.Context, r deviceRebooter, requestedAt time.Time, timeout time.Duration) error {
	for {
		if err := r.wait(ctx, rebootProbeInterval); err != nil {
			return fmt.Errorf("waiting for %s to go down: %w", r.label, err)
		}
		elapsed := r.now().Sub(requestedAt)
		uptime, err := r.uptime(ctx)
		switch {
		case errors.Is(err, errDeviceIdentityRefused):
			return err
		case err != nil:
			return nil
		case uptime != nil && time.Duration(*uptime)*time.Second < elapsed:
			return fmt.Errorf("%s rebooted back into its OS instead of into forced-recovery mode", r.label)
		}
		if elapsed >= timeout {
			return fmt.Errorf("the reboot was accepted, but %s kept answering from the boot it was already in for %s. The host may have refused the reboot", r.label, timeout)
		}
	}
}

// awaitNewBoot polls until the device answers from a boot that began after
// requestedAt. Reaching the device is not enough: the agent answers the
// request before the shutdown starts and keeps answering for a few seconds, so
// the first replies can come from the boot that is about to end.
func awaitNewBoot(ctx context.Context, r deviceRebooter, requestedAt time.Time, timeout time.Duration) (*uint64, error) {
	sawOffline := false
	for {
		if err := r.wait(ctx, rebootProbeInterval); err != nil {
			return nil, fmt.Errorf("waiting for %s to come back online: %w", r.label, err)
		}
		// Read the clock before the probe: a slow probe then only makes the
		// elapsed time, and so the test below, more conservative.
		elapsed := r.now().Sub(requestedAt)
		uptime, err := r.uptime(ctx)
		switch {
		case errors.Is(err, errDeviceIdentityRefused):
			// The reconnect reached a device that is not the one this
			// connection was pinned to. Polling on would only repeat it.
			return nil, err
		case err != nil:
			sawOffline = true
		case newBootObserved(uptime, elapsed, sawOffline):
			return uptime, nil
		}
		if elapsed >= timeout {
			if sawOffline {
				return nil, fmt.Errorf("%s went down for the reboot but has not come back after %s. Check its power and network, then look for it with 'wendy discover'", r.label, timeout)
			}
			return nil, fmt.Errorf("the reboot was accepted, but the CLI did not see %s restart within %s: it kept answering from the boot it was already in. The host may have refused the reboot", r.label, timeout)
		}
	}
}

// newBootObserved reports whether a device that just answered has booted since
// the reboot was requested. A device that has been up for less time than has
// passed since the request started after it. One that cannot report its uptime
// is judged by having been unreachable in between.
func newBootObserved(uptime *uint64, elapsed time.Duration, sawOffline bool) bool {
	if uptime != nil {
		return time.Duration(*uptime)*time.Second < elapsed
	}
	return sawOffline
}

// rebootRequestError turns the agent's refusal into what the user can do.
func rebootRequestError(label string, err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("the agent on %s does not support reboot; update it with 'wendy device update' and try again", label)
	case codes.FailedPrecondition, codes.Unavailable, codes.InvalidArgument:
		return fmt.Errorf("cannot reboot %s: %s", label, status.Convert(err).Message())
	default:
		return fmt.Errorf("asking %s to reboot: %w", label, err)
	}
}
