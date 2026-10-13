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
