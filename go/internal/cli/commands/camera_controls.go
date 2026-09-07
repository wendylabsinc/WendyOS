package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// newCameraControlsCmd shows a local camera's tunable V4L2 controls. It is the
// read half of the pair whose write half is set-control: a viewer that wants to
// fix a blown-out exposure needs to see the current value and the range first.
func newCameraControlsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "controls <id>",
		Short: "Show a local camera's tunable controls (exposure, gain, white balance, ...)",
		Long: `Show the tunable V4L2 controls a local (USB/CSI) camera exposes, with the
current value and the range each accepts.

The list comes from the camera itself, so it is whatever that hardware supports
rather than a fixed set -- a webcam with zoom or focus shows those too.

Run without an id to see which cameras this device has.`,
		// Not ExactArgs(1): running this bare is how someone finds out what it
		// wants, and "accepts 1 arg(s), received 0" does not tell them. With no
		// id we list the cameras, which IS the missing argument.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listCamerasForChoice(cmd, "controls")
			}
			id, err := parseCameraID(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.VideoService.GetCameraControls(ctx, &agentpb.GetCameraControlsRequest{DeviceId: id})
			if err != nil {
				return fmt.Errorf("getting camera controls: %w", err)
			}
			return renderCameraControls(resp.GetControls())
		},
	}
}

func renderCameraControls(controls []*agentpb.CameraControl) error {
	if jsonOutput {
		data, err := json.MarshalIndent(controls, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(controls) == 0 {
		fmt.Println("No tunable controls found (is this a local USB/CSI camera?).")
		return nil
	}
	headers := []string{"Control", "Value", "Min", "Max", "Default", "Mutable"}
	var rows [][]string
	for _, c := range controls {
		mutable := "yes"
		if !c.GetMutable() {
			mutable = "no"
		}
		rows = append(rows, []string{
			c.GetName(),
			fmt.Sprintf("%d", c.GetValue()),
			fmt.Sprintf("%d", c.GetMinimum()),
			fmt.Sprintf("%d", c.GetMaximum()),
			fmt.Sprintf("%d", c.GetDefaultValue()),
			mutable,
		})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

// newCameraSetControlCmd sets one or more controls on a local camera. Values
// persist by default -- re-applied whenever the capture pipeline reopens the
// device and across agent restarts -- because the whole point is a setting that
// holds; --no-persist makes it a one-shot.
func newCameraSetControlCmd() *cobra.Command {
	var noPersist bool
	var reset []string
	var resetAll bool
	cmd := &cobra.Command{
		Use:   "set-control <id> name=value [name=value ...]",
		Short: "Set tunable controls on a local camera",
		Long: `Set tunable V4L2 controls on a local (USB/CSI) camera.

Controls are given as name=value pairs and applied in a safe order (a mode
control such as auto_exposure lands before the control it enables). Values
persist across stream reconnects and reboots unless --no-persist is given.

Example -- stop a flame or lamp from blowing out to white by forcing a short
manual exposure:

  wendy device camera set-control 0 auto_exposure=1 exposure_time_absolute=20 backlight_compensation=0

Run "wendy device camera controls <id>" first to see the available controls and
their ranges.

To undo: --reset puts named controls back to the driver's default and stops
persisting them, and --reset-all does that for every mutable control:

  wendy device camera set-control 0 --reset auto_exposure,exposure_time_absolute
  wendy device camera set-control 0 --reset-all`,
		// Not MinimumNArgs(2): the two things a caller has to know are which
		// camera and which control names, and an arity error tells them
		// neither. Bare, we list the cameras; with an id and no pairs, we list
		// that camera's mutable controls and their ranges.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listCamerasForChoice(cmd, "set-control")
			}
			id, err := parseCameraID(args[0])
			if err != nil {
				return err
			}
			if len(args) == 1 && len(reset) == 0 && !resetAll {
				return listControlsForChoice(cmd, id)
			}

			var controls []*agentpb.CameraControlSetting
			if len(args) > 1 {
				controls, err = parseControlAssignments(args[1:])
				if err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			// --reset-all asks the camera which controls it has rather than
			// keeping a list here, so it covers whatever that hardware exposes.
			if resetAll {
				got, err := conn.VideoService.GetCameraControls(ctx,
					&agentpb.GetCameraControlsRequest{DeviceId: id})
				if err != nil {
					return fmt.Errorf("getting camera controls: %w", err)
				}
				// Every control, not just the currently mutable ones: a
				// control gated inactive by a mode (exposure_time_absolute
				// while auto_exposure is on) is exactly the one that would
				// otherwise stay pinned in the store with no way to clear it.
				for _, c := range got.GetControls() {
					reset = append(reset, c.GetName())
				}
			}
			if len(controls) == 0 && len(reset) == 0 {
				return fmt.Errorf("nothing to do: give name=value pairs, --reset <name>, or --reset-all")
			}

			// Set and reset are separate RPCs because they are separate
			// operations -- reset changes what is PERSISTED, not just the
			// value. Both can appear in one command line, so both are sent,
			// and the results are reported together as one list.
			var results []*agentpb.CameraControlResult
			if len(controls) > 0 {
				resp, err := conn.VideoService.SetCameraControls(ctx, &agentpb.SetCameraControlsRequest{
					DeviceId: id,
					Controls: controls,
					Persist:  !noPersist,
				})
				if err != nil {
					return fmt.Errorf("setting camera controls: %w", err)
				}
				results = append(results, resp.GetResults()...)
			}
			if len(reset) > 0 {
				names := make([]string, 0, len(reset))
				for _, n := range reset {
					names = append(names, strings.TrimSpace(n))
				}
				resp, err := conn.VideoService.ResetCameraControls(ctx, &agentpb.ResetCameraControlsRequest{
					DeviceId: id,
					Names:    names,
				})
				if err != nil {
					return fmt.Errorf("resetting camera controls: %w", err)
				}
				results = append(results, resp.GetResults()...)
			}
			return reportControlResults(cmd.OutOrStdout(), id, results)
		},
	}
	cmd.Flags().BoolVar(&noPersist, "no-persist", false,
		"Apply once now; do not re-apply on stream reconnect or reboot")
	cmd.Flags().StringSliceVar(&reset, "reset", nil,
		"Put these controls back to the driver's default and stop persisting them")
	cmd.Flags().BoolVar(&resetAll, "reset-all", false,
		"Put every mutable control back to the driver's default and forget them all")
	return cmd
}

// parseControlAssignments turns name=value arguments into proto controls. Values
// are integers: V4L2 controls are integer-valued (a menu like auto_exposure is
// its numeric index, e.g. 1=Manual).
func parseControlAssignments(args []string) ([]*agentpb.CameraControlSetting, error) {
	out := make([]*agentpb.CameraControlSetting, 0, len(args))
	for _, a := range args {
		name, val, ok := strings.Cut(a, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("expected name=value, got %q", a)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("control %q: value %q is not an integer", name, val)
		}
		out = append(out, &agentpb.CameraControlSetting{Name: name, Value: int32(n)})
	}
	return out, nil
}

// reportControlResults prints per-control outcomes and returns a non-nil error
// (non-zero exit) if any control was not applied, naming them, so a script can
// tell a partial success from a full one.
func reportControlResults(out io.Writer, id uint32, results []*agentpb.CameraControlResult) error {
	var failed []string
	for _, r := range results {
		if r.GetApplied() {
			fmt.Fprintf(out, "camera %d: %s applied\n", id, r.GetName())
			continue
		}
		detail := r.GetDetail()
		if detail == "" {
			detail = "not applied"
		}
		fmt.Fprintf(out, "camera %d: %s NOT applied (%s)\n", id, r.GetName(), detail)
		failed = append(failed, r.GetName())
	}
	if len(failed) > 0 {
		return fmt.Errorf("camera %d: %d control(s) not applied: %s", id, len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// listCamerasForChoice answers "which camera?" by showing the ones this device
// has. It is what both commands do when run without an id: the answer to a
// missing argument is the set of values it could take, not a count of how many
// were expected.
func listCamerasForChoice(cmd *cobra.Command, sub string) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		return fmt.Errorf("listing cameras: %w", err)
	}
	devices := resp.GetDevices()
	if len(devices) == 0 {
		return fmt.Errorf("no cameras found on this device")
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Which camera? This device has:\n\n")
	headers := []string{"ID", "Name", "Path"}
	var rows [][]string
	for _, d := range devices {
		rows = append(rows, []string{strconv.FormatUint(uint64(d.GetId()), 10), d.GetName(), d.GetPath()})
	}
	fmt.Fprint(out, tui.RenderTable(headers, rows))
	if sub == "set-control" {
		fmt.Fprintf(out, "\nThen: wendy device camera set-control <id> name=value [name=value ...]\n")
		fmt.Fprintf(out, "Run   wendy device camera set-control <id>   to see that camera's controls.\n")
	} else {
		fmt.Fprintf(out, "\nThen: wendy device camera controls <id>\n")
	}
	return nil
}

// listControlsForChoice answers "which control?" for a camera whose id is
// already known -- the second thing set-control needs and the second thing an
// arity error will not say.
func listControlsForChoice(cmd *cobra.Command, id uint32) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := conn.VideoService.GetCameraControls(ctx, &agentpb.GetCameraControlsRequest{DeviceId: id})
	if err != nil {
		return fmt.Errorf("getting camera controls: %w", err)
	}
	controls := resp.GetControls()

	var mutable []*agentpb.CameraControl
	for _, c := range controls {
		if c.GetMutable() {
			mutable = append(mutable, c)
		}
	}
	if len(mutable) == 0 {
		return fmt.Errorf("camera %d exposes no mutable controls (is it a local USB/CSI camera?)", id)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Which control? Camera %d accepts:\n\n", id)
	headers := []string{"Control", "Now", "Min", "Max"}
	var rows [][]string
	for _, c := range mutable {
		rows = append(rows, []string{
			c.GetName(),
			strconv.FormatInt(int64(c.GetValue()), 10),
			strconv.FormatInt(int64(c.GetMinimum()), 10),
			strconv.FormatInt(int64(c.GetMaximum()), 10),
		})
	}
	fmt.Fprint(out, tui.RenderTable(headers, rows))
	fmt.Fprintf(out, "\nThen: wendy device camera set-control %d name=value [name=value ...]\n", id)
	fmt.Fprintf(out, "e.g.  wendy device camera set-control %d %s=%d\n",
		id, mutable[0].GetName(), mutable[0].GetValue())
	return nil
}
