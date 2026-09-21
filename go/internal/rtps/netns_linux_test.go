//go:build linux

package rtps

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestHostNetworkContainersAndBatteryShareTwoParticipants(t *testing.T) {
	pool, cfg := testPool(t)
	wired, err := HostInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(wired) == 0 {
		t.Skip("requires a wired IPv4 interface")
	}
	cfg.Interface = wired[0]
	host := acquire(t, pool, cfg)
	var apps []*Lease
	for range 4 {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		for _, iface := range []string{loopbackInterface(t), wired[0]} {
			target := cfg
			target.Interface = iface
			target.NetworkNamespacePID = uint32(cmd.Process.Pid)
			apps = append(apps, acquire(t, pool, target))
		}
	}
	battery := acquire(t, pool, cfg)
	if battery.entry != host.entry || pool.Participants() != 2 {
		t.Fatalf("physical participants=%d; want wired+loopback", pool.Participants())
	}
	_ = battery.Close()
	for _, app := range apps {
		_ = app.Close()
	}
	if pool.Participants() != 1 {
		t.Fatal("app reconciliation failed to retain only host discovery")
	}
}

func TestNamespaceOwnershipCheckedAfterConstructionAndOnReuse(t *testing.T) {
	pool, cfg := testPool(t)
	cfg.NetworkNamespacePID = uint32(os.Getpid())
	valid := true
	cfg.VerifyNetworkNamespace = func() bool { return valid }
	pool.create = func(c Config) (*Participant, error) { p, err := NewParticipant(c); valid = false; return p, err }
	if _, err := pool.Acquire(context.Background(), cfg); err == nil {
		t.Fatal("changed owner survived construction")
	}
	if pool.Participants() != 0 {
		t.Fatal("rejected owner retained participant")
	}
	valid = true
	pool.create = NewParticipant
	a := acquire(t, pool, cfg)
	valid = false
	if _, err := pool.Acquire(context.Background(), cfg); err == nil {
		t.Fatal("existing pool entry bypassed owner verification")
	}
	if pool.Participants() != 1 {
		t.Fatal("failed acquisition interrupted original lease")
	}
	_ = a.Close()
}

func TestNamespaceIdentityPinsDescriptorAndRejectsRecycledPID(t *testing.T) {
	ns, err := captureNetworkNamespace(uint32(os.Getpid()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.close()
	if !ns.host || ns.identity.inode == 0 {
		t.Fatal("host namespace identity unavailable")
	}
	ns.start = "recycled"
	entered := false
	if err := ns.run(func() error { entered = true; return nil }); err == nil || entered {
		t.Fatal("recycled PID entered namespace")
	}
}

func TestIsolatedNamespaceUsesSeparateParticipant(t *testing.T) {
	if os.Getenv("WDY_TEST_NETNS") == "1" {
		if output, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
			t.Fatalf("loopback: %s: %v", output, err)
		}
		if err := os.WriteFile(os.Getenv("WDY_TEST_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare unavailable")
	}
	if output, err := exec.Command("unshare", "--net", "ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Skipf("network namespace capability unavailable: %s", output)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("ip unavailable")
	}
	ready := t.TempDir() + "/ready"
	cmd := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestIsolatedNamespaceUsesSeparateParticipant$")
	cmd.Env = append(os.Environ(), "WDY_TEST_NETNS=1", "WDY_TEST_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("namespace helper not ready")
		}
		time.Sleep(time.Millisecond * 10)
	}
	pool, cfg := testPool(t)
	host := acquire(t, pool, cfg)
	cfg.NetworkNamespacePID = uint32(cmd.Process.Pid)
	isolated := acquire(t, pool, cfg)
	if host.entry == isolated.entry || pool.Participants() != 2 {
		t.Fatal("isolated namespace shared host participant")
	}
	if isolated.entry.ns.host {
		t.Fatal("isolated namespace identified as host")
	}
	_ = isolated.Close()
	if pool.Participants() != 1 {
		t.Fatal("isolated shutdown interrupted host")
	}
}

func TestAutomaticHostInterfaceResolvesBeforePoolLookup(t *testing.T) {
	pool, cfg := testPool(t)
	wired, err := HostInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(wired) == 0 {
		t.Skip("no wired interface")
	}
	cfg.Interface = wired[0]
	a := acquire(t, pool, cfg)
	cfg.Interface = ""
	cfg.NetworkNamespacePID = uint32(os.Getpid())
	b := acquire(t, pool, cfg)
	if a.entry != b.entry {
		t.Fatal("automatic host interface did not share explicit target")
	}
	names, err := DiscoveryInterfaces(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(wired)+1 || names[0] != loopbackInterface(t) {
		t.Fatalf("host app coverage=%v wired=%v", names, wired)
	}
}
