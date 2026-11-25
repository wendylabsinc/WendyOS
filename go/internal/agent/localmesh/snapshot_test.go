package localmesh

import (
	"context"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestSnapshotInstalledRouteOwnershipProtectsTransit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route babel.Route
		want  bool
	}{
		{"transit", babel.Route{Link: 4, Metric: 900}, true},
		{"proof-gap-still-installed", babel.Route{Link: 4, Metric: 900, Seqno: 8}, true},
		{"other-incarnation", babel.Route{Link: 5, Metric: 900}, false},
		{"local", babel.Route{Link: 4, Metric: 900, Local: true}, false},
		{"unreachable", babel.Route{Link: 4, Metric: 900, Unreachable: true}, false},
		{"infinity", babel.Route{Link: 4, Metric: babel.Infinity}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := NodeSnapshot{ForwardingRoutes: []babel.Route{tc.route}}
			if got := view.HasInstalledRouteOnLink(4); got != tc.want {
				t.Fatalf("installed exact-link owner=%v want%v", got, tc.want)
			}
		})
	}
}

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
		{Device: a, ForwardingReady: true, Cost: 4096},
		{Device: a, ForwardingReady: true, Cost: 256},
		{Device: b, ForwardingReady: true, Cost: 4096},
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
