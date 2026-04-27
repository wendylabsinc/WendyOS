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
		Use:   "video",
		Short: "Manage video devices on the target device",
	}
	cmd.AddCommand(
		newVideoListCmd(),
		newVideoStreamCmd(),
	)
	return cmd
}

func newVideoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List video devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
			if err != nil {
				return fmt.Errorf("listing video devices: %w", err)
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
				fmt.Println("No video devices found.")
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

func newVideoStreamCmd() *cobra.Command {
	var deviceID, width, height, fps uint32
	var toStdout bool

	cmd := &cobra.Command{
		Use:   "stream",
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

			fmt.Fprintf(cmd.ErrOrStderr(), "Streaming video (Ctrl+C to stop)...\n")

			if toStdout {
				return pipeVideoToStdout(stream, cmd.OutOrStdout())
			}
			return playVideoWithGStreamer(ctx, stream)
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Video device ID")
	cmd.Flags().Uint32Var(&width, "width", 0, "Frame width (0 = device default)")
	cmd.Flags().Uint32Var(&height, "height", 0, "Frame height (0 = device default)")
	cmd.Flags().Uint32Var(&fps, "fps", 0, "Framerate (0 = device default)")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "Write raw H.264 to stdout instead of opening a window")

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

// playVideoWithGStreamer spawns gst-launch-1.0 and feeds it the H.264 stream via stdin.
func playVideoWithGStreamer(ctx context.Context, stream interface {
	Recv() (*agentpb.VideoFrame, error)
}) error {
	gstPath, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		return fmt.Errorf("gst-launch-1.0 not found; install GStreamer or use --stdout to pipe raw H.264")
	}

	gst := exec.CommandContext(ctx, gstPath,
		"fdsrc", "fd=0",
		"!", "h264parse",
		"!", "avdec_h264",
		"!", "autovideosink",
	)
	gst.Stderr = os.Stderr

	stdin, err := gst.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating GStreamer stdin pipe: %w", err)
	}

	if err := gst.Start(); err != nil {
		return fmt.Errorf("starting GStreamer: %w", err)
	}
	defer func() { gst.Process.Kill(); gst.Wait() }() //nolint:errcheck

	recvErr := make(chan error, 1)
	go func() {
		defer stdin.Close()
		for {
			frame, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if _, writeErr := stdin.Write(frame.GetData()); writeErr != nil {
				recvErr <- writeErr
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
	case <-ctx.Done():
		return nil
	}
}
