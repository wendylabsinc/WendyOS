// Command wendy-realsense-source captures calibrated frames from an Intel
// RealSense and writes them to the agent.
//
// It is a SEPARATE executable on purpose. librealsense is a C++ SDK; linking it
// into wendy-agent would put a C++ dependency on every WendyOS image whether or
// not an RGB-D camera is ever attached. So the agent spawns and supervises this
// helper the way the video service supervises its gst-launch child, and an
// image without it degrades explicitly: the agent still LISTS an attached
// RealSense, and says the helper is missing rather than reporting no depth
// camera (go/internal/agent/framesource/realsense.go).
//
// It does exactly one thing the agent cannot: it opens the device through
// librealsense, applies rs2_align to the colour stream, and reads the depth
// scale and the factory intrinsics. Alignment happens here, once, because this
// is the only place the module's extrinsics exist.
//
// Protocol (go/internal/agent/framesource/wire.go): length-prefixed protobuf
// records on stdout and NOTHING else; logs go to stderr.
//
//	describe                     one CalibratedSource record per camera, then exit
//	stream --source S [geometry] one CalibratedSource record, then frames forever
//
// The helper exits when its stdin closes, so an agent that died without reaping
// it does not leave the camera held.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, newCapturer); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", toolName, err)
		os.Exit(1)
	}
}

const toolName = "wendy-realsense-source"

const usage = `usage:
  ` + toolName + ` describe
        Write one CalibratedSource record per attached RealSense, then exit.
  ` + toolName + ` stream --source <id> [--width N] [--height N] [--fps N]
        Write one CalibratedSource record describing the capture, then
        CalibratedFrame records until stdin closes or the process is signalled.

Records are length-prefixed protobuf on stdout; diagnostics go to stderr.`

// parentPipe is the fd whose end-of-file means the agent that spawned this
// helper is gone. A var so a test can drive runStream without its own stdin —
// /dev/null under `go test` — ending the capture on the first read.
var parentPipe io.Reader = os.Stdin

// newCapturerFunc is the seam the build tags switch: a real librealsense
// capturer on an image built with it, and an explicit refusal everywhere else.
type newCapturerFunc func() (capturer, error)

func run(ctx context.Context, args []string, out io.Writer, newCap newCapturerFunc) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "describe":
		return runDescribe(ctx, out, newCap)
	case "stream":
		return runStream(ctx, args[1:], out, newCap)
	case "-h", "--help", "help":
		fmt.Fprintln(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func runDescribe(ctx context.Context, out io.Writer, newCap newCapturerFunc) error {
	cc, err := newCap()
	if err != nil {
		return err
	}
	defer cc.Close() //nolint:errcheck
	sources, err := cc.Describe(ctx)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if err := framesource.WriteRecord(out, framesource.RecordSource, src); err != nil {
			return err
		}
	}
	return nil
}

func runStream(ctx context.Context, args []string, out io.Writer, newCap newCapturerFunc) error {
	fs := flag.NewFlagSet("stream", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts CaptureOptions
	fs.StringVar(&opts.Source, "source", "", "camera to open, as `describe` reports it (empty: the only one attached)")
	widthFlag := fs.Uint("width", 0, "colour width in pixels (0: the device default)")
	heightFlag := fs.Uint("height", 0, "colour height in pixels (0: the device default)")
	fpsFlag := fs.Uint("fps", 0, "frames per second (0: the device default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts.Width = uint32(*widthFlag)
	opts.Height = uint32(*heightFlag)
	opts.Framerate = uint32(*fpsFlag)

	cc, err := newCap()
	if err != nil {
		return err
	}
	defer cc.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A parent that died without reaping us closes our stdin. Without this the
	// camera would stay held by an orphan for as long as the machine is up.
	// Read the pipe once, here, so the goroutine never touches the package var.
	parent := parentPipe
	go func() {
		_, _ = io.Copy(io.Discard, parent)
		cancel()
	}()

	capture, err := cc.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer capture.Close() //nolint:errcheck

	// The descriptor comes FIRST, and describes what was actually negotiated
	// rather than what was asked for. The agent checks a subscriber's
	// requirements against it before a single frame reaches a consumer.
	if err := framesource.WriteRecord(out, framesource.RecordSource, capture.Descriptor()); err != nil {
		return err
	}
	for {
		frame, err := capture.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if err := framesource.WriteRecord(out, framesource.RecordFrame, frame); err != nil {
			// A closed stdout is the agent going away, not a capture failure.
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
	}
}
