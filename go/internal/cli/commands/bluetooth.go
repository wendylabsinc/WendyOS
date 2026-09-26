package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui/bttable"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Per-operation timeouts bound each Bluetooth gRPC call so a hung agent cannot
// block the TUI indefinitely. They are generous: pairing in particular can be
// slow and may require user interaction on the peripheral.
const (
	btConnectTimeout    = 60 * time.Second
	btDisconnectTimeout = 30 * time.Second
	btForgetTimeout     = 30 * time.Second
)

func newBluetoothCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "bluetooth",
		Aliases: []string{"bt"},
		Short:   "Manage Bluetooth on the target device",
		Hidden:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBluetoothInteractive(cmd)
		},
	}

	cmd.AddCommand(
		newBluetoothListCmd(),
		newBluetoothConnectCmd(),
		newBluetoothDisconnectCmd(),
		newBluetoothForgetCmd(),
	)

	return cmd
}

// ── Interactive TUI entry point ─────────────────────────────────────

func runBluetoothInteractive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	model := bttable.NewModel(nil).WithHandler(&btTUIHandler{ctx: ctx, agent: conn.AgentService})
	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("bluetooth TUI: %w", err)
	}
	return nil
}

// btTUIHandler adapts the agent client to the bttable.Handler interface so the
// TUI can scan and execute operations inline and stay open between actions.
type btTUIHandler struct {
	ctx    context.Context
	agent  agentpb.WendyAgentServiceClient
	stream grpc.BidiStreamingClient[agentpb.ScanBluetoothPeripheralsRequest, agentpb.ScanBluetoothPeripheralsResponse]
}

// openScan (re)opens a scan stream and sends the scan request, mirroring the
// `list` command's error handling for unsupported (macOS beta) agents. Any
// prior stream is closed first so a rescan never leaves one half-open.
func (h *btTUIHandler) openScan() error {
	if h.stream != nil {
		_ = h.stream.CloseSend()
		h.stream = nil
	}
	stream, err := h.agent.ScanBluetoothPeripherals(h.ctx)
	if err != nil {
		return h.wrapScanErr(err)
	}
	if err := stream.Send(&agentpb.ScanBluetoothPeripheralsRequest{}); err != nil && err != io.EOF {
		return h.wrapScanErr(err)
	}
	if err := stream.CloseSend(); err != nil && err != io.EOF {
		return h.wrapScanErr(err)
	}
	h.stream = stream
	return nil
}

func (h *btTUIHandler) wrapScanErr(err error) error {
	if macErr := macOSBetaUnsupportedFeatureError(h.ctx, h.agent, err, "Bluetooth scanning"); macErr != nil {
		return macErr
	}
	return err
}

// recv reads the next scan event and maps it to a model message.
func (h *btTUIHandler) recv() tea.Msg {
	if h.stream == nil {
		return bttable.ScanDoneMsg{}
	}
	resp, err := h.stream.Recv()
	if err == io.EOF {
		return bttable.ScanDoneMsg{}
	}
	if err != nil {
		return bttable.ScanDoneMsg{Err: h.wrapScanErr(err)}
	}
	return bttable.ScanResultMsg{Peripherals: peripheralsFromProto(resp.GetDiscoveredDevices())}
}

func (h *btTUIHandler) StartScan() tea.Cmd {
	return func() tea.Msg {
		if err := h.openScan(); err != nil {
			return bttable.ScanDoneMsg{Err: err}
		}
		return h.recv()
	}
}

func (h *btTUIHandler) NextScanEvent() tea.Cmd {
	return func() tea.Msg { return h.recv() }
}

func (h *btTUIHandler) Connect(address string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, btConnectTimeout)
		defer cancel()
		resp, err := h.agent.ConnectBluetoothPeripheral(ctx, &agentpb.ConnectBluetoothPeripheralRequest{
			Address: address,
			Pair:    true,
			Trust:   true,
		})
		msg := bttable.OpResultMsg{Action: bttable.ActionConnect, Address: address, Err: err}
		// Older agents don't report the post-connect pairing state; the model
		// falls back to its optimistic assumption when PairedKnown is false.
		if err == nil && resp.Paired != nil {
			msg.PairedKnown = true
			msg.Paired = resp.GetPaired()
		}
		return msg
	}
}

func (h *btTUIHandler) Disconnect(address string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, btDisconnectTimeout)
		defer cancel()
		_, err := h.agent.DisconnectBluetoothPeripheral(ctx, &agentpb.DisconnectBluetoothPeripheralRequest{Address: address})
		return bttable.OpResultMsg{Action: bttable.ActionDisconnect, Address: address, Err: err}
	}
}

func (h *btTUIHandler) Forget(address string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(h.ctx, btForgetTimeout)
		defer cancel()
		_, err := h.agent.ForgetBluetoothPeripheral(ctx, &agentpb.ForgetBluetoothPeripheralRequest{Address: address})
		return bttable.OpResultMsg{Action: bttable.ActionForget, Address: address, Err: err}
	}
}

func peripheralsFromProto(devs []*agentpb.DiscoveredBluetoothPeripheral) []bttable.Peripheral {
	out := make([]bttable.Peripheral, 0, len(devs))
	for _, d := range devs {
		out = append(out, bttable.FromProto(d))
	}
	return out
}

func newBluetoothListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Scan for Bluetooth peripherals",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := conn.AgentService.ScanBluetoothPeripherals(ctx)
			if err != nil {
				if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Bluetooth scanning"); macErr != nil {
					return fmt.Errorf("starting Bluetooth scan: %w", macErr)
				}
				return fmt.Errorf("starting Bluetooth scan: %w", err)
			}

			// Send a scan request to start scanning. A server that rejects the
			// stream immediately may surface io.EOF here; continue to Recv so
			// grpc-go can expose the terminal status.
			if err := stream.Send(&agentpb.ScanBluetoothPeripheralsRequest{}); err != nil && err != io.EOF {
				if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Bluetooth scanning"); macErr != nil {
					return fmt.Errorf("sending scan request: %w", macErr)
				}
				return fmt.Errorf("sending scan request: %w", err)
			}
			if err := stream.CloseSend(); err != nil && err != io.EOF {
				if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Bluetooth scanning"); macErr != nil {
					return fmt.Errorf("closing send: %w", macErr)
				}
				return fmt.Errorf("closing send: %w", err)
			}

			var allDevices []*agentpb.DiscoveredBluetoothPeripheral
			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					if macErr := macOSBetaUnsupportedFeatureError(ctx, conn.AgentService, err, "Bluetooth scanning"); macErr != nil {
						return fmt.Errorf("receiving Bluetooth scan result: %w", macErr)
					}
					return fmt.Errorf("receiving Bluetooth scan result: %w", err)
				}
				allDevices = append(allDevices, resp.GetDiscoveredDevices()...)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(allDevices, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			if len(allDevices) == 0 {
				cliNotice("No Bluetooth devices found.")
				return nil
			}

			headers := []string{"Name", "Address", "RSSI", "Type", "Paired", "Connected", "Link timeout"}
			var rows [][]string
			for _, d := range allDevices {
				paired := ""
				if d.GetPaired() {
					paired = "yes"
				}
				connected := ""
				if d.GetConnected() {
					connected = "yes"
				}
				rows = append(rows, []string{
					d.GetName(),
					d.GetAddress(),
					fmt.Sprintf("%d", d.GetRssi()),
					d.GetDeviceType(),
					paired,
					connected,
					formatLinkTimeout(d),
				})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

func newBluetoothConnectCmd() *cobra.Command {
	var pair bool
	var trust bool

	cmd := &cobra.Command{
		Use:   "connect <address>",
		Short: "Connect to a Bluetooth peripheral",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.AgentService.ConnectBluetoothPeripheral(ctx, &agentpb.ConnectBluetoothPeripheralRequest{
				Address: args[0],
				Pair:    pair,
				Trust:   trust,
			})
			if err != nil {
				return fmt.Errorf("connecting to Bluetooth device: %w", err)
			}

			if pair && resp.Paired != nil && !resp.GetPaired() {
				cliSuccess("Connected to %s (not paired — the device accepted the connection without pairing)", args[0])
			} else {
				cliSuccess("Connected to %s", args[0])
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&pair, "pair", true, "Pair with the device")
	cmd.Flags().BoolVar(&trust, "trust", true, "Trust the device")

	return cmd
}

func newBluetoothDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect <address>",
		Short: "Disconnect a Bluetooth peripheral",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			_, err = conn.AgentService.DisconnectBluetoothPeripheral(ctx, &agentpb.DisconnectBluetoothPeripheralRequest{
				Address: args[0],
			})
			if err != nil {
				return fmt.Errorf("disconnecting Bluetooth device: %w", err)
			}

			cliSuccess("Disconnected from %s", args[0])
			return nil
		},
	}
}

func newBluetoothForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <address>",
		Short: "Forget a paired Bluetooth peripheral",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			_, err = conn.AgentService.ForgetBluetoothPeripheral(ctx, &agentpb.ForgetBluetoothPeripheralRequest{
				Address: args[0],
			})
			if err != nil {
				return fmt.Errorf("forgetting Bluetooth device: %w", err)
			}

			cliSuccess("Forgot device %s", args[0])
			return nil
		},
	}
}
