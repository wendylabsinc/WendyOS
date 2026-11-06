//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

type foreignProfileDevice struct{ disconnects atomic.Int32 }

func (d *foreignProfileDevice) Disconnect() *dbus.Error {
	d.disconnects.Add(1)
	return nil
}

func TestFailedOwnCoCDoesNotDisconnectOtherDeviceProfiles(t *testing.T) {
	for _, stage := range []string{"dial", "deadline", "TLS"} {
		t.Run(stage, func(t *testing.T) {
			_, connect := privateAdvertisementBus(t)
			bluez, provider := connect(), connect()
			const path = dbus.ObjectPath("/org/bluez/hci0/dev_02_00_00_00_00_01")
			foreign := &foreignProfileDevice{}
			if err := bluez.Export(foreign, path, deviceInterface); err != nil {
				t.Fatal(err)
			}
			if _, err := bluez.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil {
				t.Fatal(err)
			}
			certificate, _ := testTLSIdentity(t)
			r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture, Certificate: certificate}, Logger: zap.NewNop()},
				bus: provider, hciIndex: -1, dialSlot: make(chan struct{}, 1)}
			var owned *packetConn
			r.dialCoC = func(context.Context, string, string, uint16) (net.Conn, error) {
				if stage == "dial" {
					return nil, errors.New("own CoC refused")
				}
				if stage == "deadline" {
					return nil, context.DeadlineExceeded
				}
				peer, local := packetPair(t)
				_ = peer.Close() // Complete CoC setup, then fail TLS immediately.
				owned = local
				return local, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r.dialLink(ctx, candidate{hint: deviceHint(lowDevice), address: "02:00:00:00:00:01", addressType: "public", psm: DefaultPSM, path: path})
			if owned != nil && !owned.closed.Load() {
				t.Error("failed setup retained its own CoC")
			}
			if foreign.disconnects.Load() != 0 {
				t.Fatal("own failed CoC disconnected unrelated profiles through Device1.Disconnect")
			}
		})
	}
}
