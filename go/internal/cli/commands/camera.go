package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newVideoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "camera",
		Short: "Manage cameras on the target device",
	}
	cmd.AddCommand(
		newCameraListCmd(),
		newCameraViewCmd(),
	)
	return cmd
}

func newCameraListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cameras",
		RunE: func(cmd *cobra.Command, args []string) error {
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

			headers := []string{"ID", "Name", "Path"}
			var rows [][]string
			for _, d := range devices {
				rows = append(rows, []string{
					fmt.Sprintf("%d", d.GetId()),
					d.GetName(),
					d.GetPath(),
				})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

func newCameraViewCmd() *cobra.Command {
	var deviceID, width, height, fps uint32
	var toStdout bool

	cmd := &cobra.Command{
		Use:   "view",
		Short: "Stream H.264 video from a device camera",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := conn.VideoService.StreamVideo(ctx, &agentpb.StreamVideoRequest{
				DeviceId:  deviceID,
				Width:     width,
				Height:    height,
				Framerate: fps,
			})
			if err != nil {
				return fmt.Errorf("starting video stream: %w", err)
			}

			cliLogln("Streaming video (Ctrl+C to stop)...")

			if toStdout {
				return pipeVideoToStdout(stream, cmd.OutOrStdout())
			}
			return playVideoWithGStreamer(ctx, stream)
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Camera device ID")
	cmd.Flags().Uint32Var(&width, "width", 0, "Frame width (0 = device default)")
	cmd.Flags().Uint32Var(&height, "height", 0, "Frame height (0 = device default)")
	cmd.Flags().Uint32Var(&fps, "fps", 0, "Framerate (0 = device default)")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "Pipe encoded video to stdout instead of opening a window (codec: H.264 or VP8/WebM depending on device capabilities)")

	return cmd
}

// pipeVideoToStdout writes VideoFrame data chunks to w until the stream ends.
func pipeVideoToStdout(stream interface {
	Recv() (*agentpb.VideoFrame, error)
}, w io.Writer) error {
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving video: %w", err)
		}
		if _, err := w.Write(frame.GetData()); err != nil {
			return fmt.Errorf("writing video data: %w", err)
		}
	}
}

// playVideoWithGStreamer spawns gst-launch-1.0 and feeds it the video stream via stdin.
// It peeks the first frame to determine the codec, then starts the matching decoder pipeline.
func playVideoWithGStreamer(ctx context.Context, stream interface {
	Recv() (*agentpb.VideoFrame, error)
}) error {
	gstPath, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		return fmt.Errorf("gst-launch-1.0 not found; install GStreamer or use --stdout to pipe raw video")
	}

	// Peek first frame to learn the codec.
	first, err := stream.Recv()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("receiving video: %w", err)
	}

	gstArgs := playbackPipelineArgs(first.GetCodec())

	gst := exec.CommandContext(ctx, gstPath, gstArgs...)
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

	recvErr := make(chan error, 1)
	writeErr := make(chan error, 1)
	go func() {
		// Write the already-received first frame before entering the loop.
		if _, err := stdin.Write(first.GetData()); err != nil {
			writeErr <- fmt.Errorf("writing to GStreamer: %w", err)
			return
		}
		for {
			frame, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if _, err := stdin.Write(frame.GetData()); err != nil {
				writeErr <- fmt.Errorf("writing to GStreamer: %w", err)
				return
			}
		}
	}()

	select {
	case err := <-recvErr:
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("receiving video: %w", err)
	case err := <-writeErr:
		return err
	case <-ctx.Done():
		return nil
	}
}

// playbackPipelineArgs returns the gst-launch-1.0 element arguments for decoding
// and displaying the incoming stream of the given codec, read from stdin (fd 0).
func playbackPipelineArgs(codec agentpb.VideoCodec) []string {
	switch codec {
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		// Server sends VP8 in a WebM container (webmmux streamable=true).
		return []string{
			"fdsrc", "fd=0",
			"!", "matroskademux",
			"!", "vp8dec",
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
		return []string{
			"fdsrc", "fd=0",
			"!", "typefind",
			"!", "h264parse",
			"!", "avdec_h264",
			"!", "videoconvert",
			"!", "queue", "max-size-buffers=1", "leaky=downstream",
			"!", "autovideosink", "sync=false",
		}
	}
}
