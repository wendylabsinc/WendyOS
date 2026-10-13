package bleprovider

import (
	"bytes"
	"testing"
)

func TestAdvertisementRoundTripAndMeshIsolation(t *testing.T) {
	uuid, err := ServiceUUID(64)
	if err != nil {
		t.Fatal(err)
	}
	if uuid != "9fd0d83a-47dc-41ef-b85d-0000211be7b4" {
		t.Fatalf("uuid = %s", uuid)
	}
	other, _ := ServiceUUID(65)
	if other == uuid {
		t.Fatal("orgs share a UUID")
	}
	a, err := NewAdvertisement(1042, "production.mesh", DefaultPSM)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != AdvertisementSize {
		t.Fatalf("size %d", len(raw))
	}
	got, err := ParseAdvertisement(raw, "production.mesh")
	if err != nil || got != a {
		t.Fatalf("decoded %+v: %v", got, err)
	}
	if _, err := ParseAdvertisement(raw, "another.mesh"); err == nil {
		t.Fatal("accepted another mesh")
	}
	if _, err := ParseAdvertisement(raw[:9], "production.mesh"); err == nil {
		t.Fatal("accepted truncation")
	}
	bad := bytes.Clone(raw)
	bad[8], bad[9] = 0, 1
	if _, err := ParseAdvertisement(bad, "production.mesh"); err == nil {
		t.Fatal("accepted non-LE PSM")
	}
}

func TestInvalidAdvertisedIdentityAndPSM(t *testing.T) {
	if _, err := ServiceUUID(0); err == nil {
		t.Fatal("accepted org zero")
	}
	for _, asset := range []int32{0, -1} {
		if _, err := NewAdvertisement(asset, "mesh", DefaultPSM); err == nil {
			t.Fatal("accepted invalid asset")
		}
	}
	if _, err := NewAdvertisement(1, "", DefaultPSM); err == nil {
		t.Fatal("accepted empty mesh")
	}
	if _, err := NewAdvertisement(1, "mesh", 0x1001); err == nil {
		t.Fatal("accepted BR/EDR PSM")
	}
}
