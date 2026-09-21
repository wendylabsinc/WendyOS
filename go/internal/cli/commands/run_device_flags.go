package commands

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
)

// pflag needs a nonempty NoOptDefVal to make a value optional. Whitespace
// cannot name a device and becomes the empty picker request after trimming.
const optionalDevicePickerValue = " "

func init() {
	// Show the optional device syntax without exposing pflag's picker sentinel.
	cobra.AddTemplateFunc("optionalRunDeviceUsage", func(usage string) string {
		return strings.ReplaceAll(usage, `string[=" "]`, "[=DEVICE]   ")
	})
}

// Accept the existing --build-host DEVICE spelling as well as =DEVICE.
// When both optional flags are bare, require = to avoid assigning a device
// to the wrong role. Arguments after -- never select a device.
func optionalRunDeviceArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	var bare []string
	var changed bool
	for _, name := range []string{"hil", "build-host"} {
		if cmd.Flags().Changed(name) {
			changed = true
			value, _ := cmd.Flags().GetString(name)
			if strings.TrimSpace(value) == "" {
				bare = append(bare, name)
			}
		}
	}
	if !changed {
		if dash := cmd.ArgsLenAtDash(); dash >= 0 {
			return cobra.NoArgs(cmd, args[:dash])
		}
		return cobra.NoArgs(cmd, args)
	}
	if len(args) != 1 || len(bare) != 1 || cmd.ArgsLenAtDash() >= 0 {
		return fmt.Errorf("unexpected device arguments; use --hil=DEVICE and --build-host=DEVICE to specify devices")
	}
	return cmd.Flags().Set(bare[0], args[0])
}

var pickRunBuildHostDevice = func(ctx context.Context) (*SelectedDevice, error) {
	excluded := map[string]bool{}
	for _, provider := range providers.AvailableProviders() {
		excluded[provider.Key()] = true
	}
	return pickDevice(ctx, excluded, false, true, true)
}

func selectRunBuildHost(ctx context.Context, yes bool) (string, error) {
	if yes || !isInteractiveTerminal() {
		return "", fmt.Errorf("--build-host needs an interactive device picker; use --build-host=DEVICE for non-interactive runs")
	}
	selected, err := pickRunBuildHostDevice(withDevicePickerPurpose(ctx, buildHostPicker))
	if err != nil {
		return "", err
	}
	if selected == nil {
		return "", fmt.Errorf("no build host selected")
	}
	defer selected.Close()
	return selectedBuildHostName(selected)
}

func selectedBuildHostName(selected *SelectedDevice) (string, error) {
	if selected.Agent == nil || selected.Agent.SimulatorName != "" {
		return "", fmt.Errorf("a build host must be a WendyOS device with an agent")
	}
	// A cloud tunnel's loopback address stops working when selection closes.
	// Retain the full cloud selector so deployment reconnects to that asset.
	if selected.DefaultSelector != "" {
		return selected.DefaultSelector, nil
	}
	// Keep the LAN pin's hostname and the port that answered discovery.
	if selected.PinKey != "" {
		if _, port, err := net.SplitHostPort(selected.Agent.Addr); err == nil {
			return net.JoinHostPort(selected.PinKey, port), nil
		}
		return selected.PinKey, nil
	}
	if selected.Agent.Addr != "" {
		return selected.Agent.Addr, nil
	}
	return "", fmt.Errorf("selected build host has no reconnectable address")
}
