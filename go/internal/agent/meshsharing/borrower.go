package meshsharing

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// InternetConfig describes addresses assigned by an authenticated gateway.
// MTU reserves the 40-byte outer IPv6 header within the mesh's 1320-byte MTU.
type InternetConfig struct {
	Address4 string `json:"address4"`
	Address6 string `json:"address6,omitempty"`
	MTU      int    `json:"mtu"`
}

// Borrow negotiates on a caller-owned mesh socket. The returned control socket
// must remain open and run KeepLease for the lifetime of the borrowed routes.
func Borrow(ctx context.Context, credentials *localmesh.Credentials, gateway meshid.Device, raw net.Conn) (net.Conn, InternetConfig, error) {
	conn, lease, err := negotiateGatewayLease(ctx, credentials, gateway, raw)
	if err != nil {
		return nil, InternetConfig{}, err
	}
	config := InternetConfig{Address4: lease.address4().String(), MTU: 1280}
	if lease.IPv6 {
		config.Address6 = lease.address6().String()
	}
	return conn, config, nil
}

func (c InternetConfig) Valid() bool {
	a, err := netip.ParseAddr(c.Address4)
	if err != nil || !netip.MustParsePrefix(leaseIPv4Pool).Contains(a) || c.MTU != 1280 {
		return false
	}
	bytes := a.As4()
	lease := tunnelLease{Slot: uint16(bytes[2])<<8 | uint16(bytes[3])}
	return lease.valid() && (c.Address6 == "" || c.Address6 == lease.address6().String())
}

// KeepLease closes control on cancellation, expiry or heartbeat failure. The
// caller withdraws the packet path as soon as this returns.
func KeepLease(ctx context.Context, control net.Conn) error {
	defer control.Close()
	stop := context.AfterFunc(ctx, func() { control.Close() })
	defer stop()
	ticker := time.NewTicker(leaseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := control.SetDeadline(time.Now().Add(leaseHeartbeatInterval)); err != nil {
			return err
		}
		if err := writeControl(control, []byte{leaseHeartbeat}); err != nil {
			return err
		}
		var reply [1]byte
		if _, err := io.ReadFull(control, reply[:]); err != nil {
			return err
		}
		if reply[0] != leaseHeartbeat {
			return errors.New("invalid gateway heartbeat")
		}
	}
}
