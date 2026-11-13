//go:build linux

package bleprovider

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/google/uuid"
)

func TestVersionedAdvertisementCandidatePolicies(t *testing.T) {
	for _, format := range []string{"binary", "text"} {
		for _, mode := range []string{"valid", "blocked", "malformed-policy", "wrong-adapter", "wrong-address", "wrong-address-type", "zero-prefix"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				adapter := dbus.ObjectPath("/org/bluez/hci0")
				if mode == "wrong-adapter" {
					adapter = "/org/bluez/hci1"
				}
				f := newAdvertisementFreshness(adapter, ServiceUUID(testTenant), "field", selfFixture)
				path, objects := reportObjects("AA_BB_CC_DD_EE_01")
				props := objects[path][deviceInterface]
				props["Blocked"] = dbus.MakeVariant(false)
				switch mode {
				case "blocked":
					props["Blocked"] = dbus.MakeVariant(true)
				case "malformed-policy":
					props["Blocked"] = dbus.MakeVariant("false")
				case "wrong-address":
					props["Address"] = dbus.MakeVariant("AA:BB:CC:DD:EE:02")
				case "wrong-address-type":
					props["AddressType"] = dbus.MakeVariant("random")
				}
				device := lowDevice
				if mode == "zero-prefix" {
					device = uuid.MustParse("00000000-1234-4000-8000-000000000001")
				}
				var data []byte
				if format == "binary" {
					advert, err := NewAdvertisement(device, "field", DefaultPSM)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := advert.MarshalBinary()
					if err != nil {
						t.Fatal(err)
					}
					data = adField(0x21, append(wireUUID(testTenant.UUID()), raw...))
				} else {
					tenant, name := nameFields(device, "field")
					data = append(bytes.Clone(tenant), name...)
				}
				now := time.Unix(1000, 0)
				f.observeReport("AA:BB:CC:DD:EE:01", "public", data, now)
				peers, _ := f.candidates(objects, now)
				want := mode == "valid" || mode == "zero-prefix"
				if (len(peers) == 1) != want {
					t.Fatalf("candidate policy: peers=%+v want admitted=%v", peers, want)
				}
				if want && peers[0].hint != deviceHint(device) {
					t.Fatal("device prefix changed")
				}
				if mode == "blocked" {
					props["Blocked"] = dbus.MakeVariant(false)
					peers, _ = f.candidates(objects, now.Add(time.Second))
					if len(peers) != 1 {
						t.Fatal("unblocking did not release a still-live observation")
					}
				}
			})
		}
	}
}

func TestVersionedCompleteObservationSaturationPreservesLivePeers(t *testing.T) {
	for _, format := range []string{"binary", "text"} {
		t.Run(format, func(t *testing.T) {
			f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
			now := time.Unix(1000, 0)
			observe := func(i int, at time.Time) {
				address := fmt.Sprintf("02:00:00:00:%02X:%02X", i>>8, i&255)
				var data []byte
				if format == "binary" {
					advert, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
					raw, _ := advert.MarshalBinary()
					data = adField(0x21, append(wireUUID(testTenant.UUID()), raw...))
				} else {
					tenant, name := nameFields(lowDevice, "field")
					data = append(bytes.Clone(tenant), name...)
				}
				f.observeReport(address, "public", data, at)
			}
			for i := 0; i < maxFreshAdvertisements; i++ {
				observe(i, now)
			}
			first := dbus.ObjectPath("/org/bluez/hci0/dev_02_00_00_00_00_00")
			observe(maxFreshAdvertisements, now.Add(time.Second))
			if len(f.seen) != maxFreshAdvertisements {
				t.Fatal("observation limit changed")
			}
			if _, ok := f.seen[first]; !ok {
				t.Fatal("new radio address evicted a live existing peer")
			}
			observe(maxFreshAdvertisements, now.Add(advertisementFreshnessTTL))
			if len(f.seen) != 1 {
				t.Fatalf("expired observations not reclaimed: %d", len(f.seen))
			}
		})
	}
}
