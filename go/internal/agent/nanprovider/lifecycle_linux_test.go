//go:build linux

package nanprovider

import (
	"go.uber.org/zap"
	"strings"
	"testing"
	"time"
)

func TestRadioHealthHasBoundedPendingAndFailure(t *testing.T) {
	now := time.Now()
	l := radioLink{since: now}
	if l.expired(now.Add(34*time.Second)) || !l.expired(now.Add(35*time.Second)) {
		t.Fatal("pending deadline")
	}
	l.health(true, now)
	if l.expired(now.Add(time.Hour)) {
		t.Fatal("healthy link expired")
	}
	l.health(false, now.Add(time.Hour))
	l.health(false, now.Add(time.Hour+30*time.Second))
	if !l.expired(now.Add(time.Hour + 35*time.Second)) {
		t.Fatal("repeated failures postponed recovery")
	}
}

type drainFixture struct {
	t        *testing.T
	events   chan string
	commands []string
}

func (f *drainFixture) command(cmd string) (string, error) {
	f.commands = append(f.commands, cmd)
	switch {
	case cmd == "NAN_STATUS":
		return "peer=02:00:00:00:00:02 ndp_count=2", nil
	case strings.HasPrefix(cmd, "NAN_PEER_INFO"):
		return "ndp_id=1 initiator=1 init_ndi=02:00:00:00:00:01 resp_ndi=02:00:00:00:00:02\nndp_id=2 initiator=1 init_ndi=02:00:00:00:00:01 resp_ndi=02:00:00:00:00:02", nil
	case strings.HasPrefix(cmd, "NAN_NDP_TERMINATE"):
		if len(f.events) != 0 {
			f.t.Fatal("second termination sent before first completion")
		}
		id := fields(cmd)["ndp_id"]
		f.events <- "<3>NAN-NDP-DISCONNECTED peer=02:00:00:00:00:02 ndp_id=" + id
		return "OK", nil
	default:
		f.t.Fatalf("unexpected command %s", cmd)
		return "", nil
	}
}

func TestDrainSerializesOwnedNDPCompletion(t *testing.T) {
	f := &drainFixture{t: t, events: make(chan string, 2)}
	drainRadio(f, f.events, "02:00:00:00:00:01", "", zap.NewNop())
	if len(f.commands) != 4 || len(f.events) != 0 {
		t.Fatalf("incomplete drain: %+v", f.commands)
	}
}

func TestCounterProposalUsesSubscriptionAndLocalInitiator(t *testing.T) {
	v := map[string]string{"peer_nmi": "02:00:00:00:00:02", "init_ndi": "02:00:00:00:00:01", "ndp_id": "7"}
	cmd, err := counterResponse(v["init_ndi"], "42", "00", v)
	if err != nil || !strings.Contains(cmd, "handle=42 ndi=wnanndi0") || !strings.Contains(cmd, "ndp_id=7") {
		t.Fatalf("%s %v", cmd, err)
	}
	if _, err := counterResponse("02:00:00:00:00:03", "42", "00", v); err == nil {
		t.Fatal("foreign initiator accepted")
	}
	v["ndp_id"] = "7 other=inject"
	if _, err := counterResponse(v["init_ndi"], "42", "00", v); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestOwnedNDPsMatchesLocalRoleAndPreservesOthers(t *testing.T) {
	const local = "02:00:00:00:00:01"
	const peer = "02:00:00:00:00:02"
	info := "ndp_id=1 initiator=1 init_ndi=" + local + " resp_ndi=" + peer + "\n" +
		"ndp_id=2 initiator=0 init_ndi=" + peer + " resp_ndi=" + local + "\n" +
		"ndp_id=3 initiator=1 init_ndi=" + peer + " resp_ndi=" + local + "\n" +
		"ndp_id=0 initiator=1 init_ndi=" + local + " resp_ndi=" + peer + "\n"
	got := ownedNDPs(peer, local, info)
	if len(got) != 2 || got[0].id != "1" || got[1].id != "2" || got[1].init != peer {
		t.Fatalf("owned tuples: %+v", got)
	}
}

func TestRejectedReplacementPreservesEstablishedDeadline(t *testing.T) {
	l := &radioLink{peer: radioPeer{NDI: "02:00:00:00:00:02", ID: "1"}}
	v := map[string]string{"ndp_id": "1", "failure": "1"}
	if l.matchesDisconnect(v) {
		t.Fatal("rejected replacement removed established path with reused ID")
	}
	v["failure"] = "0"
	if !l.matchesDisconnect(v) {
		t.Fatal("established teardown ignored")
	}
	l.peer.NDI = ""
	v["failure"] = "1"
	if !l.matchesDisconnect(v) {
		t.Fatal("pending setup failure ignored")
	}
}

func TestRadioRecoveryRequiresAuthenticationRepeatedPathsAndDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	r := radioRecovery{}
	r.observe(now, false)
	for i := 0; i < 4; i++ {
		r.connected()
	}
	if r.expired(now.Add(time.Hour)) {
		t.Fatal("unauthenticated discovery can trigger radio reset")
	}
	r.observe(now, true)
	r.observe(now.Add(time.Second), false)
	r.connected()
	r.connected()
	if r.expired(now.Add(time.Hour)) {
		t.Fatal("too few replacement paths can trigger reset")
	}
	r.connected()
	if r.expired(now.Add(90*time.Second)) || !r.expired(now.Add(91*time.Second)) {
		t.Fatal("incorrect outage deadline")
	}
	r.observe(now.Add(100*time.Second), true)
	if r.expired(now.Add(time.Hour)) || r.replacements != 0 {
		t.Fatal("healthy neighbor did not suppress/reset escalation")
	}
}

func TestRadioRecoveryKeepsHigherPeerAsRendezvous(t *testing.T) {
	now := time.Unix(1000, 0)
	r := radioRecovery{hasLowerPeer: true}
	r.observe(now, true)
	r.observe(now.Add(time.Second), false)
	for i := 0; i < 3; i++ {
		r.connected()
	}
	if r.expired(now.Add(200 * time.Second)) {
		t.Fatal("higher peer reset during lower peer recovery window")
	}
	if !r.expired(now.Add(271 * time.Second)) {
		t.Fatal("higher peer fallback is unbounded")
	}
	r.observe(now.Add(240*time.Second), true)
	if r.expired(now.Add(time.Hour)) {
		t.Fatal("peer recovery did not cancel our fallback")
	}
}
