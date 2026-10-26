package hostnetwork

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type forwarding6Fixture struct {
	t          *testing.T
	journal    string
	links      []net.Interface
	guard      bool
	failDelete bool
	failEnable bool
	writes     []string
}

func newForwarding6Fixture(t *testing.T, initial string) *forwarding6Fixture {
	t.Helper()
	oldConf, oldJournal, oldInterfaces, oldTables, oldWrite := ipv6ForwardingConf, ipv6ForwardingJournal, ipv6ForwardingInterfaces, ipv6ForwardingIPTables, ipv6ForwardingWriteFile
	f := &forwarding6Fixture{t: t, journal: filepath.Join(t.TempDir(), "state", "namespace.json")}
	ipv6ForwardingConf = t.TempDir()
	ipv6Forwarding.users = map[string]bool{}
	ipv6ForwardingJournal = func() (string, error) { return f.journal, nil }
	ipv6ForwardingInterfaces = func() ([]net.Interface, error) { return f.links, nil }
	f.add("all", 0, initial, "1")
	f.add("default", 0, "0", "1")
	f.add("uplink", 2, "0", "1")
	f.add("static", 3, "1", "0")
	absent := exec.Command("sh", "-c", "exit 1").Run()
	ipv6ForwardingIPTables = func(args ...string) ([]byte, error) {
		switch args[0] {
		case "-C":
			if !f.guard {
				return nil, absent
			}
		case "-I":
			f.guard = true
		case "-D":
			if f.failDelete {
				return nil, errors.New("injected guard deletion failure")
			}
			f.guard = false
		default:
			t.Fatalf("unexpected iptables %v", args)
		}
		return nil, nil
	}
	ipv6ForwardingWriteFile = func(path string, data []byte, mode os.FileMode) error {
		rel, _ := filepath.Rel(ipv6ForwardingConf, path)
		value := strings.TrimSpace(string(data))
		f.writes = append(f.writes, rel+"="+value)
		if rel == filepath.Join("all", "forwarding") {
			if value == "1" {
				if !f.guard {
					t.Error("enabled global forwarding before fallback guard")
				}
				f.want("default", "accept_ra", "2")
				f.want("uplink", "accept_ra", "2")
				if f.failEnable {
					f.failEnable = false
					return errors.New("injected enable failure")
				}
			}
			// Linux writes to all.forwarding also overwrite default and existing
			// interface forwarding flags; a plain file mock would miss ordering bugs.
			for _, name := range append([]string{"default"}, f.names()...) {
				if err := os.WriteFile(filepath.Join(ipv6ForwardingConf, name, "forwarding"), data, mode); err != nil {
					return err
				}
			}
		}
		return os.WriteFile(path, data, mode)
	}
	t.Cleanup(func() {
		ipv6ForwardingConf, ipv6ForwardingJournal, ipv6ForwardingInterfaces, ipv6ForwardingIPTables, ipv6ForwardingWriteFile = oldConf, oldJournal, oldInterfaces, oldTables, oldWrite
		ipv6Forwarding.users = map[string]bool{}
	})
	return f
}
func (f *forwarding6Fixture) names() []string {
	var names []string
	for _, link := range f.links {
		names = append(names, link.Name)
	}
	return names
}
func (f *forwarding6Fixture) add(name string, index int, forwarding, ra string) {
	f.t.Helper()
	dir := filepath.Join(ipv6ForwardingConf, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		f.t.Fatal(err)
	}
	for key, value := range map[string]string{"forwarding": forwarding, "accept_ra": ra} {
		if err := os.WriteFile(filepath.Join(dir, key), []byte(value+"\n"), 0600); err != nil {
			f.t.Fatal(err)
		}
	}
	if index != 0 {
		f.links = append(f.links, net.Interface{Name: name, Index: index})
	}
}
func (f *forwarding6Fixture) want(name, key, value string) {
	f.t.Helper()
	got, err := readForwarding6(name, key)
	if err != nil || got != value {
		f.t.Fatalf("%s.%s = %q (%v), want %s", name, key, got, err, value)
	}
}
func (f *forwarding6Fixture) restored() {
	f.t.Helper()
	f.want("all", "forwarding", "0")
	f.want("default", "forwarding", "0")
	f.want("default", "accept_ra", "1")
	f.want("uplink", "forwarding", "0")
	f.want("uplink", "accept_ra", "1")
	f.want("static", "forwarding", "1")
	f.want("static", "accept_ra", "0")
	if f.guard {
		f.t.Fatal("guard retained after restore")
	}
	if _, err := os.Stat(f.journal); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("journal retained: %v", err)
	}
}

func TestIPv6ForwardingSharesLeaseAndRestoresDefaultsAndRA(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	for _, key := range []string{"mesh", "internet", "internet"} {
		if err := AcquireIPv6Forwarding(key); err != nil {
			t.Fatal(err)
		}
	}
	if len(ipv6Forwarding.users) != 2 {
		t.Fatal("duplicate lease counted")
	}
	f.want("all", "forwarding", "1")
	f.want("default", "accept_ra", "2")
	f.want("static", "accept_ra", "0")
	if err := ReconcileIPv6Forwarding(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseIPv6Forwarding("unknown"); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseIPv6Forwarding("internet"); err != nil {
		t.Fatal(err)
	}
	f.want("all", "forwarding", "1")
	// A newly created interface inherits the changed defaults. It must return
	// to the original defaults, even though it wasn't in the first snapshot.
	f.add("hotplug", 4, "1", "2")
	if err := ReleaseIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	f.restored()
	f.want("hotplug", "forwarding", "0")
	f.want("hotplug", "accept_ra", "1")
}
func TestIPv6ForwardingLeavesForeignGlobalOwnershipAlone(t *testing.T) {
	f := newForwarding6Fixture(t, "1")
	if err := AcquireIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	f.want("all", "forwarding", "1")
	f.want("uplink", "accept_ra", "1")
	if f.guard || len(f.writes) != 0 {
		t.Fatalf("modified foreign state: %v", f.writes)
	}
	if _, err := os.Stat(f.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected journal: %v", err)
	}
}
func TestIPv6ForwardingCrashRestoresPersistedInterfaceFlags(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	if err := AcquireIPv6Forwarding("dead"); err != nil {
		t.Fatal(err)
	}
	ipv6Forwarding.users = map[string]bool{}
	if err := ReconcileIPv6Forwarding(); err != nil {
		t.Fatal(err)
	}
	f.restored()
}
func TestIPv6ForwardingFailedAcquireRollsBack(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	f.failEnable = true
	if err := AcquireIPv6Forwarding("mesh"); err == nil {
		t.Fatal("enable failure lost")
	}
	if len(ipv6Forwarding.users) != 0 {
		t.Fatal("failed acquisition retained owner")
	}
	f.restored()
}
func TestIPv6ForwardingFailedCleanupRetainsLeaseAndReacquires(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	if err := AcquireIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	f.failDelete = true
	if err := ReleaseIPv6Forwarding("mesh"); err == nil {
		t.Fatal("cleanup failure lost")
	}
	if !ipv6Forwarding.users["mesh"] || !f.guard {
		t.Fatal("cleanup failure lost ownership or guard")
	}
	f.failDelete = false
	if err := AcquireIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	f.want("all", "forwarding", "1")
	if err := ReleaseIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	f.restored()
}
func TestIPv6ForwardingRejectsCorruptJournalAndPreservesChangedRA(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	if err := AcquireIPv6Forwarding("mesh"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ipv6ForwardingConf, "uplink", "accept_ra"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseIPv6Forwarding("mesh"); err == nil {
		t.Fatal("external RA change overwritten")
	}
	f.want("uplink", "accept_ra", "0")
	if !f.guard || !ipv6Forwarding.users["mesh"] {
		t.Fatal("failed cleanup lost protection")
	}
	ipv6Forwarding.users = map[string]bool{}
	if err := os.WriteFile(f.journal, []byte(`{"Version":1,"Global":"0","Default":{"Forwarding":"0","AcceptRA":"1"},"Interfaces":[{"Name":"../bad","Index":2,"Forwarding":"0","AcceptRA":"1"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileIPv6Forwarding(); err == nil {
		t.Fatal("unsafe journal accepted")
	}
}
func TestIPv6ForwardingConcurrentConsumers(t *testing.T) {
	f := newForwarding6Fixture(t, "0")
	if err := AcquireIPv6Forwarding("keeper"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprint(i)
			if err := AcquireIPv6Forwarding(key); err != nil {
				t.Error(err)
				return
			}
			if err := ReleaseIPv6Forwarding(key); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	f.want("all", "forwarding", "1")
	if len(ipv6Forwarding.users) != 1 {
		t.Fatal("leaked lease")
	}
	if err := ReleaseIPv6Forwarding("keeper"); err != nil {
		t.Fatal(err)
	}
	f.restored()
}
