//go:build linux

package nanprovider

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func TestIntentionalNANCompletionDrainsExactNDPWithoutFailurePenalty(t *testing.T) {
	now := time.Now()
	const local, nmi = "02:00:00:00:00:01", "02:00:00:00:00:02"
	self, device := testDevice(t, 1), testDevice(t, 2)
	policy := localmesh.NewConnectionPolicy(self, true, localmesh.ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
	lease := policy.Reserve(nmi, device, true)
	if lease == nil || !lease.Ready(device, func() { t.Fatal("reclaimer called physical closer") }) {
		t.Fatal("missing verified fixture lease")
	}
	policy.SetSnapshot(func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{ID: 1, Device: device, Cost: NANLinkCost}, {ID: 2, Device: device, Cost: 128, ForwardingReady: true}}}
	})
	signals := 0
	localmesh.BindConnectionLink(localmesh.WithConnectionLease(context.Background(), lease), 1, NANLinkCost, func() { signals++ })
	if policy.ReservePreferred("next", testDevice(t, 3), true, "", true) != nil || !lease.Retired() || signals != 1 {
		t.Fatal("real provider policy did not retire verified redundancy")
	}
	peer := radioPeer{Device: device, NMI: nmi, NDI: "02:00:00:00:00:04", ID: "7", Outgoing: true, Lease: lease}
	link := &radioLink{peer: peer, healthy: true, since: now.Add(-time.Second)}
	active := map[string]*radioLink{nmi: link}
	queue := &localmesh.CandidateQueue{}
	queue.Failed(nmi, now.Add(-time.Second))
	attempts := map[string]time.Time{nmi: now.Add(-time.Second)}
	var emitted []radioPeer
	emit := func(p radioPeer) { emitted = append(emitted, p) }
	if handleRadioHealth(radioHealth{Peer: peer}, active, queue, attempts, now, emit) != link {
		t.Fatal("exact health completion did not match owned NDP")
	}
	expireRadioLink(link, now.Add(time.Second), queue, attempts, nil, emit)
	if !link.draining || link.healthy || len(emitted) != 1 || emitted[0].NDI != "" || emitted[0].ID != peer.ID || emitted[0].Lease != lease {
		t.Fatal("intentional completion did not begin one exact owned drain")
	}
	if !queue.ReclaimEligible(nmi, now) || len(attempts) != 0 || policy.Counts().Total != 1 {
		t.Fatal("intentional drain penalized the peer or freed capacity before disconnect")
	}
	handleRadioHealth(radioHealth{Peer: peer, Up: true}, active, queue, attempts, now.Add(time.Second), emit)
	handleRadioHealth(radioHealth{Peer: peer}, active, queue, attempts, now.Add(time.Second), emit)
	if link.healthy || len(emitted) != 1 {
		t.Fatal("late health revived or re-emitted a terminal drain")
	}
	var commands []string
	control := radioCommandFunc(func(cmd string) (string, error) {
		commands = append(commands, cmd)
		if cmd == "NAN_PEER_INFO "+nmi+" ndps" {
			return fmt.Sprintf("ndp_id=7 initiator=1 init_ndi=%s resp_ndi=%s\nndp_id=8 initiator=1 init_ndi=%s resp_ndi=%s\nndp_id=7 initiator=0 init_ndi=%s resp_ndi=%s", local, nmi, local, nmi, nmi, local), nil
		}
		return "OK", nil
	})
	if err := link.drainTick(now.Add(time.Second), control, local, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	want := []string{"NAN_PEER_INFO " + nmi + " ndps", "NAN_NDP_TERMINATE peer_nmi=" + nmi + " init_ndi=" + local + " ndp_id=7"}
	if !reflect.DeepEqual(commands, want) || policy.Counts().Total != 1 || active[nmi] != link {
		t.Fatalf("termination escaped exact ownership or released early: %v", commands)
	}
	recovery, setup := &radioRecovery{}, &ndpSetupRecovery{}
	finishRadioDisconnect(map[string]string{"peer": nmi, "ndp_id": "8", "local_ndi": local}, local, active, recovery, setup, now, emit)
	if policy.Counts().Total != 1 {
		t.Fatal("another NDP disconnect freed the retired lease")
	}
	finishRadioDisconnect(map[string]string{"peer": nmi, "ndp_id": "7", "local_ndi": local, "failure": "0"}, local, active, recovery, setup, now, emit)
	if policy.Counts().Total != 0 || len(active) != 0 || len(emitted) != 2 || !queue.ReclaimEligible(nmi, now) || len(attempts) != 0 || setup.failures != 0 || recovery.replacements != 0 {
		t.Fatal("matched intentional disconnect did not release normally without failure state")
	}
	if next := policy.Reserve("next", testDevice(t, 3), true); next == nil {
		t.Fatal("actual disconnect did not free capacity for new peer")
	} else {
		next.Release()
	}
}

func TestOrdinaryNANFailureKeepsPenaltyAndStaleHealthCannotDrainReplacement(t *testing.T) {
	now := time.Now()
	const local, nmi = "02:00:00:00:00:01", "02:00:00:00:00:02"
	device := testDevice(t, 2)
	policy := localmesh.NewConnectionPolicy(testDevice(t, 1), true, localmesh.ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
	lease := policy.Reserve(nmi, device, true)
	lease.Ready(device, func() {})
	peer := radioPeer{Device: device, NMI: nmi, NDI: nmi, ID: "7", Outgoing: true, Lease: lease}
	link := &radioLink{peer: peer, healthy: true, since: now}
	active, queue, attempts := map[string]*radioLink{nmi: link}, &localmesh.CandidateQueue{}, map[string]time.Time{}
	emitted := 0
	emit := func(radioPeer) { emitted++ }
	stale := peer
	stale.Lease = nil
	if handleRadioHealth(radioHealth{Peer: stale}, active, queue, attempts, now, emit) != nil || !link.healthy || emitted != 0 {
		t.Fatal("stale lease completion changed replacement ownership")
	}
	handleRadioHealth(radioHealth{Peer: peer}, active, queue, attempts, now, emit)
	expireRadioLink(link, now.Add(time.Second), queue, attempts, nil, emit)
	if !link.draining || emitted != 1 || queue.ReclaimEligible(nmi, now) || len(attempts) != 1 || lease.Retired() || policy.Counts().Total != 1 {
		t.Fatal("ordinary failure lost its existing penalty, cooldown, or NDP ownership")
	}
	finishRadioDisconnect(map[string]string{"peer": nmi, "ndp_id": "7", "local_ndi": local, "failure": "0"}, local, active, &radioRecovery{}, &ndpSetupRecovery{}, now, emit)
	if policy.Counts().Total != 0 || queue.ReclaimEligible(nmi, now) || len(attempts) != 1 {
		t.Fatal("normal disconnect erased real failure state or failed to release")
	}
}
