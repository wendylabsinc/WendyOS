package liteclient

import (
	"fmt"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func (c *WendyLiteClient) GetBluetoothPower(timeout time.Duration) (*pb.WendyComBluetoothPowerStatus, error) {
	response, err := c.sendCommand(&pb.WendyComCommand{RequestId: c.requestIdGen.Add(1), Params: &pb.WendyComCommand_BluetoothPowerGet{BluetoothPowerGet: &pb.WendyComBluetoothPowerGetParams{}}}, timeout)
	if err != nil {
		return nil, err
	}
	if err := resultToError(response.Result); err != nil {
		return nil, fmt.Errorf("Bluetooth power status unavailable: %w", err)
	}
	status := response.GetBluetoothPower()
	if status == nil {
		return nil, fmt.Errorf("device omitted Bluetooth power status")
	}
	return status, nil
}

// SetBluetoothPower changes only supplied settings; omitted fields are preserved on the device.
func (c *WendyLiteClient) SetBluetoothPower(config *pb.WendyComBluetoothPowerConfig, timeout time.Duration) (*pb.WendyComBluetoothPowerStatus, error) {
	response, err := c.sendCommand(&pb.WendyComCommand{RequestId: c.requestIdGen.Add(1), Params: &pb.WendyComCommand_BluetoothPowerSet{BluetoothPowerSet: &pb.WendyComBluetoothPowerSetParams{Config: config}}}, timeout)
	if err != nil {
		return nil, err
	}
	status := response.GetBluetoothPower()
	if err := resultToError(response.Result); err != nil {
		if status != nil && status.Error != "" {
			return status, fmt.Errorf("Bluetooth power: %s", status.Error)
		}
		return status, fmt.Errorf("Bluetooth power change rejected: %w", err)
	}
	if status == nil {
		return nil, fmt.Errorf("device omitted Bluetooth power status")
	}
	return status, nil
}
