package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newBluetoothCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "bluetooth",
		Aliases: []string{"bt"},
		Short:   "Manage Bluetooth on the target device",
	}

	cmd.AddCommand(
		newBluetoothListCmd(),
		newBluetoothConnectCmd(),
		newBluetoothDisconnectCmd(),
		newBluetoothForgetCmd(),
	)

	return cmd
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
				return fmt.Errorf("starting Bluetooth scan: %w", err)
			}

			// Send a scan request to start scanning.
			if err := stream.Send(&agentpb.ScanBluetoothPeripheralsRequest{}); err != nil {
				return fmt.Errorf("sending scan request: %w", err)
			}
			if err := stream.CloseSend(); err != nil {
				return fmt.Errorf("closing send: %w", err)
			}

			var allDevices []*agentpb.DiscoveredBluetoothPeripheral
			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
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
				fmt.Println("No Bluetooth devices found.")
				return nil
			}

			headers := []string{"Name", "Address", "RSSI", "Type", "Paired", "Connected"}
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
		Use:   "connect [address]",
		Short: "Connect to a Bluetooth peripheral",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			_, err = conn.AgentService.ConnectBluetoothPeripheral(ctx, &agentpb.ConnectBluetoothPeripheralRequest{
				Address: args[0],
				Pair:    pair,
				Trust:   trust,
			})
			if err != nil {
				return fmt.Errorf("connecting to Bluetooth device: %w", err)
			}

			fmt.Printf("Connected to %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().BoolVar(&pair, "pair", true, "Pair with the device")
	cmd.Flags().BoolVar(&trust, "trust", true, "Trust the device")

	return cmd
}

func newBluetoothDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect [address]",
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

			fmt.Printf("Disconnected from %s\n", args[0])
			return nil
		},
	}
}

func newBluetoothForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget [address]",
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

			fmt.Printf("Forgot device %s\n", args[0])
			return nil
		},
	}
}
