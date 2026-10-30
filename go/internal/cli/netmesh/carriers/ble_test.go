package carriers

import (
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

func TestNativeBLEUsesFreshServiceBytesAndCapabilities(t *testing.T) {
	now := time.Now()
	self := testDevice(1)
	peer := testDevice(2)
	adv, err := bleprovider.NewAdvertisement(peer.UUID(), fmt.Sprintf("tenant:%s:default", self.Tenant()), 129)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := adv.MarshalBinary()
	name, _ := adv.MarshalLocalName()
	d := scan.BLEDeviceInfo{Address: "radio", LastSeen: now, ServiceDataSeen: now, ServiceData: map[string][]byte{self.Tenant().String(): raw}, Name: name}
	rows := bleCandidates([]scan.BLEDeviceInfo{d}, self, now)
	if len(rows) != 1 || rows[0].PSM != 129 || rows[0].Hint != adv.DeviceHint {
		t.Fatal("native hint fields lost")
	}
	if len(rows) != 1 || rows[0].Device.Tenant().IsValid() {
		t.Fatal("radio did not remain an identity-free hint")
	}
	adv.Capabilities = bleprovider.CapabilityTimeRelay
	busy, _ := adv.MarshalBinary()
	d.ServiceData[self.Tenant().String()] = busy
	d.AdvertisementName = name
	d.AdvertisementNameSeen = now
	if len(bleCandidates([]scan.BLEDeviceInfo{d}, self, now)) != 0 {
		t.Fatal("stale local name restored busy native endpoint")
	}
	d.ServiceData = nil
	d.ServiceDataSeen = time.Time{}
	d.AdvertisementNameSeen = time.Time{}
	if len(bleCandidates([]scan.BLEDeviceInfo{d}, self, now)) != 0 {
		t.Fatal("OS display name became a live advertisement")
	}
	d.AdvertisementNameSeen = now
	if len(bleCandidates([]scan.BLEDeviceInfo{d}, self, now)) != 1 {
		t.Fatal("explicit current local-name fallback lost")
	}
	if len(bleCandidates([]scan.BLEDeviceInfo{d}, self, now.Add(31*time.Second))) != 0 {
		t.Fatal("stale report stayed dialable")
	}
}
