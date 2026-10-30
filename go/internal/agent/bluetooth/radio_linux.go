//go:build linux

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/btradio"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (m *BlueZManager) BluetoothRadio(ctx context.Context) (*pb.BluetoothRadioStatus, error) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	defer bus.Close()
	objects, err := getManagedObjects(ctx, bus)
	if err != nil {
		return nil, err
	}
	return bluetoothRadioStatus(objects)
}

func bluetoothRadioStatus(objects managedObjects) (*pb.BluetoothRadioStatus, error) {
	adapter, err := resolveAdapterPath(objects)
	if err != nil {
		return nil, err
	}
	if os.Getenv("WENDY_BT_ADAPTER") == "" {
		if capable := findAdvertisingAdapter(objects); capable != "" {
			adapter = capable
		}
	}
	props, available := objects[dbus.ObjectPath(adapter)][advManagerIface]
	features, _ := props["SupportedFeatures"].Value().([]string)
	caps, _ := props["SupportedCapabilities"].Value().(map[string]dbus.Variant)
	power, policyErr := btradio.Desired()
	s := &pb.BluetoothRadioStatus{Adapter: adapter, Available: available, SupportedMeshModes: []string{"normal"}, MeshMode: "normal", Advertising: power, Connections: &pb.BluetoothPowerSetting{Mode: "default"}, Advertisements: btradio.Reports(),
		CodedUnavailableReason:       "BlueZ cannot require S2 or S8 for advertising; complete coded advertising + data profiles are unavailable",
		ConnectionsUnavailableReason: "BlueZ provides no portable connected-link TX power setter; the controller manages connected-link power"}
	if !available {
		s.SupportedMeshModes = nil
		s.Error = "Selected Bluetooth adapter does not support LE advertising"
	}
	if policyErr != nil {
		s.Error = policyErr.Error()
	}
	for _, advert := range s.Advertisements {
		if advert.Error != "" {
			if s.Error != "" {
				s.Error += "; "
			}
			s.Error += advert.Name + ": " + advert.Error
		}
	}
	min, minOK := caps["MinTxPower"].Value().(int16)
	max, maxOK := caps["MaxTxPower"].Value().(int16)
	minimum, maximum, validRange := btradio.PowerBounds(int32(min), int32(max))
	minOK = minOK && validRange
	maxOK = maxOK && validRange
	s.AdvertisingSetSupported = available && slices.Contains(features, "CanSetTxPower") && minOK && maxOK
	if minOK {
		v := minimum
		s.AdvertisingMinDbm = &v
	}
	if maxOK {
		v := maximum
		s.AdvertisingMaxDbm = &v
	}
	secondary, _ := props["SupportedSecondaryChannels"].Value().([]string)
	if !slices.Contains(secondary, "Coded") {
		s.CodedUnavailableReason = "Selected controller/BlueZ does not support coded advertising"
	}
	return s, nil
}
func (m *BlueZManager) ConfigureBluetoothRadio(ctx context.Context, req *pb.ConfigureBluetoothRadioRequest) (*pb.BluetoothRadioStatus, error) {
	s, err := m.BluetoothRadio(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "radio configuration is missing")
	}
	if !s.Available {
		return nil, status.Error(codes.FailedPrecondition, s.Error)
	}
	switch req.MeshMode {
	case "", "normal":
	case "coded-s2", "coded-s8":
		return nil, status.Error(codes.FailedPrecondition, s.CodedUnavailableReason)
	default:
		return nil, status.Error(codes.InvalidArgument, "mesh mode must be normal, coded-s2 or coded-s8")
	}
	if req.Connections != nil {
		if err := btradio.Validate(req.Connections); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if req.Connections.Mode != "default" {
			return nil, status.Error(codes.FailedPrecondition, s.ConnectionsUnavailableReason)
		}
	}
	if req.Advertising != nil {
		if err := btradio.Validate(req.Advertising); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if req.Advertising.Mode != "default" {
			if !s.AdvertisingSetSupported {
				return nil, status.Error(codes.FailedPrecondition, "Selected controller/BlueZ does not support setting advertising TX power")
			}
			if req.Advertising.Mode == "dbm" && (req.Advertising.Dbm < s.GetAdvertisingMinDbm() || req.Advertising.Dbm > s.GetAdvertisingMaxDbm()) {
				return nil, status.Errorf(codes.OutOfRange, "controller advertising TX power range is %d to %d dBm", s.GetAdvertisingMinDbm(), s.GetAdvertisingMaxDbm())
			}
		}
		if err := btradio.Set(ctx, req.Advertising); err != nil {
			var applied *btradio.ApplyError
			if errors.As(err, &applied) {
				return nil, status.Error(codes.Internal, err.Error())
			}
			return nil, status.Error(codes.Internal, fmt.Sprintf("Saving Bluetooth policy failed: %v", err))
		}
	}
	return m.BluetoothRadio(ctx)
}
