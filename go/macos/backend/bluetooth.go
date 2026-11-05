package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (b *Backend) BluetoothScan(session uint64) ([]*pb.DiscoveredBluetoothPeripheral, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return scanBluetooth(ctx, pb.NewWendyBluetoothServiceClient(conn.Conn), 5*time.Second)
}
func scanBluetooth(parent context.Context, client pb.WendyBluetoothServiceClient, duration time.Duration) ([]*pb.DiscoveredBluetoothPeripheral, error) {
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	stream, err := client.ScanBluetoothPeripherals(ctx)
	if err != nil {
		return nil, err
	}
	if err = stream.Send(&pb.ScanBluetoothPeripheralsRequest{}); err != nil && err != io.EOF {
		return nil, err
	}
	if err = stream.CloseSend(); err != nil && err != io.EOF {
		return nil, err
	}
	devices := map[string]*pb.DiscoveredBluetoothPeripheral{}
	received := false
	for {
		response, receiveErr := stream.Recv()
		if receiveErr != nil {
			if receiveErr == io.EOF || bluetoothScanDeadlineReached(parent, ctx, received, receiveErr, time.Now()) {
				break
			}
			return nil, receiveErr
		}
		received = true
		for _, device := range response.GetDiscoveredDevices() {
			if device.GetAddress() != "" {
				devices[device.GetAddress()] = device
			}
		}
	}
	result := make([]*pb.DiscoveredBluetoothPeripheral, 0, len(devices))
	for _, device := range devices {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].GetConnected() != result[j].GetConnected() {
			return result[i].GetConnected()
		}
		return result[i].GetName()+result[i].GetAddress() < result[j].GetName()+result[j].GetAddress()
	})
	return result, nil
}

// gRPC can classify an expired deadline before context's timer callback has
// updated Err. Compare deadlines directly so callback scheduling cannot turn a
// completed scan into an error. Caller cancellation/expiry and remote failures
// remain errors, and at least one scan response must have arrived.
func bluetoothScanDeadlineReached(parent, scan context.Context, received bool, err error, now time.Time) bool {
	if !received || parent.Err() != nil || status.Code(err) != codes.DeadlineExceeded {
		return false
	}
	deadline, ok := scan.Deadline()
	if !ok || now.Before(deadline) {
		return false
	}
	callerDeadline, ok := parent.Deadline()
	return !ok || now.Before(callerDeadline)
}

// Actions are explicit: connect, pair and connect, disconnect, or forget.
func (b *Backend) BluetoothAction(session uint64, address string, action int32) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if _, err = net.ParseMAC(address); err != nil {
		return errors.New("invalid Bluetooth device address")
	}
	client := pb.NewWendyBluetoothServiceClient(conn.Conn)
	switch action {
	case 0, 1:
		_, err = client.ConnectBluetoothPeripheral(ctx, &pb.ConnectBluetoothPeripheralRequest{Address: address, Pair: action == 1})
	case 2:
		_, err = client.DisconnectBluetoothPeripheral(ctx, &pb.DisconnectBluetoothPeripheralRequest{Address: address})
	case 3:
		_, err = client.ForgetBluetoothPeripheral(ctx, &pb.ForgetBluetoothPeripheralRequest{Address: address})
	default:
		return errors.New("unknown Bluetooth action")
	}
	return err
}
