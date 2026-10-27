package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const thermalCalibrationLong = `A radiometric thermal camera's temperature correction. Some TOPDON TC001
units read every temperature about twice too hot, on any host, so the agent
lets each camera carry its own correction:

  corrected °C = gain × camera °C + offset

where camera °C is the camera's own reading, count / 16 - 273.15 in the
TC001's stacked 512x484 mode. The agent applies it to everything it publishes in °C for that camera: the
grid on agent/camera/grid/<source> (its scale and offset), the hottest and
coldest markers on agent/camera/markers/<source>, and the check that a
frame's mean is a plausible scene (-40 to 600 °C). Corrected records carry
the gain and offset, so a viewer can tell.

It is the same line as wendy-console's mac-fire-detector
celsiusCalibration {"gain": g, "offset": o}, so a pair measured for one works
in the other. gain must lie in 0.1..10 and offset in -100..100 °C. A camera
that reads about twice too hot needs a gain near 0.5.

Measure it: fill the frame with ice water (0 °C) and then with boiling
water, read each with a reference thermometer and with the camera, and fit
gain = (t_hot - t_cold) / (r_hot - r_cold), offset = t_cold - gain × r_cold.
The pair belongs to one camera; never copy it to another.

The correction is kept by the camera's stable id (/dev/v4l/by-id), not its
/dev/videoN number, so it follows the camera across reboots and replugs, and
it survives agent and OS updates.`

// cameraThermalFlags names the camera a thermal-calibration command acts on.
type cameraThermalFlags struct {
	id       uint32
	stableID string
}

func (f *cameraThermalFlags) add(cmd *cobra.Command) {
	cmd.Flags().Uint32Var(&f.id, "id", 0, "Camera device ID from 'wendy device camera list'")
	cmd.Flags().StringVar(&f.stableID, "stable-id", "",
		"Camera stable `id` from 'camera list --json'; survives reboots and re-plugging")
}

// named reports whether the command names a camera.
func (f *cameraThermalFlags) named(cmd *cobra.Command) (bool, error) {
	if f.stableID != "" && cmd.Flags().Changed("id") {
		return false, fmt.Errorf("--id and --stable-id both name a camera; pass one")
	}
	return f.stableID != "" || cmd.Flags().Changed("id"), nil
}

func newCameraThermalCalibrationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "thermal-calibration",
		Short: "Show, set or clear a thermal camera's temperature correction",
		Long:  thermalCalibrationLong + "\n\nRun bare, it shows every thermal camera's correction.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runThermalCalibration(cmd, "show", &cameraThermalFlags{}, 0, 0)
		},
	}

	var show cameraThermalFlags
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show a thermal camera's temperature correction, or every thermal camera's",
		Long:  thermalCalibrationLong,
		Example: `  wendy device camera thermal-calibration show
  wendy device camera thermal-calibration show --id 4`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runThermalCalibration(cmd, "show", &show, 0, 0)
		},
	}
	show.add(showCmd)

	var set cameraThermalFlags
	var gain, offset float64
	setCmd := &cobra.Command{
		Use:   "set",
		Short: "Set a thermal camera's temperature correction",
		Long:  thermalCalibrationLong,
		Example: `  wendy device camera thermal-calibration set --id 4 --gain 0.48 --offset 0
  wendy device camera thermal-calibration set --stable-id by-id:usb-Camera_USB_Camera_EA6935255-video-index0 --gain 0.4812 --offset 0.35`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runThermalCalibration(cmd, "set", &set, gain, offset)
		},
	}
	set.add(setCmd)
	setCmd.Flags().Float64Var(&gain, "gain", 0, "Multiply every temperature by this (0.1 to 10)")
	setCmd.Flags().Float64Var(&offset, "offset", 0, "Then add this many °C (-100 to 100)")
	_ = setCmd.MarkFlagRequired("gain")

	var clear cameraThermalFlags
	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove a thermal camera's temperature correction",
		Long: thermalCalibrationLong + `

Clearing puts back the camera's own reading. A --stable-id clears a stored
correction even while that camera is not attached.`,
		Example: `  wendy device camera thermal-calibration clear --id 4`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runThermalCalibration(cmd, "clear", &clear, 0, 0)
		},
	}
	clear.add(clearCmd)

	cmd.AddCommand(showCmd, setCmd, clearCmd)
	return cmd
}

func runThermalCalibration(cmd *cobra.Command, action string, camera *cameraThermalFlags, gain, offset float64) error {
	named, err := camera.named(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	target, err := resolveCameraTargetFn(ctx)
	if err != nil {
		return err
	}
	defer target.Close()
	if isLiteCameraTarget(target) {
		return fmt.Errorf("a Wendy Lite camera has no thermal calibration; thermal cameras are calibrated on a WendyOS device")
	}
	conn, err := connectFromSelectedDevice(target, resolveConfig{})
	if err != nil {
		return err
	}
	if err := cameraServicePreflight(ctx, conn, cameraThermalCalibration); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if !named {
		listed, err := listThermalCalibrations(cmd, conn)
		if err != nil {
			return err
		}
		if err := renderThermalCalibrations(out, listed); err != nil {
			return err
		}
		if action == "show" {
			return nil
		}
		if !jsonOutput {
			then := "wendy device camera thermal-calibration " + action + " --id <id>"
			if action == "set" {
				then += " --gain <g> --offset <o>"
			}
			fmt.Fprintf(out, "\nThen: %s\n", then)
		}
		return fmt.Errorf("name the camera to %s with --id or --stable-id", action)
	}

	var result *agentpb.ThermalCalibration
	switch action {
	case "set":
		result, err = conn.VideoService.SetThermalCalibration(ctx, &agentpb.SetThermalCalibrationRequest{
			DeviceId: camera.id, StableId: camera.stableID, Gain: gain, Offset: offset,
		})
	case "clear":
		result, err = conn.VideoService.ClearThermalCalibration(ctx, &agentpb.ClearThermalCalibrationRequest{
			DeviceId: camera.id, StableId: camera.stableID,
		})
	default:
		result, err = conn.VideoService.GetThermalCalibration(ctx, &agentpb.GetThermalCalibrationRequest{
			DeviceId: camera.id, StableId: camera.stableID,
		})
	}
	if err != nil {
		return thermalCalibrationError(action, err)
	}
	if jsonOutput {
		return writeThermalCalibrationJSON(out, thermalCalibrationView(result))
	}
	fmt.Fprintln(out, describeThermalCalibration(result))
	switch {
	case action == "set":
		fmt.Fprintln(out, "Its temperature grid and hot and cold markers use it from the next frame.")
	case action == "clear":
		fmt.Fprintln(out, "Its temperature grid and hot and cold markers carry the camera's own reading from the next frame.")
	}
	return nil
}

// thermalCalibrationError explains an agent that predates the RPCs and leaves
// the agent's own reasons, which name the camera and the bound, as they are.
func thermalCalibrationError(action string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this agent does not support thermal calibration; update the agent on the target device (wendy device update)")
	}
	if st, ok := status.FromError(err); ok {
		return fmt.Errorf("thermal calibration %s: %s", action, st.Message())
	}
	return fmt.Errorf("thermal calibration %s: %w", action, err)
}

// listThermalCalibrations asks for the correction of every camera the device
// lists, and keeps the thermal ones: the agent refuses the others.
func listThermalCalibrations(cmd *cobra.Command, conn *grpcclient.AgentConnection) ([]*agentpb.ThermalCalibration, error) {
	ctx := cmd.Context()
	listed, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing cameras: %w", err)
	}
	var out []*agentpb.ThermalCalibration
	for _, d := range listed.GetDevices() {
		switch d.GetTransport() {
		case agentpb.VideoTransport_VIDEO_TRANSPORT_IP, agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2:
			continue
		}
		got, err := conn.VideoService.GetThermalCalibration(ctx, &agentpb.GetThermalCalibrationRequest{DeviceId: d.GetId()})
		switch status.Code(err) {
		case codes.OK:
			out = append(out, got)
		case codes.FailedPrecondition, codes.InvalidArgument, codes.NotFound:
			// Not a thermal camera, or one with no stable id, which the
			// agent explains when it is named.
		default:
			return nil, thermalCalibrationError("show", err)
		}
	}
	return out, nil
}

func renderThermalCalibrations(out io.Writer, cals []*agentpb.ThermalCalibration) error {
	if jsonOutput {
		views := make([]thermalCalibrationJSON, 0, len(cals))
		for _, c := range cals {
			views = append(views, thermalCalibrationView(c))
		}
		return writeThermalCalibrationJSON(out, views)
	}
	if len(cals) == 0 {
		fmt.Fprintln(out, "No radiometric thermal camera found on this device.")
		return nil
	}
	headers := []string{"ID", "Stable ID", "Correction", "Set"}
	var rows [][]string
	for _, c := range cals {
		correction, when := "none (the camera's own reading)", "-"
		if c.GetCalibrated() {
			correction = thermalCorrectionFormula(c.GetGain(), c.GetOffset())
			when = thermalCalibrationTime(c)
		}
		rows = append(rows, []string{strconv.FormatUint(uint64(c.GetDeviceId()), 10), c.GetStableId(), correction, when})
	}
	fmt.Fprint(out, tui.RenderTable(headers, rows))
	return nil
}

// thermalCalibrationJSON is the --json form. calibrated is always present,
// unlike in the message's own JSON, where false is omitted.
type thermalCalibrationJSON struct {
	DeviceID   uint32  `json:"device_id"`
	Path       string  `json:"path,omitempty"`
	StableID   string  `json:"stable_id"`
	Calibrated bool    `json:"calibrated"`
	Gain       float64 `json:"gain"`
	Offset     float64 `json:"offset"`
	Updated    string  `json:"updated,omitempty"`
}

func thermalCalibrationView(c *agentpb.ThermalCalibration) thermalCalibrationJSON {
	v := thermalCalibrationJSON{
		DeviceID: c.GetDeviceId(), Path: c.GetPath(), StableID: c.GetStableId(),
		Calibrated: c.GetCalibrated(), Gain: c.GetGain(), Offset: c.GetOffset(),
	}
	if c.GetCalibrated() && c.GetUpdatedUnixNanos() != 0 {
		v.Updated = time.Unix(0, c.GetUpdatedUnixNanos()).UTC().Format(time.RFC3339)
	}
	return v
}

func writeThermalCalibrationJSON(out io.Writer, v any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

// describeThermalCalibration is one camera's correction as a sentence.
func describeThermalCalibration(c *agentpb.ThermalCalibration) string {
	camera := c.GetStableId()
	if c.GetPath() != "" {
		camera = fmt.Sprintf("camera %d (%s)", c.GetDeviceId(), c.GetStableId())
	}
	if !c.GetCalibrated() {
		return camera + ": no correction; its temperatures are the camera's own reading"
	}
	return fmt.Sprintf("%s: %s, set %s", camera, thermalCorrectionFormula(c.GetGain(), c.GetOffset()), thermalCalibrationTime(c))
}

// thermalCorrectionFormula writes a pair as the line it is.
func thermalCorrectionFormula(gain, offset float64) string {
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return fmt.Sprintf("°C = %s × camera °C %s %s", strconv.FormatFloat(gain, 'g', -1, 64), sign, strconv.FormatFloat(offset, 'g', -1, 64))
}

func thermalCalibrationTime(c *agentpb.ThermalCalibration) string {
	if c.GetUpdatedUnixNanos() == 0 {
		return "-"
	}
	return time.Unix(0, c.GetUpdatedUnixNanos()).UTC().Format("2006-01-02 15:04 UTC")
}
