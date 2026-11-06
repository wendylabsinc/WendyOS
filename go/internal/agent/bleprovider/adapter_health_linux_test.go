//go:build linux

package bleprovider

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

type adapterHealthObjects struct {
	mu    sync.Mutex
	calls int
}

func (o *adapterHealthObjects) GetManagedObjects() (managedObjects, *dbus.Error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	return managedObjects{
		"/org/bluez/hci0": {adapterInterface: {"Powered": dbus.MakeVariant(false)}},
		"/org/bluez/hci1": {adapterInterface: {"Powered": dbus.MakeVariant(o.calls == 1)}},
	}, nil
}

// AQ's controller went down after setup while GetManagedObjects kept succeeding.
// Exercise the real scan loop and bus rather than just a property parser: it
// must release the failed provider to its supervisor, even with no candidates.
func TestBLEScanLoopExitsWhenSelectedAdapterPowersOff(t *testing.T) {
	testScanLoopPowerLoss(t, false)
}

func TestBLEScanLoopChecksAdapterWhileDiscoveryPaused(t *testing.T) {
	testScanLoopPowerLoss(t, true)
}

func testScanLoopPowerLoss(t *testing.T, paused bool) {
	t.Helper()
	_, connect := privateAdvertisementBus(t)
	bluez, client := connect(), connect()
	objects := &adapterHealthObjects{}
	if err := bluez.Export(objects, "/", "org.freedesktop.DBus.ObjectManager"); err != nil {
		t.Fatal(err)
	}
	if _, err := bluez.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	adapter := dbus.ObjectPath("/org/bluez/hci1")
	r := &runtime{
		cfg:         Config{Logger: zap.NewNop()},
		owner:       ":test",
		ownerLookup: func(context.Context, *dbus.Conn) (string, error) { return ":test", nil },
		freshness:   newAdvertisementFreshness(adapter, ServiceUUID(testTenant), "field", lowFixture),
	}
	r.scanPaused = paused
	r.lastScanToggle = time.Now() // Keep the existing ten-second discovery dwell in force.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	err := r.scanLoop(ctx, client, adapter)
	if err == nil || !strings.Contains(err.Error(), "powered off") || ctx.Err() != nil {
		t.Fatalf("powered-off adapter left provider running until cancellation: %v (context %v)", err, ctx.Err())
	}
	objects.mu.Lock()
	defer objects.mu.Unlock()
	if objects.calls != 2 {
		t.Fatalf("did not inspect the selected adapter's live transition: %d queries", objects.calls)
	}
}
