//go:build linux

package nanprovider

import (
	"testing"
	"time"
)

func TestRadioThreeNeighborBoundAndIDOrdering(t *testing.T) {
	now := time.Unix(1_000, 0)
	active := map[string]*radioLink{}
	attempts := map[string]time.Time{}
	const self int32 = 460
	for i := 0; i < 3; i++ {
		nmi := "02:00:00:00:00:0" + string(rune('1'+i))
		if !shouldInitiate(self, self+int32(i)+1, nmi, active, attempts, now) {
			t.Fatalf("eligible outgoing neighbor %d rejected", i)
		}
		active[nmi] = &radioLink{peer: radioPeer{Asset: self + int32(i) + 1, NMI: nmi}}
	}
	if shouldInitiate(self, 464, "02:00:00:00:00:04", active, attempts, now) ||
		shouldAccept(self, 459, "02:00:00:00:00:04", active) ||
		shouldConnect("02:00:00:00:00:04", 459, active) {
		t.Fatal("fourth neighbor admitted")
	}
	if shouldInitiate(self, 459, "02:00:00:00:00:04", nil, attempts, now) ||
		shouldAccept(self, 461, "02:00:00:00:00:04", nil) {
		t.Fatal("wrong initiator ordering admitted")
	}
	if !shouldConnect("02:00:00:00:00:01", 461, active) ||
		shouldConnect("02:00:00:00:00:01", 462, active) {
		t.Fatal("existing path asset binding changed")
	}
}

func TestRadioRetryAndInputBounds(t *testing.T) {
	now := time.Unix(1_000, 0)
	const nmi = "02:00:00:00:00:02"
	attempts := map[string]time.Time{nmi: now}
	if shouldInitiate(1, 2, nmi, nil, attempts, now.Add(14*time.Second)) ||
		!shouldInitiate(1, 2, nmi, nil, attempts, now.Add(15*time.Second)) {
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
	if shouldConnect("not-a-mac", 2, nil) {
		t.Fatal("malformed NMI admitted")
	}
}

func TestCapturedNDPEventsStayOnOwnedNDI(t *testing.T) {
	const local = "e4:4a:e0:e5:ba:66"
	const peerNMI = "e4:4a:e0:e7:6f:cc"
	const peerNDI = "e4:4a:e0:e7:6f:cd"
	_, connected := parseEvent("<3>NAN-NDP-CONNECTED peer=" + peerNMI + " ndp_id=2 local_ndi=" + local + " peer_ndi=" + peerNDI + " interface_id=")
	active := map[string]*radioLink{peerNMI: &radioLink{peer: radioPeer{Asset: 460, NMI: peerNMI}}}
	known := map[string]int32{peerNMI: 460}
	p, ok := connectedPeer(connected, local, known, active)
	if !ok || p.Asset != 460 || p.NDI != peerNDI || p.ID != "2" {
		t.Fatalf("owned connected path rejected: %+v %v", p, ok)
	}
	connected["local_ndi"] = "02:00:00:00:00:99"
	if _, ok := connectedPeer(connected, local, known, active); ok {
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
