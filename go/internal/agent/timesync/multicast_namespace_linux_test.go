//go:build linux

package timesync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"golang.org/x/net/ipv4"
)

// No transport, interface-list or refresh-timer seams are used here. Only the
// clock write is observed instead of changing the CI host's wall clock (#2122).
func TestMulticastInterfaceLifecycle(t *testing.T) {
	if os.Getenv("WENDY_MULTICAST_NAMESPACE") != "1" {
		t.Skip("requires the disposable Linux namespace CI fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("namespace fixture requires root")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		parent := os.Getenv("WENDY_MULTICAST_PARENT_" + kind)
		current, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("missing isolated %s namespace: parent=%q current=%q err=%v", kind, parent, current, err)
		}
	}
	ip := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
		return out
	}
	addLink := func(i int) {
		t.Helper()
		name, sender, ns := fmt.Sprintf("time%d", i), fmt.Sprintf("send%d", i), fmt.Sprintf("clock%d", i)
		ip("netns", "add", ns)
		ip("link", "add", name, "type", "veth", "peer", "name", sender)
		ip("link", "set", sender, "netns", ns)
		ip("address", "add", fmt.Sprintf("192.0.%d.1/24", 2+i), "dev", name)
		ip("link", "set", name, "up")
		ip("-n", ns, "address", "add", fmt.Sprintf("192.0.%d.2/24", 2+i), "dev", sender)
		ip("-n", ns, "link", "set", sender, "up")
		ip("-n", ns, "link", "set", "lo", "up")
		ip("-n", ns, "route", "add", multicastGroup+"/32", "dev", sender)
	}
	removeLink := func(i int) {
		t.Helper()
		ip("link", "del", fmt.Sprintf("time%d", i))
		ip("netns", "del", fmt.Sprintf("clock%d", i))
	}
	addLink(0)
	addLink(1)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	originalServers := Servers
	Servers = []roughtime.Server{{Name: "namespace fixture", Address: "unused", PublicKey: pub}}
	t.Cleanup(func() { Servers = originalServers })
	var mu sync.Mutex
	applied := make(map[int64]bool)
	m := &Manager{apply: func(at time.Time) {
		mu.Lock()
		applied[at.UnixMicro()] = true
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.RunMulticast(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("real multicast receiver did not stop on cancellation")
		}
	})
	wait := func(reason string, condition func() bool) {
		t.Helper()
		deadline := time.NewTimer(2*multicastRefreshInterval + 2*time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for !condition() {
			select {
			case <-deadline.C:
				mu.Lock()
				t.Logf("verified proof midpoints (Unix microseconds): %v", applied)
				mu.Unlock()
				t.Logf("interfaces: %s", ip("-j", "address"))
				t.Logf("memberships: %s", ip("-j", "maddr"))
				t.Fatal(reason)
			case <-done:
				t.Fatal("multicast receiver exited before " + reason)
			case <-tick.C:
			}
		}
	}
	// Every sender has a separate network namespace and one veth link. A packet
	// cannot arrive through a different interface or through local multicast loopback.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	startSender := func(i int, packet []byte, flood bool) (func(), func()) {
		t.Helper()
		dir := t.TempDir()
		path, ready := filepath.Join(dir, "packet"), filepath.Join(dir, "ready")
		if err := os.WriteFile(path, packet, 0600); err != nil {
			t.Fatal(err)
		}
		sendCtx, stop := context.WithTimeout(context.Background(), 100*time.Second)
		cmd := exec.CommandContext(sendCtx, "ip", "netns", "exec", fmt.Sprintf("clock%d", i), binary, "-test.run=^TestMulticastSenderProcess$", "-test.v", "-test.timeout=100s")
		cmd.Env = append(os.Environ(), "WENDY_MULTICAST_PACKET="+path, "WENDY_MULTICAST_READY="+ready,
			"WENDY_MULTICAST_INTERFACE="+fmt.Sprintf("send%d", i), "WENDY_MULTICAST_FLOOD="+fmt.Sprint(flood))
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			stop()
			t.Fatal(err)
		}
		finished := make(chan struct{})
		var processErr error
		go func() { processErr = cmd.Wait(); close(finished) }()
		var once sync.Once
		cleanup := func() {
			once.Do(func() {
				stop()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("multicast sender did not stop")
				}
			})
		}
		t.Cleanup(cleanup)
		checkAlive := func() {
			t.Helper()
			select {
			case <-finished:
				t.Fatalf("sender exited: %v: %s", processErr, output.String())
			default:
			}
		}
		wait("sender did not emit a packet", func() bool {
			checkAlive()
			_, err := os.Stat(ready)
			return err == nil
		})
		return cleanup, checkAlive
	}
	sequence := 0
	verifyDelivery := func(i int) {
		t.Helper()
		sequence++
		midpoint := time.Date(2026, 9, 30, 12, 0, sequence, 0, time.UTC)
		packet := buildRelayPacket(t, 0, priv, uint64(midpoint.UnixMicro()))
		stop, checkAlive := startSender(i, packet, false)
		defer stop()
		wait(fmt.Sprintf("multicast proof never arrived on time%d", i), func() bool {
			checkAlive()
			mu.Lock()
			defer mu.Unlock()
			return applied[midpoint.UnixMicro()]
		})
		t.Logf("verified signed time proof on time%d, sequence %d", i, sequence)
	}
	verifyDelivery(0)
	verifyDelivery(1)
	stopFlood, checkFlood := startSender(0, roughtime.Encode(roughtime.Datagram{MsgType: 0x42}), true)
	defer stopFlood()
	addLink(2) // hotplug while another interface keeps carrying packets
	verifyDelivery(2)
	verifyDelivery(0)
	hasMembership := func() bool {
		var links []struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
			} `json:"maddr"`
		}
		if err := json.Unmarshal(ip("-j", "maddr", "show", "dev", "time1"), &links); err != nil {
			t.Fatal(err)
		}
		if len(links) != 1 {
			t.Fatalf("missing interface in kernel membership query: %+v", links)
		}
		for _, link := range links {
			for _, address := range link.Addresses {
				if address.Family == "inet" && address.Address == multicastGroup {
					return true
				}
			}
		}
		return false
	}
	if !hasMembership() {
		t.Fatal("time1 had no kernel multicast membership before going down")
	}
	ip("link", "set", "time1", "down")
	wait("down interface retained its multicast membership", func() bool { return !hasMembership() })
	verifyDelivery(2)
	old, err := net.InterfaceByName("time1")
	if err != nil {
		t.Fatal(err)
	}
	removeLink(1)
	addLink(1)
	replacement, err := net.InterfaceByName("time1")
	if err != nil || replacement.Index == old.Index {
		t.Fatalf("interface replacement did not get a new kernel identity: %v, %v", replacement, err)
	}
	verifyDelivery(1)
	removeLink(2)
	verifyDelivery(0)
	checkFlood()
	stopFlood()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation left the real multicast socket blocked")
	}
	rebound, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", multicastPort))
	if err != nil {
		t.Fatalf("receiver retained its socket after cancellation: %v", err)
	}
	_ = rebound.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != sequence {
		t.Fatalf("received %d distinct proofs, want %d", len(applied), sequence)
	}
}

// Re-executed in a sender namespace by the parent; never run as a standalone test.
func TestMulticastSenderProcess(t *testing.T) {
	path := os.Getenv("WENDY_MULTICAST_PACKET")
	if path == "" {
		return
	}
	packet, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	iface, err := net.InterfaceByName(os.Getenv("WENDY_MULTICAST_INTERFACE"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	conn := ipv4.NewPacketConn(raw)
	defer conn.Close()
	for _, err := range []error{conn.SetMulticastInterface(iface), conn.SetMulticastTTL(1), conn.SetMulticastLoopback(false)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	interval := 20 * time.Millisecond
	if os.Getenv("WENDY_MULTICAST_FLOOD") == "true" {
		interval = time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	dst := &net.UDPAddr{IP: net.ParseIP(multicastGroup), Port: multicastPort}
	for n := 0; ; n++ {
		if _, err := conn.WriteTo(packet, nil, dst); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			if err := os.WriteFile(os.Getenv("WENDY_MULTICAST_READY"), []byte("sent\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		<-tick.C
	}
}
