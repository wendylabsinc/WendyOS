//go:build linux

package bluetooth

import (
	"context"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// StartBLESetup advertises only while enrollment is available. The distinct
// service/PSM cannot reach management operations or alter an enrolled device.
func StartBLESetup(ctx context.Context, logger *zap.Logger, d *SetupDispatcher) {
	maintainBLESetup(ctx, 5*time.Second, d.Available, func(ctx context.Context) error {
		return serveBLESetup(ctx, logger, d)
	}, func(err error) { logger.Debug("BLE setup unavailable; will retry", zap.Error(err)) })
}

func serveBLESetup(ctx context.Context, logger *zap.Logger, d *SetupDispatcher) error {
	setupCtx, cancel := context.WithCancel(ctx)
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, unix.BTPROTO_L2CAP)
	if err != nil {
		cancel()
		return fmt.Errorf("BLE setup socket: %w", err)
	}
	if err = unix.Bind(fd, &unix.SockaddrL2{PSM: SetupPSM, AddrType: unix.BDADDR_LE_PUBLIC}); err == nil {
		err = unix.Listen(fd, 4)
	}
	if err != nil {
		unix.Close(fd)
		cancel()
		return fmt.Errorf("BLE setup listener: %w", err)
	}
	if err = startServiceAdvertising(setupCtx, logger, SetupServiceUUID, dbus.ObjectPath("/org/wendy/setupAdvertisement")); err != nil {
		unix.Close(fd)
		cancel()
		return fmt.Errorf("BLE setup advertisement: %w", err)
	}
	defer cancel()
	defer unix.Close(fd)
	slots := make(chan struct{}, 4)
	for setupCtx.Err() == nil && d.Available(setupCtx) {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 1000)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		client, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			continue
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); serveSetupConn(ctx, client, d) }()
		default:
			unix.Close(client)
		}
	}
	return nil
}

func serveSetupConn(ctx context.Context, fd int, d *SetupDispatcher) {
	conn := &l2capStreamConn{fd: fd}
	defer conn.Close()
	// Bound stalled peers and setup operations. Existing connections get their
	// final enrollment response before the advertisement disappears.
	deadline := time.Now().Add(5 * time.Minute)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	read := func() (*agentpb.BluetoothCommand, error) {
		data, err := readMessageFromConn(conn)
		if err != nil {
			return nil, err
		}
		cmd := new(agentpb.BluetoothCommand)
		err = proto.Unmarshal(data, cmd)
		return cmd, err
	}
	write := func(r *agentpb.BluetoothResponse) error {
		data, err := proto.Marshal(r)
		if err != nil {
			return err
		}
		return writeFrameToConn(conn, data)
	}
	hello, err := read()
	if err != nil || hello.GetSetupHello() == nil {
		return
	}
	cipher, public, err := newSetupCipher(hello.GetSetupHello().GetPublicKey())
	if err != nil {
		return
	}
	if write(&agentpb.BluetoothResponse{Response: &agentpb.BluetoothResponse_SetupHello{SetupHello: &agentpb.SetupHello{PublicKey: public}}}) != nil {
		return
	}
	for ctx.Err() == nil && time.Now().Before(deadline) {
		_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
		outer, err := read()
		if err != nil || outer.GetSetupSealed() == nil {
			return
		}
		plain, err := cipher.open(outer.GetSetupSealed().GetData())
		if err != nil {
			return
		}
		cmd := new(agentpb.BluetoothCommand)
		err = proto.Unmarshal(plain, cmd)
		clear(plain)
		if err != nil {
			return
		}
		requestCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		response := d.Dispatch(requestCtx, cmd)
		cancel()
		data, err := proto.Marshal(response)
		if err != nil {
			return
		}
		sealed := cipher.seal(data)
		clear(data)
		if write(&agentpb.BluetoothResponse{Response: &agentpb.BluetoothResponse_SetupSealed{SetupSealed: &agentpb.SetupSealed{Data: sealed}}}) != nil {
			return
		}
		if !d.Available(ctx) {
			return
		}
	}
}
