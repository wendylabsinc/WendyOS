//go:build linux

package main

import "testing"

func TestGatewayWithdrawalAfterCatalogueShutdown(t *testing.T) {
	m := &meshCatalogManager{}
	for range 2 {
		if err := m.SetGatewayOffer(false); err != nil {
			t.Fatalf("cleanup cannot finish after catalogue shutdown: %v", err)
		}
	}
	if err := m.SetGatewayOffer(true); err == nil {
		t.Fatal("inactive catalogue advertised an uplink")
	}
}
