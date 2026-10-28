//go:build linux

package bleprovider

import (
	"context"
	"fmt"
	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"net"
	"testing"
	"time"
)

func TestNormalBusyAdvertisementReplacesReady(t *testing.T) {
	adapter := dbus.ObjectPath("/org/bluez/hci0")
	path := dbus.ObjectPath(string(adapter) + "/dev_AA_BB_CC_DD_EE_01")
	props := map[string]dbus.Variant{"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:01"), "AddressType": dbus.MakeVariant("public")}
	objects := managedObjects{path: {deviceInterface: props}}
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	f := newAdvertisementFreshness(adapter, ServiceUUID(testTenant), "field", selfFixture)
	at := time.Now()
	for i, caps := range []byte{3, 2, 3} {
		adv.Capabilities = caps
		raw, _ := adv.MarshalBinary()
		props["ServiceData"] = dbus.MakeVariant(map[string]dbus.Variant{ServiceUUID(testTenant): dbus.MakeVariant(raw)})
		f.observe(path, props, at.Add(time.Duration(i)*time.Millisecond))
		peers, _ := f.candidates(objects, at.Add(time.Duration(i)*time.Millisecond))
		want := 1
		if caps == 2 {
			want = 0
		}
		if len(peers) != want {
			t.Fatalf("caps%x candidates%d", caps, len(peers))
		}
		if !f.seen[path].advert.TimeRelayAvailable() {
			t.Fatal("busy lost time endpoint")
		}
	}
}
func TestNormalCapacityAdvertisementKeepsTime(t *testing.T) {
	policy := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 2})
	r := &runtime{cfg: Config{Policy: policy}, advertisementRefresh: make(chan struct{}, 1), timeSlots: make(chan struct{}, 2)}
	r.updateAdvertisementAdmission()
	if r.advertisement.Capabilities != 3 {
		t.Fatal("initial unavailable")
	}
	a := policy.Reserve("a", lowFixture, false)
	b := policy.Reserve("b", peerFixture, false)
	if a == nil || b == nil {
		t.Fatal("reservations")
	}
	r.updateAdvertisementAdmission()
	if r.advertisement.Capabilities != 2 {
		t.Fatal("full node still invites native")
	}
	if policy.Counts().Total != 2 {
		t.Fatal("advertisement retired a connection")
	}
	r.timeSlots <- struct{}{}
	r.timeSlots <- struct{}{}
	r.updateAdvertisementAdmission()
	if r.advertisement.Capabilities != 0 {
		t.Fatal("full time relay still advertised")
	}
	<-r.timeSlots
	<-r.timeSlots
	r.updateAdvertisementAdmission()
	if r.advertisement.Capabilities != CapabilityTimeRelay {
		t.Fatal("time capacity release not advertised")
	}

	a.Release()
	r.updateAdvertisementAdmission()
	if r.advertisement.Capabilities != 3 {
		t.Fatal("capacity release not advertised")
	}
	b.Release()
}

func TestNormalQueuedDialObservesLatestBusy(t *testing.T) {
	adapter := dbus.ObjectPath("/org/bluez/hci0")
	path := dbus.ObjectPath(string(adapter) + "/dev_AA_BB_CC_DD_EE_01")
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	f := newAdvertisementFreshness(adapter, ServiceUUID(testTenant), "field", selfFixture)
	f.observe(path, meshDeviceProps(ServiceUUID(testTenant), raw), time.Now())
	r := &runtime{freshness: f, dialSlot: make(chan struct{}, 1), dialCoC: func(context.Context, string, string, uint16) (net.Conn, error) {
		t.Error("busy candidate dialed")
		return nil, fmt.Errorf("unexpected dial")
	}}
	r.dialSlot <- struct{}{}
	done := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		done <- r.dialLink(ctx, candidate{path: path, hint: adv.DeviceHint, psm: adv.PSM, address: "AA:BB:CC:DD:EE:01", addressType: "public"})
	}()
	adv.Capabilities = CapabilityTimeRelay
	raw, _ = adv.MarshalBinary()
	f.observe(path, meshDeviceProps(ServiceUUID(testTenant), raw), time.Now())
	<-r.dialSlot
	select {
	case skipped := <-done:
		if !skipped {
			t.Fatal("busy was penalized as an attempted dial")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
