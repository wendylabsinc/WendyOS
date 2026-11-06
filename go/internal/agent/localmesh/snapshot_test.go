package localmesh

import (
	"context"
	"testing"
)

func TestCarrierLabelsPreserveActualTransport(t *testing.T) {
	for _, carrier := range []string{"ethernet", "infrastructure-wifi", "tcp", "nan", "ble", "quic", "tls-stream"} {
		if got := carrierName(WithCarrier(context.Background(), carrier), "unknown"); got != carrier {
			t.Fatalf("carrier %q became %q", carrier, got)
		}
	}
	if got := carrierName(WithCarrier(context.Background(), "untrusted-string"), "quic"); got != "quic" {
		t.Fatalf("invalid carrier label accepted: %q", got)
	}
}

func TestSnapshotHasCheaperDirectLink(t *testing.T) {
	a := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	b := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	s := NodeSnapshot{Links: []PeerLink{
		{Device: a, Cost: 4096},
		{Device: a, Cost: 256},
		{Device: b, Cost: 4096},
	}}
	if !s.HasCheaperLink(a, 4096) {
		t.Fatal("same device's TCP link should make BLE redundant")
	}
	if s.HasCheaperLink(b, 4096) {
		t.Fatal("an unrelated peer must not suppress BLE")
	}
	if s.HasCheaperLink(a, 256) {
		t.Fatal("an equally priced link is not cheaper")
	}
}
