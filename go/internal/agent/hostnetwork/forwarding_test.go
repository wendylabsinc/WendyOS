package hostnetwork

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func forwardingFixture(t *testing.T, initial string) *bool {
	t.Helper()
	oldSysctl, oldMarker, oldTables := forwardingSysctl, forwardingMarker, lanIPTables
	forwardingSysctl = filepath.Join(t.TempDir(), "forwarding")
	forwardingMarker = filepath.Join(t.TempDir(), "baseline")
	if err := os.WriteFile(forwardingSysctl, []byte(initial+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	forwarding.users = make(map[string]bool)
	forwarding.lanOwners = make(map[string]string)
	fail := false
	absent := exec.Command("sh", "-c", "exit 1").Run()
	rules := map[string]bool{}
	lanIPTables = func(args ...string) ([]byte, error) {
		verb := args[2]
		offset := 4
		if verb == "-I" {
			offset = 5
		}
		key := args[1] + "/" + args[3] + "/" + strings.Join(args[offset:], " ")
		switch verb {
		case "-S":
			return nil, nil
		case "-N":
			return nil, nil
		case "-C":
			if rules[key] {
				return nil, nil
			}
			return nil, absent
		case "-I", "-A":
			rules[key] = true
		case "-D":
			if fail {
				return nil, errors.New("injected deletion failure")
			}
			delete(rules, key)
		default:
			t.Fatalf("unexpected command %v", args)
		}
		return nil, nil
	}
	t.Cleanup(func() {
		forwardingSysctl, forwardingMarker, lanIPTables = oldSysctl, oldMarker, oldTables
		forwarding.users = make(map[string]bool)
		forwarding.lanOwners = make(map[string]string)
	})
	return &fail
}
func forwardingValue(t *testing.T, want string) {
	t.Helper()
	b, e := os.ReadFile(forwardingSysctl)
	if e != nil || strings.TrimSpace(string(b)) != want {
		t.Fatalf("forwarding %q/%v want%s", b, e, want)
	}
}
func TestForwardingLeasesShareAndRestore(t *testing.T) {
	for _, initial := range []string{"0", "1"} {
		t.Run(initial, func(t *testing.T) {
			forwardingFixture(t, initial)
			for _, key := range []string{"mesh", "lan", "lan"} {
				if e := AcquireForwarding(key); e != nil {
					t.Fatal(e)
				}
			}
			if len(forwarding.users) != 2 {
				t.Fatal("duplicate counted")
			}
			if e := ReleaseForwarding("mesh"); e != nil {
				t.Fatal(e)
			}
			forwardingValue(t, "1")
			if e := ReleaseForwarding("lan"); e != nil {
				t.Fatal(e)
			}
			forwardingValue(t, initial)
			if _, e := os.Stat(forwardingMarker); !os.IsNotExist(e) {
				t.Fatal("baseline retained", e)
			}
		})
	}
}
func TestForwardingFailedCleanupCanReacquire(t *testing.T) {
	fail := forwardingFixture(t, "0")
	if e := AcquireForwarding("lan"); e != nil {
		t.Fatal(e)
	}
	*fail = true
	if e := ReleaseForwarding("lan"); e == nil {
		t.Fatal("failure lost")
	}
	forwardingValue(t, "0")
	if len(forwarding.users) != 1 {
		t.Fatal("lost retry ownership")
	}
	*fail = false
	if e := AcquireForwarding("lan"); e != nil {
		t.Fatal(e)
	}
	forwardingValue(t, "1")
	if e := ReleaseForwarding("lan"); e != nil {
		t.Fatal(e)
	}
	forwardingValue(t, "0")
}
func TestForwardingCrashReclaimsOnlyOwnedBaseline(t *testing.T) {
	forwardingFixture(t, "0")
	if e := AcquireForwarding("dead"); e != nil {
		t.Fatal(e)
	}
	forwarding.users = make(map[string]bool) // new process; only durable marker/rules survive
	if e := reconcileUnusedForwarding(); e != nil {
		t.Fatal(e)
	}
	forwardingValue(t, "0")
	if e := os.WriteFile(forwardingMarker, []byte("unexpected\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := AcquireForwarding("new"); e == nil {
		t.Fatal("malformed baseline accepted")
	}
	forwardingValue(t, "0")
}
func TestForwardingConcurrentLifetimes(t *testing.T) {
	forwardingFixture(t, "0")
	if e := AcquireForwarding("keeper"); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprint(i)
			if e := AcquireForwarding(k); e != nil {
				t.Error(e)
				return
			}
			if e := ReleaseForwarding(k); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	forwardingValue(t, "1")
	if len(forwarding.users) != 1 {
		t.Fatal("leaked consumer")
	}
	if e := ReleaseForwarding("keeper"); e != nil {
		t.Fatal(e)
	}
	forwardingValue(t, "0")
}

func TestOrphanLANForwardingReleasesReplacedNamespacesAndRetainsFailures(t *testing.T) {
	fail := forwardingFixture(t, "0")
	if err := AcquireForwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	for _, access := range []LANServiceAccess{
		{NetNS: "/old/ns", AppIP: "10.42.1.2", Port: 8080},
		{NetNS: "/replacement/ns", AppIP: "10.42.1.2", Port: 9090},
		{NetNS: "/other/ns", AppIP: "10.42.1.3", Port: 8080},
	} {
		if err := acquireLANForwarding(access); err != nil {
			t.Fatal(err)
		}
	}
	if err := releaseOrphanLANForwarding("10.42.1.2"); err != nil {
		t.Fatal(err)
	}
	if len(forwarding.users) != 2 || len(forwarding.lanOwners) != 1 || !forwarding.users["mesh"] {
		t.Fatalf("orphan release changed another owner or retained old handles: %v %v", forwarding.users, forwarding.lanOwners)
	}
	forwardingValue(t, "1")
	if err := ReleaseForwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	*fail = true
	if err := releaseOrphanLANForwarding("10.42.1.3"); err == nil || len(forwarding.users) != 1 || len(forwarding.lanOwners) != 1 {
		t.Fatal("failed final restoration lost its retry owner")
	}
	*fail = false
	if err := releaseOrphanLANForwarding("10.42.1.3"); err != nil {
		t.Fatal(err)
	}
	forwardingValue(t, "0")
	if len(forwarding.users) != 0 || len(forwarding.lanOwners) != 0 {
		t.Fatal("orphan cleanup leaked forwarding ownership")
	}
}

func TestInternetGrantsDeferOnlyOwnedForwardingRestoration(t *testing.T) {
	forwardingFixture(t, "0")
	previous := lanIPTables
	active, inspectFail, acquireFail := true, false, false
	lanIPTables = func(args ...string) ([]byte, error) {
		if acquireFail && len(args) > 3 && args[2] == "-N" && args[3] == BridgeInternetChain {
			return nil, errors.New("acquisition failed")
		}
		if len(args) > 3 && args[2] == "-S" && args[3] == BridgeInternetChain {
			if inspectFail {
				return nil, errors.New("inspection failed")
			}
			if active {
				return []byte("-A " + BridgeInternetChain + " -m comment --comment " + bridgeInternetMarker("owner") + " -j ACCEPT\n"), nil
			}
		}
		return previous(args...)
	}
	if err := AcquireForwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	inspectFail = true
	if err := ReleaseForwarding("mesh"); err == nil {
		t.Fatal("inspection failure authorized restoration")
	}
	forwardingValue(t, "1")
	inspectFail = false
	if err := ReleaseForwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	forwardingValue(t, "1")
	acquireFail = true
	if err := AcquireForwarding("new-agent"); err == nil {
		t.Fatal("acquisition failure was hidden")
	}
	if _, err := os.Stat(pendingForwardingPath()); err != nil {
		t.Fatal("failed acquisition lost deferred restoration", err)
	}
	forwardingValue(t, "1")
	acquireFail = false
	if err := AcquireForwarding("new-agent"); err != nil {
		t.Fatal(err)
	}
	active = false
	if err := withForwardingStateLock(restoreDeferredBridgeForwarding); err != nil {
		t.Fatal(err)
	}
	forwardingValue(t, "1")
	if err := ReleaseForwarding("new-agent"); err != nil {
		t.Fatal(err)
	}
	forwardingValue(t, "0")
	if err := os.WriteFile(pendingForwardingPath(), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := withForwardingStateLock(restoreDeferredBridgeForwarding); err == nil {
		t.Fatal("malformed deferred ownership accepted")
	}
}
