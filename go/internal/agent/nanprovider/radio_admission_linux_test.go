//go:build linux

package nanprovider

import (
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestConnectedNDPRequiresPendingIdentity(t *testing.T) {
	const nmi = "02:00:00:00:00:02"
	const local = "02:00:00:00:00:01"
	event := map[string]string{"peer": nmi, "local_ndi": local, "peer_ndi": nmi, "ndp_id": "2"}
	if peer, ok := connectedPeer(event, local, nil); ok {
		t.Errorf("unsolicited connected event acquired a worker: %+v", peer)
	}
	active := map[string]*radioLink{nmi: {peer: radioPeer{Device: testDevice(t, 2), NMI: nmi, ID: "1"}}}
	if _, ok := connectedPeer(event, local, active); ok {
		t.Error("connected event replaced a different pending NDP incarnation")
	}
	event["ndp_id"] = "1"
	if _, ok := connectedPeer(event, local, active); !ok {
		t.Error("matching pending NDP was rejected")
	}
	active[nmi].peer.NDI = "02:00:00:00:00:09"
	if _, ok := connectedPeer(event, local, active); ok {
		t.Error("connected event replaced an established NDI")
	}
}

func TestRadioPreservesDistinctUnderlayOwners(t *testing.T) {
	const existingNMI = "02:00:00:00:00:02"
	const newNMI = "02:00:00:00:00:03"
	owner := testDevice(t, 2)
	collision, err := meshid.ParseDevice(testTenant(t), "018b8657-2f1a-4d3e-9c1a-000000010002")
	if err != nil {
		t.Fatal(err)
	}
	high, err := meshid.ParseDevice(testTenant(t), "ffffffff-ffff-4fff-bfff-fffffffffff0")
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]*radioLink{existingNMI: {peer: radioPeer{Device: owner, NMI: existingNMI}}}
	for name, peer := range map[string]meshid.Device{"same address": collision, "same device on another NMI": owner} {
		t.Run(name, func(t *testing.T) {
			if shouldConnect(newNMI, peer, active) ||
				shouldInitiate(testDevice(t, 1), peer, newNMI, active, nil, time.Now()) ||
				shouldAccept(high, peer, newNMI, active) {
				t.Fatal("a newcomer can replace a pending/active underlay owner")
			}
		})
	}
	if shouldAccept(high, collision, existingNMI, active) {
		t.Error("a different device can overwrite an existing NMI binding")
	}
	for _, peer := range []meshid.Device{testDevice(t, 0), testDevice(t, 0xffff)} {
		if shouldConnect(newNMI, peer, nil) || shouldAccept(high, peer, newNMI, nil) {
			t.Error("reserved NAN underlay admitted before NDP setup")
		}
	}
	if !shouldConnect(existingNMI, owner, active) ||
		!shouldConnect(newNMI, testDevice(t, 3), active) {
		t.Fatal("valid incumbent or distinct neighbour was rejected")
	}
}

func TestRadioAttemptHistoryIsBoundedAndExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	attempts := map[string]time.Time{}
	for i := 0; i < 128; i++ {
		attempts[fmt.Sprintf("02:00:00:00:01:%02x", i)] = now
	}
	const nmi = "02:00:00:00:02:01"
	if shouldInitiate(testDevice(t, 1), testDevice(t, 2), nmi, nil, attempts, now) {
		t.Error("new NMI admitted while recent attempt history is full")
	}
	if !shouldInitiate(testDevice(t, 1), testDevice(t, 2), nmi, nil, attempts, now.Add(15*time.Second)) {
		t.Error("expired attempt history blocks a new neighbour")
	}
	if len(attempts) != 0 {
		t.Error("expired attempts remain retained")
	}
	for i := 0; i < 4096; i++ {
		rememberRadioAttempt(attempts, fmt.Sprintf("02:00:00:01:%02x:%02x", i>>8, i&255), now)
	}
	if len(attempts) > 128 {
		t.Fatal("rapid peer churn grew the attempt history beyond its bound")
	}
	expireRadioAttempts(attempts, now.Add(15*time.Second))
	if len(attempts) != 0 {
		t.Fatal("idle expired history remains retained")
	}
}
