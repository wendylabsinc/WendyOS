package mcusource_test

import (
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
)

func TestWorldNativeCameraPrincipalTCPPairing(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for _, transport := range []string{"tcp", "wendycom"} {
		p := mcusource.SensorPairing{SourcePrincipalURI: principal, Transport: transport, SensorAllowlist: []string{"camera-b"}}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "pairings.json")
		store := mcusource.NewPairingStore(path)
		if err := store.Add(p); err != nil {
			t.Fatal(err)
		}
		reopened := mcusource.NewPairingStore(path)
		if err := reopened.Load(); err != nil {
			t.Fatal(err)
		}
		got := reopened.List()
		if len(got) != 1 || got[0].Key() != principal || got[0].Transport != transport || len(got[0].SensorAllowlist) != 1 || got[0].SensorAllowlist[0] != "camera-b" {
			t.Fatalf("pairing changed: %+v", got)
		}
	}
	for _, p := range []mcusource.SensorPairing{
		{SourcePrincipalURI: principal, Transport: "grpc"},
		{SourcePrincipalURI: principal},
		{SourcePrincipalURI: principal, SourceAssetID: 1, Transport: "tcp"},
		{SourcePrincipalURI: "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/operator/user", Transport: "tcp"},
		{SourcePrincipalURI: "not-a-principal", Transport: "tcp"},
	} {
		if err := p.Validate(); err == nil {
			t.Fatalf("accepted invalid pairing %+v", p)
		}
	}
	if err := (mcusource.SensorPairing{SourceAssetID: 8, Transport: "tcp"}).Validate(); err != nil {
		t.Fatal("legacy numeric pairing changed", err)
	}
}
