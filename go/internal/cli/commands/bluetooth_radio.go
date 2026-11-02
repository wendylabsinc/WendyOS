package commands

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type liteBluetoothPowerProvider interface {
	BluetoothPower(context.Context, models.ExternalDevice, *lite.WendyComBluetoothPowerConfig) (*lite.WendyComBluetoothPowerStatus, error)
}

func parseBluetoothPower(value string) (*pb.BluetoothPowerSetting, error) {
	if value == "default" || value == "maximum" {
		return &pb.BluetoothPowerSetting{Mode: value}, nil
	}
	dbm, err := strconv.ParseInt(value, 10, 32)
	if err != nil || dbm < -127 || dbm > 20 {
		return nil, fmt.Errorf("power must be default, maximum, or a dBm value between -127 and 20")
	}
	return &pb.BluetoothPowerSetting{Mode: "dbm", Dbm: int32(dbm)}, nil
}
func litePower(s *pb.BluetoothPowerSetting) *lite.WendyComBluetoothPowerSetting {
	mode := lite.WendyComBluetoothPowerMode_WENDY_COM_BT_POWER_DEFAULT
	if s.Mode == "maximum" {
		mode = lite.WendyComBluetoothPowerMode_WENDY_COM_BT_POWER_MAXIMUM
	}
	if s.Mode == "dbm" {
		mode = lite.WendyComBluetoothPowerMode_WENDY_COM_BT_POWER_DBM
	}
	return &lite.WendyComBluetoothPowerSetting{Mode: mode, Dbm: s.Dbm}
}
func newBluetoothPowerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "power", Short: "Inspect or set device-wide Bluetooth transmission power", Args: cobra.NoArgs}
	cmd.Flags().String("advertising", "", "Advertising power: default, maximum, or dBm")
	cmd.Flags().String("connections", "", "Connected-link power: default, maximum, or dBm")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
		defer cancel()
		var err error
		var adv, conn *pb.BluetoothPowerSetting
		for _, v := range []struct {
			name string
			to   **pb.BluetoothPowerSetting
		}{{"advertising", &adv}, {"connections", &conn}} {
			if cmd.Flags().Changed(v.name) {
				value, _ := cmd.Flags().GetString(v.name)
				*v.to, err = parseBluetoothPower(value)
				if err != nil {
					return err
				}
			}
		}
		target, err := resolveTarget(ctx, SuppressUpdateCheck())
		if err != nil {
			return err
		}
		defer target.Close()
		if target.External != nil {
			provider, ok := target.Provider.(liteBluetoothPowerProvider)
			if !ok {
				return fmt.Errorf("selected device does not support Bluetooth power management")
			}
			var update *lite.WendyComBluetoothPowerConfig
			if adv != nil || conn != nil {
				update = &lite.WendyComBluetoothPowerConfig{}
				if adv != nil {
					update.Advertising = litePower(adv)
				}
				if conn != nil {
					update.Connections = litePower(conn)
				}
			}
			s, err := provider.BluetoothPower(ctx, *target.External, update)
			if err != nil {
				return err
			}
			return printBluetoothRadio(cmd, s)
		}
		if target.Agent == nil {
			return fmt.Errorf("selected device has no management connection")
		}
		client := pb.NewWendyBluetoothServiceClient(target.Agent.Conn)
		var s *pb.BluetoothRadioStatus
		if adv == nil && conn == nil {
			s, err = client.GetBluetoothRadio(ctx, &pb.GetBluetoothRadioRequest{})
		} else {
			s, err = client.ConfigureBluetoothRadio(ctx, &pb.ConfigureBluetoothRadioRequest{Advertising: adv, Connections: conn})
		}
		if err != nil {
			return err
		}
		return printBluetoothRadio(cmd, s)
	}
	return cmd
}
func newBluetoothModeCmd() *cobra.Command {
	return &cobra.Command{Use: "mode [normal|coded-s2|coded-s8]", Short: "Inspect or choose the mesh advertising and connected data PHY", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
		defer cancel()
		if len(args) > 0 && args[0] != "normal" && args[0] != "coded-s2" && args[0] != "coded-s8" {
			return fmt.Errorf("mode must be normal, coded-s2 or coded-s8")
		}
		target, err := resolveTarget(ctx, SuppressUpdateCheck())
		if err != nil {
			return err
		}
		defer target.Close()
		if target.External != nil {
			provider, ok := target.Provider.(liteNetProvider)
			if !ok {
				return fmt.Errorf("selected device does not support Bluetooth mode management")
			}
			var s *lite.WendyComNetStatus
			if len(args) == 0 {
				s, err = provider.NetStatus(ctx, *target.External)
			} else {
				s, err = provider.ConfigureNet(ctx, *target.External, func(c *lite.WendyComNetConfig) error {
					switch args[0] {
					case "normal":
						c.BleProfile = 0
					case "coded-s2":
						c.BleProfile = 1
					case "coded-s8":
						c.BleProfile = 2
					}
					return nil
				})
			}
			if err != nil {
				return err
			}
			return printLiteNetStatus(cmd, s)
		}
		if target.Agent == nil {
			return fmt.Errorf("selected device has no management connection")
		}
		client := pb.NewWendyBluetoothServiceClient(target.Agent.Conn)
		var s *pb.BluetoothRadioStatus
		if len(args) == 0 {
			s, err = client.GetBluetoothRadio(ctx, &pb.GetBluetoothRadioRequest{})
		} else {
			s, err = client.ConfigureBluetoothRadio(ctx, &pb.ConfigureBluetoothRadioRequest{MeshMode: args[0]})
		}
		if err != nil {
			return err
		}
		return printBluetoothRadio(cmd, s)
	}}
}
func printBluetoothRadio(cmd *cobra.Command, s proto.Message) error {
	data, err := (protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}).Marshal(s)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}
