package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func newCameraCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "camera",
		Short: "Manage cameras on the target device",
	}
	cmd.AddCommand(
		newCameraListCmd(),
		newCameraViewCmd(),
		// "watch" is a hidden alias for "view": it just works for muscle memory
		// but stays out of help to keep the listed commands focused.
		newCameraWatchCmd(),
		newCameraLoginCmd(),
		newCameraForgetCmd(),
		newCameraTestCmd(),
		newCameraControlsCmd(),
		newCameraSetControlCmd(),
	)
	return cmd
}

func newCameraListCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cameras",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			var devices []*agentpb.VideoDevice
			if refresh {
				// --refresh runs a discovery round on the device before listing, so a
				// camera that just came online shows up without waiting for the
				// agent's own periodic discovery loop.
				resp, err := conn.VideoService.RefreshCameras(ctx, &agentpb.RefreshCamerasRequest{})
				if err != nil {
					if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Camera listing"); macErr != nil {
						return fmt.Errorf("refreshing cameras: %w", macErr)
					}
					return fmt.Errorf("refreshing cameras: %w", err)
				}
				devices = resp.GetDevices()
			} else {
				resp, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
				if err != nil {
					if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Camera listing"); macErr != nil {
						return fmt.Errorf("listing cameras: %w", macErr)
					}
					return fmt.Errorf("listing cameras: %w", err)
				}
				devices = resp.GetDevices()
			}

			return renderCameraList(devices)
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Run a discovery round on the device before listing")
	return cmd
}

// renderCameraList prints a camera listing as JSON or a table. Factored out of
// newCameraListCmd's RunE so `camera list` and `camera list --refresh` render
// through identical code no matter which RPC produced the devices.
func renderCameraList(devices []*agentpb.VideoDevice) error {
	if jsonOutput {
		data, err := json.MarshalIndent(devices, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(devices) == 0 {
		fmt.Println("No cameras found.")
		return nil
	}

	headers := []string{"ID", "Type", "Name", "Where", "Status"}
	var rows [][]string
	for _, d := range devices {
		rows = append(rows, []string{
			fmt.Sprintf("%d", d.GetId()),
			transportLabel(d.GetTransport()),
			d.GetName(),
			cameraWhere(d),
			cameraStatus(d),
		})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

// transportLabel returns a short label for the camera transport column.
// Unknown transports render as "-" so the column stays aligned and the user
// can spot devices the agent could not classify.
func transportLabel(t agentpb.VideoTransport) string {
	switch t {
	case agentpb.VideoTransport_VIDEO_TRANSPORT_USB:
		return "usb"
	case agentpb.VideoTransport_VIDEO_TRANSPORT_CSI:
		return "csi"
	case agentpb.VideoTransport_VIDEO_TRANSPORT_IP:
		return "ip"
	case agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2:
		return "ros2"
	default:
		return "-"
	}
}

// newCameraLoginCmd stores credentials for a network camera on the device.
//
// The password is read from a terminal without echo, or from
// WENDY_CAMERA_PASSWORD when there is no terminal, so scripts and continuous
// integration do not have to drive a prompt. It is never passed as a flag, which
// would put it in the shell history and the process list.
func newCameraLoginCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "login <id>",
		Short: "Store credentials for a network camera",
		Long: "Store the username and password for a network camera on the device.\n\n" +
			"The password is prompted for without echo, or taken from\n" +
			"WENDY_CAMERA_PASSWORD when standard input is not a terminal. It is\n" +
			"stored on the device and never returned by any command.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseCameraID(args[0])
			if err != nil {
				return err
			}
			password, err := readCameraPassword(cmd, id)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			if _, err := conn.VideoService.SetCameraCredentials(ctx, &agentpb.SetCameraCredentialsRequest{
				DeviceId: id,
				Username: username,
				Password: password,
			}); err != nil {
				return fmt.Errorf("storing camera credentials: %w", err)
			}
			cliLogln("Stored credentials for camera %d.", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "user", "admin", "Camera username")
	return cmd
}

// readCameraPassword prompts on a terminal, or reads the environment variable
// when there is no terminal to prompt on.
func readCameraPassword(cmd *cobra.Command, id uint32) (string, error) {
	if fromEnv := os.Getenv("WENDY_CAMERA_PASSWORD"); fromEnv != "" {
		return fromEnv, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf(
			"no terminal to prompt on; set WENDY_CAMERA_PASSWORD to supply the password for camera %d", id)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Password for camera %d: ", id)
	secret, err := term.ReadPassword(fd)
	fmt.Fprintln(cmd.OutOrStdout())
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(secret), nil
}

// newCameraForgetCmd removes a network camera and its stored credentials.
func newCameraForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <id>",
		Short: "Remove a network camera and its stored credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			if _, err := conn.VideoService.ForgetCamera(ctx, &agentpb.ForgetCameraRequest{
				DeviceId: id,
			}); err != nil {
				return fmt.Errorf("forgetting camera: %w", err)
			}
			cliLogln("Forgot camera %d.", id)
			return nil
		},
	}
}

// cameraTester is the narrow slice of agentpb.WendyVideoServiceClient that
// runCameraTest needs, so tests can stub the RPC without a real gRPC
// connection.
type cameraTester interface {
	TestCameraCredentials(ctx context.Context, in *agentpb.TestCameraCredentialsRequest, opts ...grpc.CallOption) (*agentpb.TestCameraCredentialsResponse, error)
}

// newCameraTestCmd validates a network camera's stored credentials without
// starting a stream. It exists because `camera view` failing on bad
// credentials is a roundabout way to find that out — it starts a hub and a
// capture pipeline first — and because a camera the operator only suspects
// is misconfigured deserves a direct answer.
func newCameraTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Validate a network camera's stored credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			return runCameraTest(ctx, conn.VideoService, id, cmd.OutOrStdout())
		},
	}
}

// runCameraTest calls TestCameraCredentials and turns the result into either a
// printed confirmation or an actionable error.
//
// A camera with no stored login at all surfaces as a gRPC error (the same
// FailedPrecondition + ErrorInfo{IP_CAMERA_NO_CREDENTIALS} StreamVideo
// returns), so it is routed through cameraStreamDiagnostic and gets the
// established `camera login` hint for free. AUTH_FAILED and UNREACHABLE
// arrive as response data instead — expected outcomes of a credentials test,
// not RPC failures — and are turned into errors here so a non-zero exit
// status reaches the shell.
func runCameraTest(ctx context.Context, client cameraTester, id uint32, out io.Writer) error {
	resp, err := client.TestCameraCredentials(ctx, &agentpb.TestCameraCredentialsRequest{DeviceId: id})
	if err != nil {
		return cameraStreamDiagnostic(err)
	}
	switch resp.GetResult() {
	case agentpb.TestCameraCredentialsResponse_RESULT_OK:
		if detail := resp.GetDetail(); detail != "" {
			fmt.Fprintf(out, "Camera %d: credentials accepted (%s): %s.\n", id, resp.GetAddress(), detail)
		} else {
			fmt.Fprintf(out, "Camera %d: credentials accepted (%s).\n", id, resp.GetAddress())
		}
		return nil
	case agentpb.TestCameraCredentialsResponse_RESULT_AUTH_FAILED:
		return fmt.Errorf("camera %d rejected the stored credentials; run `wendy device camera login %d`: %s",
			id, id, resp.GetDetail())
	case agentpb.TestCameraCredentialsResponse_RESULT_UNREACHABLE:
		return fmt.Errorf("camera %d: %s", id, resp.GetDetail())
	default:
		return fmt.Errorf("camera %d: unexpected test result %v", id, resp.GetResult())
	}
}

// parseCameraID validates a camera ID argument.
func parseCameraID(arg string) (uint32, error) {
	id, err := strconv.ParseUint(arg, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid camera id %q; see `wendy device camera list`", arg)
	}
	return uint32(id), nil
}

func newCameraViewCmd() *cobra.Command {
	return newCameraStreamCmd("view", false)
}

// newCameraWatchCmd is a hidden alias for "view". It just works for muscle
// memory but is kept out of help so the listed subcommands stay focused.
func newCameraWatchCmd() *cobra.Command {
	return newCameraStreamCmd("watch", true)
}

// newCameraStreamCmd builds the camera streaming command under the given name.
// "view" is the canonical, listed command; "watch" reuses the same logic as a
// hidden alias.
func newCameraStreamCmd(use string, hidden bool) *cobra.Command {
	var deviceID, width, height, fps uint32
	var stableID string
	var toStdout, raw bool

	cmd := &cobra.Command{
		Use:    use,
		Hidden: hidden,
		Short:  "Stream H.264 video from a device camera",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if raw && !toStdout {
				// Raw frames are bytes for a program, not a picture for a window.
				return fmt.Errorf("--raw writes whole uncompressed frames and needs --stdout")
			}
			// Camera streaming stays off the session broker: the proxy hop
			// adds a second set of flow-control windows between device and
			// viewer, and view latency is a fought-for property here (the
			// #762–#764 latency work). Like watch, the stream also holds one
			// connection for its whole lifetime, so reuse saves nothing after
			// the first frame.
			conn, err := connectToAgent(ctx, DisableSessionBroker())
			if err != nil {
				return err
			}
			defer conn.Close()

			// --stable-id addresses the camera by its stable udev identity and
			// is resolved by the AGENT at request time. --id is the boot-order
			// number, so a value written down yesterday can name a different
			// camera today; --stable-id cannot. When it is given the picker
			// below is skipped, because the camera has already been named
			// exactly.
			if stableID != "" {
				if cmd.Flags().Changed("id") {
					return fmt.Errorf("--id and --stable-id both name a camera; pass one")
				}
			} else if !cmd.Flags().Changed("id") {
				listed, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
				if err != nil {
					return fmt.Errorf("listing cameras: %w", err)
				}
				chosen, err := resolveCameraID(listed.GetDevices(), deviceID, false, pickCamera)
				if err != nil {
					return err
				}
				deviceID = chosen
			}

			codec := agentpb.VideoCodec_VIDEO_CODEC_H264
			if raw {
				codec = agentpb.VideoCodec_VIDEO_CODEC_RAW
			}
			req := &agentpb.StreamVideoRequest{
				DeviceId:  deviceID,
				StableId:  stableID,
				Width:     width,
				Height:    height,
				Framerate: fps,
				Codec:     codec,
			}
			startStream := func() (videoStream, error) {
				return conn.VideoService.StreamVideo(ctx, req)
			}
			resolveCredentials := func(needsLogin uint32) error {
				cwd, cwdErr := os.Getwd()
				if cwdErr != nil {
					cwd = "."
				}
				return resolveCameraCredentials(ctx, cmd,
					func(c context.Context, r *agentpb.SetCameraCredentialsRequest) error {
						_, setErr := conn.VideoService.SetCameraCredentials(c, r)
						return setErr
					}, needsLogin, cwd, cameraPromptAllowed())
			}

			// Server-streaming status errors normally arrive on the first Recv,
			// not while constructing the stream. Wrap both phases so a missing IP
			// camera login is resolved and retried exactly once wherever gRPC
			// surfaces it. Local cameras never take this path.
			stream, err := streamVideoWithCredentialRetry(startStream, resolveCredentials)
			if err != nil {
				return fmt.Errorf("starting video stream: %w", cameraStreamDiagnostic(err))
			}
			diagnosticStream := &cameraDiagnosticStream{videoStream: stream}

			cliLogln("Streaming video (Ctrl+C to stop)...")

			if toStdout {
				return pipeVideoToStdout(diagnosticStream, cmd.OutOrStdout())
			}
			return playVideoWithGStreamer(ctx, diagnosticStream)
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Camera device ID (boot order; see --stable-id)")
	cmd.Flags().StringVar(&stableID, "stable-id", "",
		"Camera stable ID from `camera list --json` — survives reboots and re-plugging")
	cmd.Flags().Uint32Var(&width, "width", 0, "Frame width (0 = device default)")
	cmd.Flags().Uint32Var(&height, "height", 0, "Frame height (0 = device default)")
	cmd.Flags().Uint32Var(&fps, "fps", 0, "Framerate (0 = device default)")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "Pipe encoded video to stdout instead of opening a window (codec: H.264 or VP8/WebM depending on device capabilities)")
	cmd.Flags().BoolVar(&raw, "raw", false, "With --stdout: write the camera's uncompressed capture frames (one whole frame per message, layout printed to stderr) instead of encoded video. Only cameras captured in a raw pixel format offer this; viewers of the same camera keep receiving H.264.")

	return cmd
}

// videoStream is the receive side of the StreamVideo gRPC stream.
type videoStream interface {
	Recv() (*agentpb.VideoFrame, error)
}

type cameraDiagnosticStream struct{ videoStream }

func (s *cameraDiagnosticStream) Recv() (*agentpb.VideoFrame, error) {
	frame, err := s.videoStream.Recv()
	if err != nil {
		return nil, cameraStreamDiagnostic(err)
	}
	return frame, nil
}

// cameraStreamDiagnostic turns machine-readable agent errors into the action the
// operator should take. Every reason the agent can attach names its own fix.
func cameraStreamDiagnostic(err error) error {
	info := streamreason.Info(err)
	if info == nil {
		return err
	}
	switch info.GetReason() {
	case streamreason.TegraFirmwareMismatch:
		rootfs, boot := info.GetMetadata()["rootfs_l4t"], info.GetMetadata()["boot_firmware_l4t"]
		return fmt.Errorf("Jetson CSI camera is unavailable because the rootfs (%s) and boot firmware (%s) are from different L4T families. Run `wendy os install`, choose this Jetson, and perform full USB recovery (do not use --rootfs-only)", rootfs, boot)
	case streamreason.IPCameraNoCredentials:
		id := info.GetMetadata()["device_id"]
		return fmt.Errorf("camera %s has no stored credentials. Run `wendy device camera login %s`", id, id)
	case streamreason.CameraInUse:
		device := info.GetMetadata()["device"]
		return fmt.Errorf("camera %s is held by another application on this device and could not be shared with it. Stop the app holding it, then retry", device)
	}
	return err
}

func pipeVideoToStdout(stream videoStream, w io.Writer) error {
	announcedRaw := false
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving video: %w", err)
		}
		if f := frame.GetRawFormat(); f != nil && !announcedRaw {
			// Once, on stderr: stdout is the frame bytes, and the reader needs
			// the geometry to slice them.
			announcedRaw = true
			cliLogln("raw frames: %dx%d %s, %d bytes per line, %d bytes per frame",
				f.GetWidth(), f.GetHeight(), f.GetFourcc(), f.GetBytesPerLine(),
				f.GetBytesPerLine()*f.GetHeight())
		}
		if _, err := w.Write(frame.GetData()); err != nil {
			return fmt.Errorf("writing video data: %w", err)
		}
	}
}

// playVideoWithGStreamer spawns gst-launch-1.0 and feeds it the video stream via stdin.
// It peeks the first frame to determine the codec, then starts the matching decoder pipeline.
func playVideoWithGStreamer(ctx context.Context, stream videoStream) error {
	// Peek the first frame before checking local playback dependencies. Server-
	// streaming RPCs can surface Unimplemented only on Recv(), and that remote
	// unsupported error is more actionable than a missing local GStreamer binary.
	first, err := stream.Recv()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("receiving video: %w", err)
	}
	codec := first.GetCodec()

	gstPath, err := ensureGSTLaunch(ctx)
	if err != nil {
		return err
	}

	gst := exec.CommandContext(ctx, gstPath, playbackPipelineArgs(codec)...)
	gst.Stderr = os.Stderr

	stdin, err := gst.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating GStreamer stdin pipe: %w", err)
	}

	if err := gst.Start(); err != nil {
		return fmt.Errorf("starting GStreamer: %w", err)
	}
	defer func() {
		stdin.Close()      //nolint:errcheck — signal EOF to GStreamer before killing
		gst.Process.Kill() //nolint:errcheck
		gst.Wait()         //nolint:errcheck
	}()

	done := make(chan error, 1)
	go func() { done <- feedGStreamer(ctx, stream, first, codec, stdin) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return nil
	}
}

func feedGStreamer(ctx context.Context, stream videoStream, first *agentpb.VideoFrame, codec agentpb.VideoCodec, stdin io.Writer) error {
	if codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		if _, err := stdin.Write(first.GetData()); err != nil {
			return fmt.Errorf("writing to GStreamer: %w", err)
		}
		for {
			frame, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("receiving video: %w", err)
			}
			if _, err := stdin.Write(frame.GetData()); err != nil {
				return fmt.Errorf("writing to GStreamer: %w", err)
			}
		}
	}

	// H.264: receive frames into a buffer that keeps only the most recent
	// keyframe onward while the writer is behind, then write the freshest
	// available bytes to GStreamer.
	feed := newH264FeedBuffer()
	feed.push(first.GetData())
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				feed.close(err)
				return
			}
			feed.push(frame.GetData())
		}
	}()

	for {
		data, err, done := feed.take(ctx)
		if len(data) > 0 {
			if _, werr := stdin.Write(data); werr != nil {
				return fmt.Errorf("writing to GStreamer: %w", werr)
			}
		}
		if done {
			// If our caller cancelled the context (Ctrl+C / shutdown), treat any
			// resulting stream error as a clean exit.
			if ctx.Err() != nil {
				return nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("receiving video: %w", err)
			}
			return nil
		}
	}
}

type h264FeedBuffer struct {
	mu     sync.Mutex
	buf    []byte
	err    error
	closed bool
	signal chan struct{} // buffered (cap 1); a value means buf/closed changed
}

func newH264FeedBuffer() *h264FeedBuffer {
	return &h264FeedBuffer{signal: make(chan struct{}, 1)}
}

// wake delivers a non-blocking notification to a waiting take.
func (b *h264FeedBuffer) wake() {
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

// push appends a frame's bytes, then drops any not-yet-taken backlog ahead of
// the most recent keyframe so the consumer never receives stale video.
func (b *h264FeedBuffer) push(data []byte) {
	b.mu.Lock()
	b.buf = append(b.buf, data...)
	if off, ok := lastKeyframeOffset(b.buf); ok && off > 0 {
		b.buf = b.buf[off:]
	}
	b.mu.Unlock()
	b.wake()
}

// close marks the stream finished; err is the terminating error (io.EOF on a
// clean end).
func (b *h264FeedBuffer) close(err error) {
	b.mu.Lock()
	b.err, b.closed = err, true
	b.mu.Unlock()
	b.wake()
}

// take blocks until buffered bytes, stream termination, or context
// cancellation. It returns the buffered bytes (clearing them from the buffer);
// done is true once the stream has ended and all bytes have been taken, with
// err the terminating error (nil or io.EOF on a clean end, nil on cancellation).
func (b *h264FeedBuffer) take(ctx context.Context) (data []byte, err error, done bool) {
	for {
		b.mu.Lock()
		if len(b.buf) > 0 {
			data, b.buf = b.buf, nil
			b.mu.Unlock()
			return data, nil, false
		}
		if b.closed {
			err = b.err
			b.mu.Unlock()
			return nil, err, true
		}
		b.mu.Unlock()

		select {
		case <-b.signal:
		case <-ctx.Done():
			return nil, nil, true
		}
	}
}

// H.264 NAL unit types relevant to keyframe detection (ITU-T H.264 Table 7-1).
const (
	h264NalTypeIDR = 5 // coded slice of an IDR picture
	h264NalTypeSPS = 7 // sequence parameter set
)

func nextStartCode(data []byte, from int) (codeStart, headerIdx int, found bool) {
	for i := from; i+2 < len(data); i++ {
		if data[i] == 0x00 && data[i+1] == 0x00 && data[i+2] == 0x01 {
			cs := i
			if cs > 0 && data[cs-1] == 0x00 {
				cs--
			}
			return cs, i + 3, true
		}
	}
	return 0, 0, false
}

// lastKeyframeOffset scans Annex-B H.264 data and returns the byte offset of
// the start code that begins the most recent keyframe access unit — the SPS
// preceding the IDR (the agent repeats SPS/PPS before every keyframe via
// h264parse config-interval=-1), or the first IDR slice when no SPS precedes it
// in this data. A keyframe picture is frequently coded as several IDR slices
// (e.g. x264enc tune=zerolatency uses sliced threads); those slices form one
// access unit, so only the first slice — not each one — marks a keyframe.
// found is false when data contains no keyframe.
func lastKeyframeOffset(data []byte) (offset int, found bool) {
	sps := -1             // start code of an SPS not yet consumed by a keyframe
	inIDRPicture := false // within a run of IDR slice NALs forming one picture
	for i := 0; ; {
		codeStart, headerIdx, ok := nextStartCode(data, i)
		if !ok || headerIdx >= len(data) {
			break
		}
		nalType := data[headerIdx] & 0x1F
		switch nalType {
		case h264NalTypeSPS:
			sps = codeStart
		case h264NalTypeIDR:
			if !inIDRPicture {
				// First slice of a keyframe access unit.
				if sps >= 0 {
					offset = sps
				} else {
					offset = codeStart
				}
				found = true
				inIDRPicture = true
				sps = -1
			}
		}
		// A non-IDR NAL ends the current IDR picture's run of slices.
		if nalType != h264NalTypeIDR {
			inIDRPicture = false
		}
		i = headerIdx
	}
	return offset, found
}

func playbackPipelineArgs(codec agentpb.VideoCodec) []string {
	switch codec {
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		// Server sends VP8 in a WebM container (webmmux streamable=true).
		// The leaky queue after matroskademux drops whole frames when decode
		// falls behind, draining an encoded-side backlog instead of playing
		// through it; the queue after the decoder absorbs display-sink jitter.
		return []string{
			"fdsrc", "fd=0",
			"!", "matroskademux",
			"!", "queue", "max-size-buffers=2", "leaky=downstream",
			"!", "vp8dec",
			"!", "videoconvert",
			"!", "queue", "max-size-buffers=1", "leaky=downstream",
			"!", "autovideosink", "sync=false",
		}
	default: // H264
		// fdsrc emits untyped buffers (no caps); h264parse needs video/x-h264.
		// A bare "video/x-h264" capsfilter here cannot bridge that gap: the
		// capsfilter must fixate caps onto the untyped buffers, but video/x-h264
		// alone is unfixed (width/height/framerate are template ranges), so it
		// fails with "Output caps are unfixed" and the pipeline won't preroll.
		// typefind inspects the actual bytes, detects the H.264 start codes, and
		// sets fixed content-derived caps; h264parse then auto-detects whether
		// the stream is Annex B byte-stream or length-prefixed AVC.
		//
		// The leaky queue between h264parse and the decoder drops whole access
		// units when decode cannot keep up, so an encoded-side backlog drains
		// by dropping rather than playing through. max-threads=1 removes
		// avdec_h264's frame-threading output delay (~thread-count frames).
		return []string{
			"fdsrc", "fd=0",
			"!", "typefind",
			"!", "h264parse",
			"!", "queue", "max-size-buffers=2", "leaky=downstream",
			"!", "avdec_h264", "max-threads=1",
			"!", "videoconvert",
			"!", "queue", "max-size-buffers=1", "leaky=downstream",
			"!", "autovideosink", "sync=false",
		}
	}
}
