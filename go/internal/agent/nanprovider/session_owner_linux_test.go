//go:build linux

package nanprovider

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAbsentNANStatusRequiresExactHelperResponse(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		exitCode     int
		want         bool
	}{
		{"helper reports absent", "nan0 is not present\n", 1, true},
		{"other helper error", "global control socket missing\n", 1, false},
		{"same text other exit", "nan0 is not present\n", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", "printf '%s' \"$1\" >&2; exit \"$2\"", "sh", tc.output, strconv.Itoa(tc.exitCode))
			out, err := cmd.CombinedOutput()
			if got := absentNANStatus(string(out), err); got != tc.want {
				t.Fatalf("absentNANStatus(%q, exit %d)=%v, want %v", out, tc.exitCode, got, tc.want)
			}
		})
	}
	if absentNANStatus("nan0 is not present\n", errors.New("transport failure")) {
		t.Fatal("transport failure treated as absent NAN")
	}
}

func TestSessionPlanPreservesExternalNANAndResetsCrashedProvider(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		started, reclaimedNDI, marked bool
		want                          sessionPlan
	}{
		{"fresh session", false, false, false, sessionPlan{own: true}},
		{"stale marker but NAN already absent", false, false, true, sessionPlan{own: true}},
		{"external host shell session", true, false, false, sessionPlan{}},
		{"crash left owned NDI", true, true, false, sessionPlan{own: true, reset: true}},
		{"crash left nan0 without NDI", true, false, true, sessionPlan{own: true, reset: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planSession(tc.started, tc.reclaimedNDI, tc.marked); got != tc.want {
				t.Fatalf("planSession = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOwnerMarkerRecoversOnlyMatchingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nan.owner")
	owner := Identity{Org: 64, Asset: 445}
	other := Identity{Org: 64, Asset: 446}
	socket := socketIdentity{dev: 1, ino: 42, ctimeSec: 100, ctimeNsec: 123}
	otherSocket := socketIdentity{dev: 1, ino: 43, ctimeSec: 100, ctimeNsec: 124}
	if match, err := ownerMatches(path, owner, socket); err != nil || match {
		t.Fatalf("missing marker match=%v err=%v", match, err)
	}
	if err := markOwner(path, owner, socketIdentity{}); err != nil {
		t.Fatal(err)
	}
	if match, err := ownerMatches(path, owner, socket); err != nil || !match {
		t.Fatalf("pending marker match=%v err=%v", match, err)
	}
	if err := markOwner(path, owner, socket); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("marker mode=%v err=%v", stat, err)
	}
	if match, err := ownerMatches(path, owner, socket); err != nil || !match {
		t.Fatalf("matching owner match=%v err=%v", match, err)
	}
	if match, err := ownerMatches(path, owner, otherSocket); err != nil || match {
		t.Fatalf("new host-shell nan0 must not match: match=%v err=%v", match, err)
	}
	if match, err := ownerMatches(path, other, socket); err == nil || match {
		t.Fatalf("foreign owner must fail closed: match=%v err=%v", match, err)
	}
	if err := clearOwner(path, other); err == nil {
		t.Fatal("foreign owner cleared marker")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("other identity removed marker: %v", err)
	}
	if err := clearOwner(path, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker remains after owner cleanup: %v", err)
	}
}

func TestStaleMarkerWithNANAbsentIsReplacedAndCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nan.owner")
	id := Identity{Org: 64, Asset: 445}
	oldSocket := socketIdentity{dev: 1, ino: 41, ctimeSec: 100, ctimeNsec: 123}
	newSocket := socketIdentity{dev: 1, ino: 42, ctimeSec: 101, ctimeNsec: 456}
	if err := markOwner(path, id, oldSocket); err != nil {
		t.Fatal(err)
	}
	marked, err := ownerMatches(path, id, socketIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if plan := planSession(false, false, marked); !plan.own || plan.reset {
		t.Fatalf("unexpected absent-NAN plan: %+v", plan)
	}
	if err := markOwner(path, id, socketIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := markOwner(path, id, newSocket); err != nil {
		t.Fatal(err)
	}
	if err := clearOwner(path, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale marker remains after clean shutdown: %v", err)
	}
}

func TestControlSocketIdentityChangesAcrossRecreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nan0")
	listen := func() *net.UnixConn {
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	firstConn := listen()
	first, err := controlSocketIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	firstConn.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	secondConn := listen()
	defer secondConn.Close()
	second, err := controlSocketIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("socket recreate reused full identity: %+v", first)
	}
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlSocketIdentity(regular); err == nil {
		t.Fatal("regular file accepted as NAN control socket")
	}
}
