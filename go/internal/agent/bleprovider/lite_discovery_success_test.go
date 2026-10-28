package bleprovider

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestNormalDefaultNetworkAdvertisementForLite(t *testing.T) {
	tenant := mustFixtureTenant("22f35004-d638-4b5c-869f-b815974d6eae")
	advertisement, err := NewAdvertisement(uuid.MustParse("fbf0f32d-431d-47d9-8907-629f0edc5828"), fmt.Sprintf("tenant:%s:default", tenant), DefaultPSM)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := advertisement.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Shared with Lite's normal BLE discovery success fixture.
	want := []byte{1, 3, 0, 129, 0x05, 0xdd, 0xfb, 0xf0, 0xf3, 0x2d}
	if !bytes.Equal(payload, want) {
		t.Fatalf("default BLE payload %x, want %x", payload, want)
	}
}
