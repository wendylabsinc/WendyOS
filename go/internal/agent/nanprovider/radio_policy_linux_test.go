//go:build linux

package nanprovider

import (
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func testTenant(t *testing.T) meshid.Tenant {
	t.Helper()
	tenant, err := meshid.ParseTenant("5b8c6c8e-2f9f-4b4a-9a4f-6e2d8c9a0b12")
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// testDevice builds a deterministic device whose UUID sorts by n, so ordering
// assertions read like the old asset-ID tests.
func testDevice(t *testing.T, n uint16) meshid.Device {
	t.Helper()
	id, err := meshid.ParseDevice(testTenant(t), fmt.Sprintf("018b8657-2f1a-4d3e-9c1a-%012x", n))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRadioFourNeighborBoundWithoutUUIDOrdering(t *testing.T) {
	now := time.Unix(1_000, 0)
	active := map[string]*radioLink{}
	attempts := map[string]time.Time{}
	self := testDevice(t, 460)
	for i := 0; i < 4; i++ {
		nmi := "02:00:00:00:00:0" + string(rune('1'+i))
		peer := testDevice(t, 461+uint16(i))
		if !shouldInitiate(self, peer, nmi, active, attempts, now) {
			t.Fatalf("eligible outgoing neighbor %d rejected", i)
		}
		active[nmi] = &radioLink{peer: radioPeer{Device: peer, NMI: nmi}}
	}
	if shouldInitiate(self, testDevice(t, 465), "02:00:00:00:00:05", active, attempts, now) ||
		shouldAccept(self, testDevice(t, 459), "02:00:00:00:00:05", active) ||
		shouldConnect("02:00:00:00:00:04", testDevice(t, 459), active) {
		t.Fatal("fifth neighbor admitted")
	}
	if !shouldInitiate(self, testDevice(t, 459), "02:00:00:00:00:04", nil, attempts, now) ||
		!shouldAccept(self, testDevice(t, 461), "02:00:00:00:00:04", nil) {
		t.Fatal("UUID order incorrectly gates admission")
	}
	if !shouldConnect("02:00:00:00:00:01", testDevice(t, 461), active) ||
		shouldConnect("02:00:00:00:00:01", testDevice(t, 462), active) {
		t.Fatal("existing path device binding changed")
	}
}

func TestRadioRetryAndInputBounds(t *testing.T) {
	now := time.Unix(1_000, 0)
	const nmi = "02:00:00:00:00:02"
	attempts := map[string]time.Time{nmi: now}
	if shouldInitiate(testDevice(t, 1), testDevice(t, 2), nmi, nil, attempts, now.Add(14*time.Second)) ||
		!shouldInitiate(testDevice(t, 1), testDevice(t, 2), nmi, nil, attempts, now.Add(15*time.Second)) {
		t.Fatal("NDP attempt cooldown is not bounded")
	}
	for _, bad := range []string{"", "0", "256", "1 other=field", "-1"} {
		if validNDPID(bad) {
			t.Fatalf("accepted invalid NDP ID %q", bad)
		}
	}
	for _, bad := range []string{"", "0", "1 peer_nmi=spoof", "-1"} {
		if validHandle(bad) {
			t.Fatalf("accepted invalid supplicant handle %q", bad)
		}
	}
	if shouldConnect("not-a-mac", testDevice(t, 2), nil) {
		t.Fatal("malformed NMI admitted")
	}
}

func TestCapturedNDPEventsStayOnOwnedNDI(t *testing.T) {
	const local = "e4:4a:e0:e5:ba:66"
	const peerNMI = "e4:4a:e0:e7:6f:cc"
	const peerNDI = "e4:4a:e0:e7:6f:cd"
	_, connected := parseEvent("<3>NAN-NDP-CONNECTED peer=" + peerNMI + " ndp_id=2 local_ndi=" + local + " peer_ndi=" + peerNDI + " interface_id=")
	active := map[string]*radioLink{peerNMI: {peer: radioPeer{Device: testDevice(t, 460), NMI: peerNMI}}}
	p, ok := connectedPeer(connected, local, active)
	if !ok || p.Device != testDevice(t, 460) || p.NDI != peerNDI || p.ID != "2" {
		t.Fatalf("owned connected path rejected: %+v %v", p, ok)
	}
	connected["local_ndi"] = "02:00:00:00:00:99"
	if _, ok := connectedPeer(connected, local, active); ok {
		t.Fatal("foreign NDI path adopted")
	}
	connected["local_ndi"] = local
	active[peerNMI] = &radioLink{peer: p}
	_, disconnected := parseEvent("<3>NAN-NDP-DISCONNECTED peer=" + peerNMI + " ndp_id=2 local_ndi=" + local + " peer_ndi=" + peerNDI + " reason=1 locally_generated=1, failure=0")
	if removal, ok := disconnectedPeer(disconnected, local, active); !ok || removal.ID != "2" {
		t.Fatalf("owned disconnect ignored: %+v %v", removal, ok)
	}
	disconnected["local_ndi"] = "02:00:00:00:00:99"
	if _, ok := disconnectedPeer(disconnected, local, active); ok {
		t.Fatal("foreign NDI disconnect removed owned path")
	}
}
