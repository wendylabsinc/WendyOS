package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/encoding/protojson"
)

type liteNetProvider interface {
	NetStatus(context.Context, models.ExternalDevice) (*litepb.WendyComNetStatus, error)
	ConfigureNet(context.Context, models.ExternalDevice, func(*litepb.WendyComNetConfig) error) (*litepb.WendyComNetStatus, error)
}

var liteNetLimitFlags = []string{"max-links", "ble-links", "wifi-links", "nan-links"}

func inspectLiteNet(cmd *cobra.Command, target *SelectedDevice) error {
	provider, ok := target.Provider.(liteNetProvider)
	if !ok {
		return fmt.Errorf("selected provider does not support Wendy Net settings")
	}
	status, err := provider.NetStatus(cmd.Context(), *target.External)
	if err != nil {
		return err
	}
	return printLiteNetStatus(cmd, status)
}

func configureLiteNet(cmd *cobra.Command, target *SelectedDevice) error {
	provider, ok := target.Provider.(liteNetProvider)
	if !ok {
		return fmt.Errorf("selected provider does not support Wendy Net settings")
	}
	for _, name := range []string{"ethernet", "roam", "share-uplink"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s is not supported by Wendy Lite", name)
		}
	}
	changed := false
	for _, name := range append([]string{"participate", "ble", "infrastructure-wifi", "nan"}, liteNetLimitFlags...) {
		changed = changed || cmd.Flags().Changed(name)
	}
	if !changed {
		return fmt.Errorf("specify --participate, --ble, --infrastructure-wifi, --nan or a link limit; omitted settings are preserved")
	}
	status, err := provider.ConfigureNet(cmd.Context(), *target.External, func(config *litepb.WendyComNetConfig) error {
		return updateLiteNetConfig(cmd, config)
	})
	if err != nil {
		return err
	}
	return printLiteNetStatus(cmd, status)
}

func updateLiteNetConfig(cmd *cobra.Command, config *litepb.WendyComNetConfig) error {
	for _, flag := range []struct {
		name string
		to   *bool
	}{{"participate", &config.Enabled}, {"ble", &config.Ble}, {"infrastructure-wifi", &config.InfrastructureWifi}, {"nan", &config.Nan}} {
		if cmd.Flags().Changed(flag.name) {
			value, err := cmd.Flags().GetBool(flag.name)
			if err != nil {
				return err
			}
			*flag.to = value
		}
	}
	for _, flag := range []struct {
		name string
		to   *uint32
	}{{"max-links", &config.MaxLinks}, {"ble-links", &config.BleLinks}, {"wifi-links", &config.WifiLinks}, {"nan-links", &config.NanLinks}} {
		if cmd.Flags().Changed(flag.name) {
			value, err := cmd.Flags().GetUint32(flag.name)
			if err != nil {
				return err
			}
			*flag.to = value
		}
	}
	return nil
}

func printLiteNetStatus(cmd *cobra.Command, status *litepb.WendyComNetStatus) error {
	if status == nil {
		return fmt.Errorf("device returned no Wendy Net status")
	}
	if status.GetRestarting() {
		fmt.Fprintln(cmd.ErrOrStderr(), "Wendy Net settings applied; the device is restarting to change its carrier. Reconnect after it boots.")
	}
	if jsonOutput {
		body, err := (protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}).Marshal(status)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(body))
		return err
	}
	config := status.GetDesired()
	cmd.Printf("Participate: %t\nBLE: %t\nInfrastructure Wi-Fi: %t\nNAN: %t\nCarrier limit: %d\nBLE limit: %d\nWi-Fi limit: %d\nNAN limit: %d\nState: %s\nActive carriers: %d\n",
		config.GetEnabled(), config.GetBle(), config.GetInfrastructureWifi(), config.GetNan(), config.GetMaxLinks(),
		config.GetBleLinks(), config.GetWifiLinks(), config.GetNanLinks(),
		strings.ToLower(strings.TrimPrefix(status.GetState().String(), "WENDY_COM_NET_")), status.GetActiveLinks())
	return nil
}
