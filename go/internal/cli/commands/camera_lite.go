package commands

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/litevideo"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// liteCameraOptions carries the parts of the stream command's flags that reach
// the Wendy Lite path.
type liteCameraOptions struct {
	toStdout       bool
	nonInteractive bool
	// agentOnlyFlags names the flags the operator passed that only an agent
	// camera can honour. It is collected before the target is known, because
	// which flags were passed is a property of the command line, not of the
	// device it turned out to name.
	agentOnlyFlags []string
}

// streamLiteCamera plays the first camera channel a Wendy Lite device
// publishes, over WendyCom sensor-link.
func streamLiteCamera(ctx context.Context, cmd *cobra.Command, target *SelectedDevice, o liteCameraOptions) error {
	dev := *target.External
	// Capability first: a provider that cannot stream at all should say so,
	// rather than sending the operator off to correct flags that were never
	// the reason it could not be used.
	connector, ok := target.Provider.(providers.WendyComConnector)
	if !ok {
		return fmt.Errorf("selected device (%s) does not support camera streaming; select a WendyOS LAN device instead",
			dev.DisplayName)
	}
	if len(o.agentOnlyFlags) > 0 {
		return agentOnlyCameraFlagsError(o.agentOnlyFlags, dev.DisplayName)
	}
	// An unflashed board has nothing listening on the Wendy Lite protocol, so
	// connecting would only time out. Say what it actually needs, as
	// GetDeviceInfo does for the same case.
	if dev.ConnectionInfo["needsInstall"] == "true" {
		return fmt.Errorf("%s has no Wendy Lite firmware installed, so it has no camera to stream. "+
			"Run `wendy run` in a Wendy Lite project to flash it first", dev.DisplayName)
	}
	// BLE carries WendyCom well enough for configuration, and not at all for
	// video: an L2CAP channel cannot move a frame stream. Refuse before
	// connecting rather than after the first frame fails to arrive.
	if dev.ConnectionType() == "BLE" {
		return fmt.Errorf("%s is connected over Bluetooth LE, which cannot carry a video stream. "+
			"Connect the board over USB, or put it on your network with `wendy device wifi connect`, and retry",
			dev.DisplayName)
	}

	cliLogln("Connecting to %s...", dev.DisplayName)
	// Returned as-is: the provider's error already names serial contention and
	// which mTLS identities it tried, and both are more use than a wrapper.
	client, err := connector.ConnectWendyCom(dev)
	if err != nil {
		return err
	}
	src, err := litevideo.Open(ctx, client, litevideo.Options{})
	if err != nil {
		// Open closed the client on every error path.
		return err
	}
	defer src.Close() //nolint:errcheck — teardown; closing the link drops the subscription anyway
	defer func() {
		if n := src.Dropped(); n > 0 {
			cliLogln("Dropped %d frames to keep up with the device.", n)
		}
	}()

	channel := src.Channel()
	format := src.Format()
	if src.Cameras() > 1 {
		// The choice is the manifest's order and nothing better, so say which
		// one it landed on rather than making it invisible.
		cliLogln("Device has %d cameras; streaming the first, %q.", src.Cameras(), channel.GetName())
	}
	cliLogln("Streaming %s %dx%d@%dfps from %q (channel %d) — Ctrl+C to stop",
		liteCodecLabel(format.GetCodec()), format.GetWidth(), format.GetHeight(), format.GetFps(),
		channel.GetName(), channel.GetChannelId())

	if o.toStdout {
		// Bytes out needs no pipeline, so it works even when the manifest does
		// not say how the channel is encoded.
		return feedLiteVideo(ctx, src, cmd.OutOrStdout())
	}
	codec, err := playbackCodecFromLite(format.GetCodec())
	if err != nil {
		return err
	}
	return runGStreamer(ctx, codec, !o.nonInteractive, func(stdin io.Writer) error {
		return feedLiteVideo(ctx, src, stdin)
	})
}

// feedLiteVideo writes whole sensor-link frames to w — the decoder's stdin,
// or the caller's stdout under --stdout.
//
// There is no backlog to trim here as there is on the agent path: litevideo
// already dropped and resynced upstream, because its frames arrive on the
// WendyCom read loop, which can never be made to wait for a decoder.
func feedLiteVideo(ctx context.Context, src *litevideo.Source, w io.Writer) error {
	for {
		frame, err := src.Recv(ctx)
		if err != nil {
			return liteStreamEnd(err)
		}
		if _, err := w.Write(frame.Data); err != nil {
			return fmt.Errorf("writing video data: %w", err)
		}
	}
}

// liteStreamEnd decides whether the end of a stream is news. Ctrl+C and a
// closed source are how a viewer normally stops, and both exit 0, matching
// runGStreamer's own handling of a cancelled context.
func liteStreamEnd(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, litevideo.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("receiving video: %w", err)
}

// agentOnlyCameraFlags names the camera flags the operator passed that only an
// agent camera can act on. It reads Changed rather than the values, so
// `--id 0` counts as passed and an untouched `--width` does not.
func agentOnlyCameraFlags(cmd *cobra.Command) []string {
	var used []string
	for _, name := range []string{"id", "stable-id", "width", "height", "fps", "raw"} {
		if cmd.Flags().Changed(name) {
			used = append(used, "--"+name)
		}
	}
	return used
}

func agentOnlyCameraFlagsError(flags []string, device string) error {
	return fmt.Errorf("%s streams the first camera in its sensor-link manifest, in the format that manifest declares, "+
		"and takes no camera options: drop %s", device, joinWithAnd(flags))
}

func joinWithAnd(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		out := ""
		for i, s := range items[:len(items)-1] {
			if i > 0 {
				out += ", "
			}
			out += s
		}
		return out + " and " + items[len(items)-1]
	}
}

func liteCodecLabel(c sensorlinkpb.VideoFormat_Codec) string {
	switch c {
	case sensorlinkpb.VideoFormat_MJPEG:
		return "MJPEG"
	case sensorlinkpb.VideoFormat_H264:
		return "H.264"
	default:
		return "video of an unspecified codec"
	}
}

// cameraTargetUnsupportedError explains a resolved target the stream command
// cannot use. It keeps connectFromSelectedDevice's wording: the same selection
// through a different command should not produce a different explanation.
func cameraTargetUnsupportedError(target *SelectedDevice) error {
	if target.Bluetooth != nil {
		return fmt.Errorf("selected device (%s) is a Bluetooth device; this command requires a LAN connection. "+
			"Use 'wendy device wifi connect' which supports BLE", target.Bluetooth.DisplayName)
	}
	return fmt.Errorf("selected device does not support gRPC agent commands")
}
