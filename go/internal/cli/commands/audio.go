package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newAudioCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audio",
		Short: "Manage audio devices on the target device",
	}

	cmd.AddCommand(
		newAudioListCmd(),
		newAudioSetDefaultCmd(),
		newAudioMonitorCmd(),
		newAudioListenCmd(),
	)

	return cmd
}

func newAudioListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List audio devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.AudioService.ListAudioDevices(ctx, &agentpb.ListAudioDevicesRequest{})
			if err != nil {
				return fmt.Errorf("listing audio devices: %w", err)
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
				fmt.Println("No audio devices found.")
				return nil
			}

			headers := []string{"ID", "Name", "Type", "Default"}
			var rows [][]string
			for _, d := range devices {
				defaultStr := ""
				if d.GetIsDefault() {
					defaultStr = "*"
				}
				rows = append(rows, []string{
					fmt.Sprintf("%d", d.GetId()),
					d.GetName(),
					d.GetType().String(),
					defaultStr,
				})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

func newAudioSetDefaultCmd() *cobra.Command {
	var deviceID uint32

	cmd := &cobra.Command{
		Use:   "set-default",
		Short: "Set the default audio device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.AudioService.SetDefaultAudioDevice(ctx, &agentpb.SetDefaultAudioDeviceRequest{
				DeviceId: deviceID,
			})
			if err != nil {
				return fmt.Errorf("setting default audio device: %w", err)
			}

			if !resp.GetSuccess() {
				return fmt.Errorf("failed: %s", resp.GetErrorMessage())
			}

			fmt.Printf("Default audio device set to ID %d.\n", deviceID)
			return nil
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Audio device ID")
	_ = cmd.MarkFlagRequired("id")

	return cmd
}

func newAudioMonitorCmd() *cobra.Command {
	var deviceID uint32
	var rateHz uint32

	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Real-time VU meter for audio levels",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := conn.AudioService.StreamAudioLevels(ctx, &agentpb.StreamAudioLevelsRequest{
				DeviceId:     deviceID,
				UpdateRateHz: rateHz,
			})
			if err != nil {
				return fmt.Errorf("starting audio level stream: %w", err)
			}

			fmt.Println("Audio Monitor (Ctrl+C to stop)")
			fmt.Println(strings.Repeat("-", 60))

			var received int
			for {
				update, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("receiving audio levels: %w", err)
				}
				received++

				// Simple VU meter visualization.
				peakDb := update.GetPeakDb()
				rmsDb := update.GetRmsDb()

				// Convert dB to a bar (0 dB = max, -60 dB = min).
				barLen := int((peakDb + 60) / 60 * 40)
				if barLen < 0 {
					barLen = 0
				}
				if barLen > 40 {
					barLen = 40
				}

				bar := strings.Repeat("|", barLen) + strings.Repeat(" ", 40-barLen)
				fmt.Printf("\rPeak: %6.1f dB  RMS: %6.1f dB  [%s]", peakDb, rmsDb, bar)
			}

			fmt.Println()
			if received == 0 {
				return fmt.Errorf("no audio data received — check that a microphone is available on the device")
			}
			return nil
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Audio device ID")
	cmd.Flags().Uint32Var(&rateHz, "rate", 10, "Update rate in Hz")

	return cmd
}

func newAudioListenCmd() *cobra.Command {
	var deviceID uint32
	var sampleRate uint32
	var channels uint32
	var stdout bool

	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Stream raw audio from a device microphone",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := conn.AudioService.StreamAudio(ctx, &agentpb.StreamAudioRequest{
				DeviceId:   deviceID,
				SampleRate: sampleRate,
				Channels:   channels,
			})
			if err != nil {
				return fmt.Errorf("starting audio stream: %w", err)
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Streaming audio (Ctrl+C to stop)...\n")

			if stdout {
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						return fmt.Errorf("receiving audio: %w", err)
					}
					if _, err := cmd.OutOrStdout().Write(chunk.GetPcmData()); err != nil {
						return fmt.Errorf("writing audio data: %w", err)
					}
				}
				return nil
			}

			return playRealtimeAudio(ctx, stream, sampleRate, channels)
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Audio device ID")
	cmd.Flags().Uint32Var(&sampleRate, "sample-rate", 16000, "Sample rate in Hz")
	cmd.Flags().Uint32Var(&channels, "channels", 1, "Number of audio channels")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "Write raw PCM to stdout instead of playing")

	return cmd
}
