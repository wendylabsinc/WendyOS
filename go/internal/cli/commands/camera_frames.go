package commands

// `wendy device camera frames` — the consumer side of the calibrated frame
// service.
//
// `camera view --raw --stdout` is how apps subscribe to the agent's shared
// camera today: whole capture frames on stdout, geometry on stderr, and an app
// slices the bytes. This is the same shape for a measurement rather than a
// picture — colour plane then depth plane per frame — with two additions that
// the raw tap could not have:
//
//   - `--require`, so an app declares what it cannot work without and is
//     REFUSED by name when the camera cannot provide it, instead of quietly
//     receiving a stream worth less than it thinks;
//   - a human mode, so an operator can answer "is this camera actually giving
//     me aligned metric depth right now" without writing a program. That
//     question had no answer before, which is how a robot came to read a name
//     off a wall poster and offer it to a visitor.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// recvLimitSlack is added to a source's own frame-size estimate when sizing the
// client's receive limit. The agent's figure is an estimate scaled by pixel
// count; a frame a few hundred kilobytes over it must still arrive.
const recvLimitSlack = 512 * 1024

func newCameraFramesCmd() *cobra.Command {
	var (
		source         string
		require        []string
		width          uint32
		height         uint32
		fps            uint32
		toStdout       bool
		list           bool
		count          uint32
		nonInteractive bool
	)

	cmd := &cobra.Command{
		Use:   "frames",
		Short: "Stream calibrated RGB-D frames (colour + aligned metric depth) from a device camera",
		Long: "Stream calibrated frames: colour, depth aligned pixel-for-pixel to that colour,\n" +
			"the scale that turns depth into metres, the camera's intrinsics with their\n" +
			"provenance, and the settings it actually applied — all under one frame id and\n" +
			"one capture instant.\n\n" +
			"Declare what you cannot work without via --require. A camera that cannot\n" +
			"provide it is refused by name, before the first frame and again if the\n" +
			"property stops holding mid-stream. Nothing is ever silently downgraded.\n\n" +
			"Requirements: " + framesource.Slugs(framesource.Requirements) + "\n\n" +
			"Run with --list to see what the cameras on this device can promise.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			required, err := parseRequirements(require)
			if err != nil {
				return err
			}

			opts := []resolveOption{DisableSessionBroker()}
			if nonInteractive {
				opts = append(opts, NonInteractive(), SuppressUpdateCheck(), SuppressProvisioningHint())
			}
			conn, err := connectCameraStreamFn(ctx, opts...)
			if err != nil {
				return err
			}
			defer conn.Close()

			listed, err := conn.CalibratedFrameService.ListCalibratedSources(ctx,
				&agentpbv2.ListCalibratedSourcesRequest{})
			if err != nil {
				return fmt.Errorf("listing calibrated frame sources: %w", cameraStreamDiagnostic(err))
			}
			if list {
				return renderCalibratedSources(cmd.OutOrStdout(), listed.GetSources())
			}

			geometry := framesource.Options{Width: width, Height: height, Framerate: fps}
			limit := recvLimitFor(listed.GetSources(), source, geometry)

			req := &agentpbv2.StreamCalibratedFramesRequest{
				Source:        source,
				Width:         width,
				Height:        height,
				Framerate:     fps,
				Require:       required,
				MaxFrameBytes: uint64(limit),
			}
			stream, err := conn.CalibratedFrameService.StreamCalibratedFrames(ctx, req,
				grpc.MaxCallRecvMsgSize(limit))
			if err != nil {
				return fmt.Errorf("starting calibrated frame stream: %w", cameraStreamDiagnostic(err))
			}

			cliLogln("Streaming calibrated frames (Ctrl+C to stop)...")
			if toStdout {
				return pipeCalibratedFramesToStdout(stream, cmd.OutOrStdout(), count)
			}
			return reportCalibratedFrames(stream, cmd.OutOrStdout(), count)
		},
	}

	cmd.Flags().StringVar(&source, "source", "",
		"Calibrated source to stream, as `camera frames --list` reports it (empty: the only one that can serve this request)")
	cmd.Flags().StringSliceVar(&require, "require", nil,
		"Properties this consumer cannot work without; repeat or comma-separate. One of: "+
			framesource.Slugs(framesource.Requirements))
	cmd.Flags().Uint32Var(&width, "width", 0, "Colour width (0 = source default)")
	cmd.Flags().Uint32Var(&height, "height", 0, "Colour height (0 = source default)")
	cmd.Flags().Uint32Var(&fps, "fps", 0, "Framerate (0 = source default)")
	cmd.Flags().BoolVar(&toStdout, "stdout", false,
		"Write the frame bytes (colour plane then depth plane, per frame) to stdout instead of a readable summary. Geometry, depth scale and intrinsics are printed once to stderr.")
	cmd.Flags().BoolVar(&list, "list", false,
		"List the calibrated sources on this device and what each can promise, then exit")
	cmd.Flags().Uint32Var(&count, "count", 0, "Stop after this many frames (0 = stream until interrupted)")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false,
		"Disable terminal prompts and automatic installs")

	return cmd
}

// parseRequirements turns the typed flag values into requirements. A typo is an
// error here rather than a requirement nobody checks: --require alligned-depth
// silently satisfied would be the exact failure this command exists to prevent.
func parseRequirements(values []string) ([]agentpbv2.FrameRequirement, error) {
	var out []agentpbv2.FrameRequirement
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			r, err := framesource.ParseRequirement(part)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return framesource.Normalise(out), nil
}

// recvLimitFor sizes this client's per-message receive limit from what the
// device said its frames cost. One frame is one gRPC message and grpc-go's
// default limit is 4 MiB, which a 1080p colour plane alone exceeds — so a
// client that does not raise it cannot receive the stream it just asked for.
//
// A request that names no geometry joins whatever the source is already
// capturing, and a listing cannot say what that is: another consumer may have
// the camera pinned at a larger mode than its default, and sizing from the
// default would fail on the first Recv -- the exact failure the limit exists
// to prevent. The largest frame the agent relays is bounded by the helper
// wire's record limit, so a joining request accepts up to that.
func recvLimitFor(sources []*agentpbv2.CalibratedSource, name string, opts framesource.Options) int {
	if opts.IsDefault() {
		return framesource.MaxRecordBytes + recvLimitSlack
	}
	largest := uint64(0)
	for _, s := range sources {
		if name != "" && s.GetSource() != name {
			continue
		}
		if n := framesource.FrameBytesFor(s, opts); n > largest {
			largest = n
		}
	}
	if largest > math.MaxInt32-recvLimitSlack {
		return math.MaxInt32
	}
	limit := largest + recvLimitSlack
	if limit < defaultClientRecvBytes {
		return defaultClientRecvBytes
	}
	return int(limit)
}

// defaultClientRecvBytes mirrors grpc-go's own default, so a device that
// reported nothing useful leaves the limit exactly where it was.
const defaultClientRecvBytes = 4 * 1024 * 1024

// calibratedFrameStream is the receive side of StreamCalibratedFrames.
type calibratedFrameStream interface {
	Recv() (*agentpbv2.CalibratedFrame, error)
}

func renderCalibratedSources(w io.Writer, sources []*agentpbv2.CalibratedSource) error {
	if jsonOutput {
		data, err := json.MarshalIndent(sources, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(data))
		return nil
	}
	if len(sources) == 0 {
		fmt.Fprintln(w, "No calibrated frame sources on this device.")
		fmt.Fprintln(w, "An RGB-D camera needs a capture helper the agent can run; a plain webcam has no depth to calibrate.")
		return nil
	}
	headers := []string{"Source", "Kind", "Camera", "Colour", "Depth", "Provides", "Status"}
	var rows [][]string
	for _, s := range sources {
		depth := "-"
		if s.GetDepthWidth() > 0 {
			depth = fmt.Sprintf("%dx%d", s.GetDepthWidth(), s.GetDepthHeight())
		}
		provides := framesource.Slugs(s.GetProvides())
		if provides == "" {
			provides = "-"
		}
		status := "ready"
		if !s.GetAvailable() {
			status = "unavailable"
		}
		rows = append(rows, []string{
			s.GetSource(), s.GetKind(), s.GetDescription(),
			fmt.Sprintf("%dx%d %s", s.GetColourWidth(), s.GetColourHeight(), s.GetColourFourcc()),
			depth, provides, status,
		})
	}
	fmt.Fprint(w, tui.RenderTable(headers, rows))
	// The reason a source is unavailable is a sentence, not a column: it names
	// the fix, and truncating it into a table would lose exactly the part an
	// operator needs.
	for _, s := range sources {
		if !s.GetAvailable() && s.GetUnavailableReason() != "" {
			fmt.Fprintf(w, "\n%s: %s\n", s.GetSource(), s.GetUnavailableReason())
		}
	}
	return nil
}

// stdoutLayout is the byte layout announced on stderr: what a reader of
// stdout has been told each frame consists of.
type stdoutLayout struct {
	colourBytes int
	depthBytes  int
}

func layoutOf(f *agentpbv2.CalibratedFrame) stdoutLayout {
	return stdoutLayout{colourBytes: len(f.GetColour()), depthBytes: len(f.GetDepth().GetData())}
}

// pipeCalibratedFramesToStdout writes the planes for a program to read: the
// whole colour plane then the whole depth plane, per frame, with the geometry
// and the depth scale announced once on stderr. Same contract as
// `camera view --raw --stdout`, one plane richer.
//
// The layout is announced once, so it must hold for every frame. A subscriber
// that did not require depth is allowed to keep receiving when depth drops
// mid-run -- but a reader that was told "C bytes of colour then D of depth"
// would then consume frame N+1's colour as frame N's depth, silently, from
// that point on. That is the downgrade this command exists to make visible,
// so the stream is ended with an error instead of written in a layout the
// reader was never told about.
func pipeCalibratedFramesToStdout(stream calibratedFrameStream, w io.Writer, count uint32) error {
	var announced *stdoutLayout
	var seen uint32
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving calibrated frames: %w", cameraStreamDiagnostic(err))
		}
		if announced == nil {
			announceCalibratedLayout(frame)
			l := layoutOf(frame)
			announced = &l
		} else if got := layoutOf(frame); got != *announced {
			return fmt.Errorf("frame %d does not match the layout announced on stderr "+
				"(colour %d bytes, now %d; depth %d bytes, now %d); a reader of --stdout cannot follow that, "+
				"so the stream was ended rather than written in a layout it was not told about. "+
				"Add --require %s to be refused up front when the camera cannot keep providing depth",
				frame.GetFrameId(), announced.colourBytes, got.colourBytes, announced.depthBytes, got.depthBytes,
				framesource.SlugAlignedDepth)
		}
		if _, err := w.Write(frame.GetColour()); err != nil {
			return fmt.Errorf("writing colour plane: %w", err)
		}
		if d := frame.GetDepth(); d != nil {
			if _, err := w.Write(d.GetData()); err != nil {
				return fmt.Errorf("writing depth plane: %w", err)
			}
		}
		seen++
		if count > 0 && seen >= count {
			return nil
		}
	}
}

// announceCalibratedLayout prints, once and on stderr, everything a program
// reading stdout needs to slice the bytes and turn them into metres.
func announceCalibratedLayout(f *agentpbv2.CalibratedFrame) {
	cf := f.GetColourFormat()
	cliLogln("colour: %dx%d %s, %d bytes per line, %d bytes per frame",
		cf.GetWidth(), cf.GetHeight(), cf.GetFourcc(), cf.GetBytesPerLine(),
		cf.GetBytesPerLine()*cf.GetHeight())
	if d := f.GetDepth(); d != nil {
		cliLogln("depth: %dx%d uint16 LE, %d bytes per line, %d bytes per frame, %s, metres = value * %g",
			d.GetWidth(), d.GetHeight(), d.GetBytesPerLine(),
			d.GetBytesPerLine()*d.GetHeight(), alignmentLabel(d.GetAlignment()), d.GetScaleM())
	} else {
		cliLogln("depth: none on this source")
	}
	if in := f.GetIntrinsics(); in != nil {
		cliLogln("intrinsics (%s): fx=%.2f fy=%.2f cx=%.2f cy=%.2f at %dx%d%s",
			provenanceLabel(in.GetProvenance()), in.GetFx(), in.GetFy(), in.GetCx(), in.GetCy(),
			in.GetWidth(), in.GetHeight(), noteSuffix(in.GetNote()))
	} else {
		cliLogln("intrinsics: none reported")
	}
	cliLogln("each frame is %d bytes of colour followed by %d bytes of depth",
		len(f.GetColour()), len(f.GetDepth().GetData()))
}

// reportCalibratedFrames is the operator's view: one line per frame naming what
// actually arrived, including the distance at the centre pixel — the cheapest
// possible proof that the depth is real, aligned and in metres.
func reportCalibratedFrames(stream calibratedFrameStream, w io.Writer, count uint32) error {
	var seen uint32
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving calibrated frames: %w", cameraStreamDiagnostic(err))
		}
		fmt.Fprintln(w, describeCalibratedFrame(frame))
		seen++
		if count > 0 && seen >= count {
			return nil
		}
	}
}

func describeCalibratedFrame(f *agentpbv2.CalibratedFrame) string {
	cf := f.GetColourFormat()
	parts := []string{
		fmt.Sprintf("frame %d", f.GetFrameId()),
		fmt.Sprintf("t=%d", f.GetCapturedAtNs()),
		fmt.Sprintf("colour %dx%d %s", cf.GetWidth(), cf.GetHeight(), cf.GetFourcc()),
	}
	if d := f.GetDepth(); d != nil {
		centre := "centre n/a"
		if m, ok := centreDistanceMetres(d); ok {
			centre = fmt.Sprintf("centre %.3f m", m)
		}
		parts = append(parts, fmt.Sprintf("depth %dx%d %s scale=%g %s",
			d.GetWidth(), d.GetHeight(), alignmentLabel(d.GetAlignment()), d.GetScaleM(), centre))
	} else {
		parts = append(parts, "depth none")
	}
	if in := f.GetIntrinsics(); in != nil {
		parts = append(parts, fmt.Sprintf("intrinsics fx=%.1f fy=%.1f cx=%.1f cy=%.1f (%s)",
			in.GetFx(), in.GetFy(), in.GetCx(), in.GetCy(), provenanceLabel(in.GetProvenance())))
	} else {
		parts = append(parts, "intrinsics none")
	}
	if s := f.GetSettings(); s != nil {
		parts = append(parts, describeCaptureSettings(s))
	} else {
		parts = append(parts, "settings none")
	}
	return strings.Join(parts, "  ")
}

// describeCaptureSettings prints the exposure in microseconds only when the
// device said what its unit is. A converted figure derived from a guessed unit
// reads like a measurement and is not one.
func describeCaptureSettings(s *agentpbv2.CaptureSettings) string {
	auto := "manual"
	if s.GetAutoExposure() {
		auto = "auto"
	}
	if s.GetExposureUnitUs() > 0 {
		return fmt.Sprintf("exposure %.0f us (%s) gain %.0f", s.GetExposureUs(), auto, s.GetGain())
	}
	return fmt.Sprintf("exposure %d (driver units, %s) gain %.0f", s.GetExposureRaw(), auto, s.GetGain())
}

// centreDistanceMetres reads the depth value at the centre pixel and converts
// it. A zero there is not a distance — RealSense writes 0 where it has no
// measurement — so it is reported as unavailable rather than as "0 metres".
func centreDistanceMetres(d *agentpbv2.DepthPlane) (float64, bool) {
	if d.GetWidth() == 0 || d.GetHeight() == 0 || d.GetScaleM() <= 0 {
		return 0, false
	}
	x, y := d.GetWidth()/2, d.GetHeight()/2
	// uint64 throughout: the geometry is what the device sent, and int is 32
	// bits on some of the ARM images this runs on. Fail closed, visibly.
	offset := uint64(y)*uint64(d.GetBytesPerLine()) + uint64(x)*2
	data := d.GetData()
	if offset+2 > uint64(len(data)) {
		return 0, false
	}
	raw := binary.LittleEndian.Uint16(data[offset : offset+2])
	if raw == 0 {
		return 0, false
	}
	return float64(raw) * float64(d.GetScaleM()), true
}

func alignmentLabel(a agentpbv2.DepthPlane_Alignment) string {
	switch a {
	case agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR:
		return "aligned-to-colour"
	case agentpbv2.DepthPlane_ALIGNMENT_SENSOR_NATIVE:
		return "sensor-native"
	default:
		return "alignment-unstated"
	}
}

func provenanceLabel(p agentpbv2.CameraIntrinsics_Provenance) string {
	switch p {
	case agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED:
		return "measured"
	case agentpbv2.CameraIntrinsics_PROVENANCE_ASSUMED:
		return "assumed"
	default:
		return "provenance-unstated"
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " — " + note
}
