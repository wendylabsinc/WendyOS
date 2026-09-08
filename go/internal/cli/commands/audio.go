package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func newAudioCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audio",
		Short: "Manage audio devices on the target device",
		Long: "Interactively manage audio devices on the target device. " +
			"Use up/down to select a device, Enter to set it as the default, " +
			"and left/right to adjust playback volume.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if jsonOutput || !isInteractiveTerminal() {
				return runAudioList(cmd)
			}
			return runAudioTUI(cmd)
		},
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
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAudioList(cmd) },
	}
}

func runAudioList(cmd *cobra.Command) error {
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
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}

	if len(devices) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No audio devices found.")
		return nil
	}

	headers := []string{"ID", "Name", "Type", "Default"}
	rows := make([][]string, 0, len(devices))
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
	fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable(headers, rows))
	return nil
}

func runAudioTUI(cmd *cobra.Command) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := conn.AudioServiceV2.ListAudioDevices(ctx, &agentpbv2.ListAudioDevicesRequest{})
	if err != nil {
		return fmt.Errorf("listing audio devices: %w", err)
	}
	if len(resp.GetDevices()) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No audio devices found.")
		return nil
	}

	model := newAudioTUIModel(resp.GetDevices(), &audioRPCHandler{ctx: ctx, client: conn.AudioServiceV2})
	if _, err := tea.NewProgram(model).Run(); err != nil {
		return fmt.Errorf("audio TUI: %w", err)
	}
	return nil
}

func newAudioSetDefaultCmd() *cobra.Command {
	var deviceID uint32

	cmd := &cobra.Command{
		Use:   "set-default",
		Short: "Set the default audio device",
		Long: "Set the default audio device. When --id is omitted in an interactive terminal, " +
			"choose a device from the audio device list.",
		Example: "  wendy device audio set-default\n" +
			"  wendy device audio set-default --id 42",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			idSet := cmd.Flags().Changed("id")
			if shouldOpenAudioSetDefaultTUI(idSet, isInteractiveTerminal(), jsonOutput) {
				return runAudioTUI(cmd)
			}
			if !idSet {
				return fmt.Errorf("required flag(s) \"id\" not set")
			}

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

	return cmd
}

func shouldOpenAudioSetDefaultTUI(idSet, interactive, json bool) bool {
	return !idSet && interactive && !json
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
	var all bool
	var bufferMs uint32

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

			// Resolve the device locally so we always send a concrete ID. The
			// agent's auto-select (DeviceId == 0) would pick the first ALSA
			// capture device, which on some platforms (e.g. Jetson Tegra) is a
			// virtual routing endpoint that fails with an EIO on read.
			if deviceID == 0 {
				listResp, err := conn.AudioService.ListAudioDevices(ctx, &agentpb.ListAudioDevicesRequest{})
				if err != nil {
					return fmt.Errorf("listing audio devices: %w", err)
				}
				interactive := !stdout && term.IsTerminal(int(os.Stdin.Fd()))
				id, chosen, err := resolveListenDeviceID(listResp.GetDevices(), deviceID, all, interactive, pickAudioDevice)
				if err != nil {
					if errors.Is(err, ErrUserCancelled) {
						return nil
					}
					return err
				}
				deviceID = id
				if chosen != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Using %s — %s\n", chosen.GetName(), chosen.GetDescription())
				}
			}

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

			return playRealtimeAudio(ctx, stream, sampleRate, channels, bufferMs)
		},
	}

	cmd.Flags().Uint32Var(&deviceID, "id", 0, "Audio device ID (default: auto-select or pick a microphone)")
	cmd.Flags().Uint32Var(&sampleRate, "sample-rate", 16000, "Sample rate in Hz")
	cmd.Flags().Uint32Var(&channels, "channels", 1, "Number of audio channels")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "Write raw PCM to stdout instead of playing")
	cmd.Flags().BoolVar(&all, "all", false, "Include virtual/dummy capture devices when selecting a microphone")
	cmd.Flags().Uint32Var(&bufferMs, "buffer-ms", 30, "Playback jitter-buffer target in ms; lower = less latency, more prone to dropouts")

	return cmd
}
