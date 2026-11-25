package carriers

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const radioFreshness = 30 * time.Second

func recent(seen, now time.Time) bool {
	return !seen.IsZero() && !seen.After(now) && now.Sub(seen) <= radioFreshness
}

// ServiceData and explicit local-name fields are observations, not an OS-cached
// display name. A present native advertisement wins even if it now says busy;
// an older local name must not restore its withdrawn accepting bit.
func bleCandidates(devices []scan.BLEDeviceInfo, self meshid.Device, now time.Time) []Candidate {
	name := fmt.Sprintf("tenant:%s:default", self.Tenant())
	tenantService := scan.CanonicalUUID(bleprovider.ServiceUUID(self.Tenant()))
	out := make([]Candidate, 0, len(devices))
	for _, device := range devices {
		if device.Address == "" || !recent(device.LastSeen, now) {
			continue
		}
		var adv bleprovider.Advertisement
		var valid, present bool
		if recent(device.ServiceDataSeen, now) {
			for service, data := range device.ServiceData {
				if scan.CanonicalUUID(service) != tenantService {
					continue
				}
				present = true
				var err error
				adv, err = bleprovider.ParseAdvertisement(data, name)
				valid = err == nil
				break
			}
		}
		if !present && recent(device.AdvertisementNameSeen, now) {
			var err error
			adv, err = bleprovider.ParseLocalName(device.AdvertisementName, name)
			valid = err == nil
		}
		if !valid || !adv.NativeAccepting() {
			continue
		}
		out = append(out, Candidate{Transport: BLE, Address: device.Address, PSM: adv.PSM, Hint: adv.DeviceHint, NetworkHash: adv.NetworkHash, Capabilities: adv.Capabilities, Cost: bleprovider.LinkCost})
		if len(out) >= 256 {
			break
		}
	}
	return out
}
