package bleprovider

import (
	"bytes"
	"github.com/google/uuid"
	"testing"
)

func TestNormalV1AdvertisementCapabilities(t *testing.T) {
	for _, caps := range []byte{0, CapabilityNativeAccepting, CapabilityTimeRelay, CapabilityNativeAccepting | CapabilityTimeRelay, 0x83} {
		adv, err := NewAdvertisement(uuid.MustParse("fbf0f32d-431d-47d9-8907-629f0edc5828"), "tenant:22f35004-d638-4b5c-869f-b815974d6eae:default", 129)
		if err != nil {
			t.Fatal(err)
		}
		adv.Capabilities = caps
		raw, err := adv.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		expected := []byte{1, caps, 0, 129, 0x05, 0xdd, 0xfb, 0xf0, 0xf3, 0x2d}
		if !bytes.Equal(raw, expected) {
			t.Fatalf("binary %x", raw)
		}
		parsed, err := ParseAdvertisement(raw, "tenant:22f35004-d638-4b5c-869f-b815974d6eae:default")
		if err != nil || parsed != adv {
			t.Fatalf("roundtrip %+v %v", parsed, err)
		}
		name, err := adv.MarshalLocalName()
		if err != nil || len(name) != 13 {
			t.Fatalf("name %q %v", name, err)
		}
		parsed, err = ParseLocalName(name, "tenant:22f35004-d638-4b5c-869f-b815974d6eae:default")
		if err != nil || parsed != adv {
			t.Fatalf("name roundtrip %+v %v", parsed, err)
		}
		if parsed.NativeAccepting() != (caps&1 != 0) || parsed.TimeRelayAvailable() != (caps&2 != 0) {
			t.Fatal("capabilities changed")
		}
	}
}
