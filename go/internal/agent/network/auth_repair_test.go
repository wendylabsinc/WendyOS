package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// repairKinds is every kind of saved profile the repair can meet.
var repairKinds = []struct{ uuid, keyMgmt, authAlg, ssid string }{
	{"psk-open", "wpa-psk", "open", "Home"},
	{"psk-unset", "wpa-psk", "", "Lab"},
	{"sae-open", "sae", "open", "Office"},
	{"wep-open", "none", "open", "Old"},
	{"eap-open", "wpa-eap", "open", "Corp"},
	{"open-net", "", "", "Cafe"},
}

// repairProfiles answers the batched read; withGone adds a profile that
// vanishes before it is read, which sends the read down the per-profile path.
func repairProfiles(withGone bool) []reply {
	list, uuids, records := "", []string{}, []string{}
	var single []reply
	for _, k := range repairKinds {
		list += k.uuid + ":802-11-wireless\n"
		uuids = append(uuids, k.uuid)
		records = append(records, batchRecord(k.uuid, k.keyMgmt, k.authAlg, k.ssid))
		single = append(single, reply{args: profileArgs(k.uuid), out: profileOut(k.keyMgmt, k.authAlg, k.ssid)})
	}
	batch := reply{args: batchArgs(uuids...), out: batchOut(records...)}
	if withGone {
		list += "gone:802-11-wireless\n"
		batch = reply{args: batchArgs(append(uuids, "gone")...), out: batchOut(records...), code: nmcliNoSuchConnection}
		single = append(single, reply{args: profileArgs("gone"), out: "Error: gone - no such connection profile.\n", code: nmcliNoSuchConnection})
	}
	return append([]reply{{args: listUUIDs, out: list + "eth:802-3-ethernet\n"}, batch}, single...)
}

func TestRepairForcedOpenAuthClearsOnlyWPAProfiles(t *testing.T) {
	for _, withGone := range []bool{false, true} {
		path, log := writeFakeNMCLI(t, append(repairProfiles(withGone), reply{args: "connection modify ", prefix: true})...)
		n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
		if !n.repairForcedOpenAuth(context.Background()) {
			t.Errorf("withGone=%v: want a complete pass", withGone)
		}
		calls := readCalls(t, log)
		got := callsWithPrefix(calls, "connection modify ")
		want := []string{
			"connection modify psk-open " + authAlgField + " ",
			"connection modify sae-open " + authAlgField + " ",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("withGone=%v: modified:\n%s\nwant:\n%s", withGone, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if reads := callsWithPrefix(calls, "--escape no "); !withGone && len(reads) != 1 {
			t.Errorf("read profiles with %d calls; want one", len(reads))
		}
	}
}

func TestRepairForcedOpenAuthReportsIncompletePass(t *testing.T) {
	for name, replies := range map[string][]reply{
		"unreadable profile": {
			{args: listUUIDs, out: "psk-open:802-11-wireless\n"},
			{args: profileArgs("psk-open"), out: "Error: NetworkManager is not running.\n", code: 8},
		},
		"modify fails": append(repairProfiles(false), reply{args: "connection modify ", prefix: true, code: 1}),
		"no list":      {{args: listUUIDs, code: 8}},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := writeFakeNMCLI(t, replies...)
			n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
			if n.repairForcedOpenAuth(context.Background()) {
				t.Error("want an incomplete pass")
			}
		})
	}
}

// testDeadline bounds a run, so a broken repair fails the test instead of
// retrying until the package timeout.
func testDeadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// useRepairPaths points the repair at a test os-release and marker.
func useRepairPaths(t *testing.T, osID string) (marker string) {
	t.Helper()
	dir := t.TempDir()
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("NAME=\"Test\"\nID_LIKE=wendyos\nID=\""+osID+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(dir, "state", "marker")
	oldOS, oldMarker, oldRetry := osReleasePath, authRepairMarker, authRepairRetry
	osReleasePath, authRepairMarker, authRepairRetry = osRelease, marker, 5*time.Millisecond
	t.Cleanup(func() { osReleasePath, authRepairMarker, authRepairRetry = oldOS, oldMarker, oldRetry })
	return marker
}

func TestRepairForcedOpenAuthOnceRunsOnceOnWendyOS(t *testing.T) {
	marker := useRepairPaths(t, "wendyos")
	path, log := writeFakeNMCLI(t, append(repairProfiles(true), reply{args: "connection modify ", prefix: true})...)
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}

	n.RepairForcedOpenAuthOnce(testDeadline(t))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written after a complete pass: %v", err)
	}
	first := len(readCalls(t, log))
	n.RepairForcedOpenAuthOnce(testDeadline(t))
	if again := len(readCalls(t, log)); again != first {
		t.Errorf("second run made %d nmcli calls; want none", again-first)
	}
}

func TestRepairForcedOpenAuthOnceSkipsOtherHosts(t *testing.T) {
	marker := useRepairPaths(t, "ubuntu")
	path, log := writeFakeNMCLI(t, repairProfiles(false)...)
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}

	n.RepairForcedOpenAuthOnce(testDeadline(t))
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Errorf("made nmcli calls %q on a non-WendyOS host", calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker written on a non-WendyOS host")
	}
}

func TestRepairForcedOpenAuthOnceRetriesWithoutMarker(t *testing.T) {
	marker := useRepairPaths(t, "wendyos")
	path, log := writeFakeNMCLI(t, reply{args: listUUIDs, code: 8})
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	n.RepairForcedOpenAuthOnce(ctx)
	if tries := len(callsWithPrefix(readCalls(t, log), listUUIDs)); tries < 2 {
		t.Errorf("listed profiles %d times; want retries until the deadline", tries)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker written although no pass completed")
	}
}
