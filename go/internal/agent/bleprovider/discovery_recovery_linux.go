//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// Thor's controller stopped scanning after a successful LE connection
	// while BlueZ still reported Discovering=true. Give ordinary advertising
	// gaps time to recover before recycling our discovery session.
	discoverySilenceLimit  = 45 * time.Second
	discoveryRestartPeriod = 90 * time.Second
)

type discoveryRecovery struct {
	lastMeshSignal time.Time
	lastRestart    time.Time
}

func (d *discoveryRecovery) observed(at time.Time) {
	d.lastMeshSignal = at
}

func (d *discoveryRecovery) restartDue(now time.Time, missingPeers bool) bool {
	if !missingPeers || d.lastMeshSignal.IsZero() || now.Before(d.lastMeshSignal) ||
		now.Sub(d.lastMeshSignal) < discoverySilenceLimit ||
		(!d.lastRestart.IsZero() && now.Sub(d.lastRestart) < discoveryRestartPeriod) {
		return false
	}
	d.lastRestart = now // Bound retries even when BlueZ rejects a restart.
	return true
}

func meshDiscoveryFilter() map[string]dbus.Variant {
	return map[string]dbus.Variant{"Transport": dbus.MakeVariant("le"), "DuplicateData": dbus.MakeVariant(true)}
}

// BlueZ owns one discovery session per D-Bus client. These calls use the
// existing agent bus connection, so StopDiscovery releases only our session;
// other clients and existing ACLs are not disconnected.
func restartDiscoverySession(ctx context.Context, call func(context.Context, string, ...any) error) error {
	stopErr := call(ctx, adapterInterface+".StopDiscovery")
	filterErr := call(ctx, adapterInterface+".SetDiscoveryFilter", meshDiscoveryFilter())
	// Attempt to restore our session even if BlueZ reported an inconsistent
	// StopDiscovery or rejected the filter update.
	startErr := call(ctx, adapterInterface+".StartDiscovery")
	return errors.Join(wrapDiscoveryError("stop", stopErr), wrapDiscoveryError("filter", filterErr), wrapDiscoveryError("start", startErr))
}

func wrapDiscoveryError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s BLE discovery: %w", stage, err)
}

func restartBlueZDiscovery(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath) error {
	object := bus.Object(bluezName, adapter)
	return restartDiscoverySession(ctx, func(ctx context.Context, method string, args ...any) error {
		return object.CallWithContext(ctx, method, 0, args...).Err
	})
}
