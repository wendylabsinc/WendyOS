package bleprovider

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

var (
	testTenant  = mustFixtureTenant("9ba0dd9f-65ea-34da-8878-d92358210b80")
	otherTenant = mustFixtureTenant("11111111-1111-4111-8111-111111111111")
	testDevice  = uuid.MustParse("6f6b1c9e-2d4a-4c8f-9a01-7e5d3b2c1a09")
	peerDevice  = uuid.MustParse("c1c2c3c4-0000-4000-8000-000000000000")
	lowDevice   = uuid.MustParse("11111111-2222-3333-4444-555555555555")
	selfFixture = meshid.NewDevice(testTenant, testDevice)
	peerFixture = meshid.NewDevice(testTenant, peerDevice)
	lowFixture  = meshid.NewDevice(testTenant, lowDevice)
)

func fixtureDevice(text string) meshid.Device {
	return meshid.NewDevice(testTenant, uuid.MustParse(text))
}

func mustFixtureTenant(text string) meshid.Tenant {
	tenant, err := meshid.ParseTenant(text)
	if err != nil {
		panic(err)
	}
	return tenant
}

func TestAdvertisementRoundTripAndMeshIsolation(t *testing.T) {
	serviceUUID := ServiceUUID(testTenant)
	if serviceUUID != testTenant.String() {
		t.Fatalf("uuid = %s", serviceUUID)
	}
	other, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	if ServiceUUID(other) == serviceUUID {
		t.Fatal("tenants share a UUID")
	}
	a, err := NewAdvertisement(testDevice, "production.mesh", DefaultPSM)
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
	if total := 3 + 2 + 16 + len(raw); total > 31 {
		t.Fatalf("legacy advertisement needs %d bytes including flags and ServiceData framing", total)
	}
	got, err := ParseAdvertisement(raw, "production.mesh")
	if err != nil || got != a {
		t.Fatalf("decoded %+v: %v", got, err)
	}
	if _, err := ParseAdvertisement(raw, "another.mesh"); err == nil {
		t.Fatal("accepted another mesh")
	}
	if _, err := ParseAdvertisement(raw[:AdvertisementSize-1], "production.mesh"); err == nil {
		t.Fatal("accepted truncation")
	}
	bad := bytes.Clone(raw)
	bad[1], bad[2] = 0, 1
	if _, err := ParseAdvertisement(bad, "production.mesh"); err == nil {
		t.Fatal("accepted non-LE PSM")
	}
	// A zero prefix is valid; it does not imply a nil complete UUID.
	zeroPrefix := uuid.MustParse("00000000-1234-4000-8000-000000000001")
	zero, err := NewAdvertisement(zeroPrefix, "production.mesh", DefaultPSM)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = zero.MarshalBinary()
	if _, err := ParseAdvertisement(raw, "production.mesh"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidAdvertisedIdentityAndPSM(t *testing.T) {
	if ServiceUUID(testTenant) == "" {
		t.Fatal("tenant service UUID empty")
	}
	if ServiceUUID(meshid.Tenant{}) != "" {
		t.Fatal("accepted zero tenant")
	}
	if _, err := NewAdvertisement(uuid.Nil, "mesh", DefaultPSM); err == nil {
		t.Fatal("accepted nil device")
	}
	if _, err := NewAdvertisement(peerDevice, "", DefaultPSM); err == nil {
		t.Fatal("accepted empty mesh")
	}
	if _, err := NewAdvertisement(peerDevice, "mesh", 0x1001); err == nil {
		t.Fatal("accepted BR/EDR PSM")
	}
}
