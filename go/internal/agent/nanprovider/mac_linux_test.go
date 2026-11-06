//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

var invalidNANTargets = []string{
	"00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "01:00:5e:00:00:01",
	"03:00:00:00:00:02", "02:00:00:00:00:00:00:01",
	"02-00-00-00-00-01", "0200.0000.0001", "020000000001",
	"", "not-a-mac", "02:00:00:00:00", "02:00:00:00:00:gg",
	"02:00:00:00:00:01 peer_id=2", " 02:00:00:00:00:01",
}

func TestNANTargetAddressContract(t *testing.T) {
	for _, good := range []string{"00:00:00:00:00:01", "e4:4a:e0:e5:ba:15", "02:00:00:00:00:01", "E4:4A:E0:E5:BA:15", "e4:4A:e0:E5:ba:15"} {
		if !validMAC(good) {
			t.Errorf("valid individual address rejected: %q", good)
		}
	}
	for _, bad := range invalidNANTargets {
		if validMAC(bad) {
			t.Errorf("invalid supplicant target accepted: %q", bad)
		}
	}
}

func TestInvalidNANTargetsCannotAcquirePeerWorkers(t *testing.T) {
	const local = "02:00:00:00:00:01"
	const nmi = "02:00:00:00:00:02"
	self, lower, higher := testDevice(t, 10), testDevice(t, 9), testDevice(t, 11)
	active := map[string]*radioLink{nmi: {peer: radioPeer{Device: higher, NMI: nmi, ID: "1"}}}
	for _, bad := range invalidNANTargets {
		if shouldInitiate(self, higher, bad, nil, nil, time.Now()) || shouldAccept(self, lower, bad, nil) {
			t.Errorf("invalid discovery/request NMI admitted: %q", bad)
		}
		event := map[string]string{"peer": nmi, "local_ndi": local, "peer_ndi": bad, "ndp_id": "1"}
		if p, ok := connectedPeer(event, local, active); ok {
			t.Errorf("invalid connected NDI acquired a peer worker: %q %+v", bad, p)
		}
	}
	// A failed setup can report an unknown responder NDI. It must still
	// retire its exact pending incarnation; admission rules do not parse ends.
	_, failed := parseEvent("<3>NAN-NDP-DISCONNECTED peer=" + nmi + " ndp_id=1 local_ndi=" + local + " peer_ndi=00:00:00:00:00:00 failure=1")
	if _, ok := disconnectedPeer(failed, local, active); !ok {
		t.Fatal("zero responder NDI prevented matching pending-failure cleanup")
	}
	failed["ndp_id"] = "2"
	if _, ok := disconnectedPeer(failed, local, active); ok {
		t.Fatal("failed setup removed another pending incarnation")
	}
}

func TestCounterResponseRejectsInvalidTargetsBeforeCommand(t *testing.T) {
	const local = "02:00:00:00:00:01"
	for _, bad := range invalidNANTargets {
		v := map[string]string{"peer_nmi": bad, "init_ndi": local, "ndp_id": "1"}
		if cmd, err := counterResponse(local, "2", "00", v); err == nil || cmd != "" {
			t.Errorf("invalid peer target produced command %q: %v", cmd, err)
		}
		v["peer_nmi"], v["init_ndi"] = "02:00:00:00:00:02", bad
		if cmd, err := counterResponse(bad, "2", "00", v); err == nil || cmd != "" {
			t.Errorf("invalid local initiator produced command %q: %v", cmd, err)
		}
	}
}

func TestDrainDoesNotIssueCommandsForInvalidTargets(t *testing.T) {
	const local = "02:00:00:00:00:01"
	const peer = "02:00:00:00:00:02"
	for _, bad := range invalidNANTargets {
		// Inventory addresses are one whitespace-delimited field. Inputs with
		// spaces are already covered at the validator/control builder boundary.
		if strings.ContainsAny(bad, " \t\n") || bad == "" {
			continue
		}
		for _, badPeer := range []bool{true, false} {
			queries := 0
			control := radioCommandFunc(func(cmd string) (string, error) {
				switch {
				case cmd == "NAN_STATUS":
					if badPeer {
						return "peer=" + bad + " ndp_count=1", nil
					}
					return "peer=" + peer + " ndp_count=1", nil
				case cmd == "NAN_PEER_INFO "+peer+" ndps":
					queries++
					return "ndp_id=1 initiator=0 init_ndi=" + bad + " resp_ndi=" + local, nil
				default:
					t.Errorf("invalid inventory target caused command %q", cmd)
					return "", errors.New("invalid inventory target")
				}
			})
			drainRadioContext(context.Background(), control, nil, local, "", zap.NewNop())
			if badPeer && queries != 0 || !badPeer && queries != 1 {
				t.Errorf("wrong inventory lookup count for %q: %d", bad, queries)
			}
		}
	}
}
