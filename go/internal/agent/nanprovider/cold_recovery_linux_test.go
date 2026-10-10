//go:build linux

package nanprovider

import (
	"testing"
	"time"
)

func TestColdNDPRecoveryRequiresThreePathsOutboundTrafficAndDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	noTraffic := coldNDPRecovery{}
	for i := 0; i < 3; i++ {
		noTraffic.connected(now.Add(time.Duration(i)*35*time.Second), ndiTraffic{rx: 40, tx: 100})
	}
	if noTraffic.ready(now.Add(time.Hour), false) {
		t.Fatal("NDP signaling without outbound data triggered a reset")
	}
	r := coldNDPRecovery{}
	r.connected(now, ndiTraffic{rx: 40, tx: 100})
	r.connected(now.Add(35*time.Second), ndiTraffic{rx: 40, tx: 100})
	r.observe(now.Add(36*time.Second), ndiTraffic{rx: 40, tx: 101})
	if r.ready(now.Add(time.Hour), false) {
		t.Fatal("two NDPs triggered a reset")
	}
	r.connected(now.Add(70*time.Second), ndiTraffic{rx: 40, tx: 101})
	if r.ready(now.Add(89*time.Second), false) || !r.ready(now.Add(90*time.Second), false) {
		t.Fatal("wrong cold no-RX deadline")
	}
	if r.ready(now.Add(time.Hour), false) {
		t.Fatal("repeated reset requested without a new epoch")
	}
}

func TestColdNDPRecoveryPreservesTrafficAndHealthyPeers(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name    string
		traffic ndiTraffic
		healthy bool
	}{
		{"received data", ndiTraffic{rx: 2, tx: 2}, false},
		{"authenticated neighbor", ndiTraffic{rx: 1, tx: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := coldNDPRecovery{}
			for i := 0; i < 3; i++ {
				r.connected(now.Add(time.Duration(i)*35*time.Second), ndiTraffic{rx: 1, tx: 1})
			}
			r.observe(now.Add(71*time.Second), tc.traffic)
			if r.ready(now.Add(time.Hour), tc.healthy) {
				t.Fatal("healthy receive path requested a reset")
			}
		})
	}
}

func TestColdNDPRecoveryIgnoresCounterReset(t *testing.T) {
	now := time.Unix(1000, 0)
	r := coldNDPRecovery{}
	r.connected(now, ndiTraffic{rx: 40, tx: 100})
	r.connected(now.Add(35*time.Second), ndiTraffic{rx: 40, tx: 101})
	r.observe(now.Add(36*time.Second), ndiTraffic{rx: 0, tx: 0})
	if r.ready(now.Add(time.Hour), false) || r.paths != 0 {
		t.Fatal("interface counter reset was treated as proof of packet loss")
	}
}

func TestColdNDPRecoveryAfterOneRXRequiresLaterPathsAndLongerDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	r := coldNDPRecovery{}
	r.connected(now, ndiTraffic{rx: 40, tx: 100})
	r.observe(now.Add(20*time.Second), ndiTraffic{rx: 41, tx: 101})
	r.connected(now.Add(60*time.Second), ndiTraffic{rx: 41, tx: 101})
	r.observe(now.Add(61*time.Second), ndiTraffic{rx: 41, tx: 102})
	r.connected(now.Add(100*time.Second), ndiTraffic{rx: 41, tx: 102})
	if r.ready(now.Add(time.Hour), false) {
		t.Fatal("fewer than three later NDPs triggered recovery")
	}
	r.connected(now.Add(140*time.Second), ndiTraffic{rx: 41, tx: 103})
	if r.ready(now.Add(289*time.Second), false) || !r.ready(now.Add(290*time.Second), false) {
		t.Fatal("RX followed by a silent data path did not use the longer deadline")
	}
	if r.ready(now.Add(time.Hour), false) {
		t.Fatal("second reset requested for one data-path outage")
	}
}

func TestColdNDPRecoveryFreshRXRestartsEvidenceWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	r := coldNDPRecovery{}
	r.connected(now, ndiTraffic{rx: 10, tx: 10})
	r.observe(now.Add(20*time.Second), ndiTraffic{rx: 11, tx: 11})
	r.connected(now.Add(60*time.Second), ndiTraffic{rx: 11, tx: 12})
	r.connected(now.Add(100*time.Second), ndiTraffic{rx: 11, tx: 13})
	r.observe(now.Add(120*time.Second), ndiTraffic{rx: 12, tx: 14})
	r.connected(now.Add(150*time.Second), ndiTraffic{rx: 12, tx: 15})
	if r.ready(now.Add(time.Hour), false) || r.paths != 1 {
		t.Fatal("fresh receive traffic did not restart NDP evidence window")
	}
}
