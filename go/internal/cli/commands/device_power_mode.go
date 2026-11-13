package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func newDevicePowerModeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "power-mode",
		Short: "Show and set the NVIDIA Jetson power mode",
		Long: "Interactively shows the power mode (nvpmodel) a Jetson device is in and the modes it " +
			"offers, and lets you pick one to set. Modes are the device's own names and IDs (for " +
			"example MAXN or MODE_30W). Without an interactive terminal, or with --json, it prints " +
			"the same information as 'list'. Use 'list', 'get' and 'set' for explicit, " +
			"non-interactive commands. Devices that are not a Jetson report that power modes are " +
			"not supported.",
		Example: "  wendy device power-mode\n" +
			"  wendy device power-mode list\n" +
			"  wendy device power-mode get\n" +
			"  wendy device power-mode set MAXN",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOutput || !isInteractiveTerminal() {
				return runPowerModeList(cmd)
			}
			return runPowerModeInteractive(cmd)
		},
	}
	cmd.AddCommand(newDevicePowerModeListCmd(), newDevicePowerModeGetCmd(), newDevicePowerModeSetCmd())
	return cmd
}

func newDevicePowerModeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the power modes the device offers and the current one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPowerModeList(cmd)
		},
	}
}

func newDevicePowerModeGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the current power mode",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			return runPowerModeGet(ctx, conn.AgentService, os.Stdout, jsonOutput)
		},
	}
}

func runPowerModeList(cmd *cobra.Command) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return runPowerModeShow(ctx, conn.AgentService, os.Stdout, jsonOutput)
}

func newDevicePowerModeSetCmd() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "set <name|id>",
		Short: "Switch the NVIDIA Jetson power mode",
		Long: "Switches a Jetson device to the named or numbered power mode, as listed by " +
			"'wendy device power-mode'. Some modes only take effect after a reboot. For those, " +
			"the command says so and asks for confirmation; answering yes sets the mode and reboots " +
			"the device in one step, and answering no leaves the device untouched. Without a " +
			"terminal, pass --yes to confirm in advance.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			interactive := isInteractiveTerminal() && !jsonOutput
			return runPowerModeSet(ctx, conn.AgentService, args[0], assumeYes, interactive, os.Stdout, jsonOutput)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Set the mode and reboot the device without asking when the mode needs a reboot (required without an interactive terminal)")
	return cmd
}

// powerModeRPCError turns an agent error into one a person can act on.
func powerModeRPCError(action string, err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("%s: this device's agent does not support power modes; update the agent first", action)
	case codes.Unavailable, codes.InvalidArgument:
		return fmt.Errorf("%s: %s", action, status.Convert(err).Message())
	}
	return fmt.Errorf("%s: %w", action, err)
}

func powerModeLabel(m *agentpb.PowerMode) string {
	return fmt.Sprintf("%s (%d)", m.GetName(), m.GetId())
}

func powerModeJSON(m *agentpb.PowerMode) map[string]any {
	if m == nil {
		return nil
	}
	return map[string]any{"id": m.GetId(), "name": m.GetName()}
}

func writeJSON(out io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

func runPowerModeShow(ctx context.Context, client agentpb.WendyAgentServiceClient, out io.Writer, asJSON bool) error {
	resp, err := client.GetPowerMode(ctx, &agentpb.GetPowerModeRequest{})
	if err != nil {
		return powerModeRPCError("reading the power mode", err)
	}
	current := resp.GetCurrent()
	boot := resp.GetBootDefault()

	if asJSON {
		available := make([]map[string]any, 0, len(resp.GetAvailable()))
		for _, m := range resp.GetAvailable() {
			available = append(available, powerModeJSON(m))
		}
		doc := map[string]any{"current": powerModeJSON(current), "available": available}
		if boot != nil {
			doc["bootDefault"] = powerModeJSON(boot)
		}
		return writeJSON(out, doc)
	}

	fmt.Fprintf(out, "%s %s\n\n", tui.Dim("Power mode:"), tui.Value(powerModeLabel(current)))
	fmt.Fprintln(out, tui.Dim("Available modes:"))
	for _, m := range resp.GetAvailable() {
		marker := " "
		var notes []string
		if m.GetId() == current.GetId() {
			marker = "*"
			notes = append(notes, "current")
		}
		if boot != nil && m.GetId() == boot.GetId() {
			notes = append(notes, "boot default")
		}
		line := fmt.Sprintf("  %s %d  %s", marker, m.GetId(), m.GetName())
		if len(notes) > 0 {
			line += tui.Dim(fmt.Sprintf("  (%s)", joinNotes(notes)))
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

// runPowerModeGet prints only the current mode: "NAME (id)", or {"current": ...}.
func runPowerModeGet(ctx context.Context, client agentpb.WendyAgentServiceClient, out io.Writer, asJSON bool) error {
	resp, err := client.GetPowerMode(ctx, &agentpb.GetPowerModeRequest{})
	if err != nil {
		return powerModeRPCError("reading the power mode", err)
	}
	if asJSON {
		return writeJSON(out, map[string]any{"current": powerModeJSON(resp.GetCurrent())})
	}
	_, err = fmt.Fprintln(out, powerModeLabel(resp.GetCurrent()))
	return err
}

func joinNotes(notes []string) string {
	s := notes[0]
	for _, n := range notes[1:] {
		s += ", " + n
	}
	return s
}

// powerModeSetOutcome is what a set attempt ended in.
type powerModeSetOutcome struct {
	resp *agentpb.SetPowerModeResponse
	// aborted is set when the user declined the reboot: nothing was changed.
	aborted bool
}

// rebooting reports that the device is restarting into the new mode.
func (o powerModeSetOutcome) rebooting() bool {
	return !o.aborted && o.resp.GetRebootScheduled()
}

// setPowerMode switches the mode. The first call never reboots: for a mode that
// needs a reboot the agent only reports it, and nothing is changed (a declined
// nvpmodel prompt records nothing). Only a confirmed second call, which sets the
// mode and reboots at once, changes the device.
func setPowerMode(ctx context.Context, client agentpb.WendyAgentServiceClient, mode string, assumeYes, interactive bool, out io.Writer) (powerModeSetOutcome, error) {
	resp, err := client.SetPowerMode(ctx, &agentpb.SetPowerModeRequest{Mode: mode})
	if err != nil {
		return powerModeSetOutcome{}, powerModeRPCError("setting the power mode", err)
	}
	label := powerModeLabel(resp.GetMode())

	if resp.GetRebootRequired() {
		if !assumeYes {
			if !interactive {
				return powerModeSetOutcome{}, fmt.Errorf("power mode %s needs a reboot and was not set; pass --yes to set it and reboot the device", label)
			}
			fmt.Fprintf(out, "Power mode %s only takes effect after a reboot.\n", tui.Value(label))
			if !confirmDefaultNoFn(fmt.Sprintf("Set %s and reboot the device now?", label)) {
				return powerModeSetOutcome{resp: resp, aborted: true}, nil
			}
		}
		resp, err = client.SetPowerMode(ctx, &agentpb.SetPowerModeRequest{Mode: mode, Reboot: true})
		if status.Code(err) == codes.Unavailable {
			// The device may restart before it answers; the agent is gone, not failing.
			resp = &agentpb.SetPowerModeResponse{Mode: resp.GetMode(), RebootRequired: true, RebootScheduled: true}
			err = nil
		}
		if err != nil {
			return powerModeSetOutcome{}, powerModeRPCError("setting the power mode", err)
		}
	}
	return powerModeSetOutcome{resp: resp}, nil
}

func printPowerModeSet(out io.Writer, o powerModeSetOutcome, asJSON bool) error {
	if o.aborted {
		_, err := fmt.Fprintln(out, "Aborted. The power mode is unchanged.")
		return err
	}
	resp := o.resp
	label := powerModeLabel(resp.GetMode())
	if asJSON {
		return writeJSON(out, map[string]any{
			"mode":            powerModeJSON(resp.GetMode()),
			"rebootRequired":  resp.GetRebootRequired(),
			"rebootScheduled": resp.GetRebootScheduled(),
		})
	}
	if resp.GetRebootScheduled() {
		fmt.Fprintf(out, "Power mode %s set; the device is rebooting now.\n", tui.Value(label))
		fmt.Fprintln(out, "Run 'wendy device power-mode get' once it is back to check.")
	} else {
		fmt.Fprintf(out, "Power mode set to %s.\n", tui.Value(label))
	}
	return nil
}

func runPowerModeSet(ctx context.Context, client agentpb.WendyAgentServiceClient, mode string, assumeYes, interactive bool, out io.Writer, asJSON bool) error {
	o, err := setPowerMode(ctx, client, mode, assumeYes, interactive, out)
	if err != nil {
		return err
	}
	return printPowerModeSet(out, o, asJSON)
}
