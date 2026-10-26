//go:build linux

package ipcam

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Run as root inside a disposable network AND mount namespace:
// sudo unshare --mount --net --propagation private env WENDY_TEST_NETNS=1 ./ipcam.test -test.run '^TestDHCPLinuxSharedLANClient$' -test.v
// Real dnsmasq and udhcpc exchange packets through a bridge and veth pairs.
// The agent's addressless port observes the shared LAN, but must not register
// the laptop requesting a lease as a camera. No production device is involved.
func TestDHCPLinuxSharedLANClient(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated Linux network/mount namespaces, root, ip, dnsmasq and busybox")
	}
	if os.Geteuid() != 0 {
		t.Fatal("network namespace test requires root")
	}
	// Refuse the privileged fixture unless both namespaces differ from PID 1.
	// Setting the opt-in alone must never alter the host's links or mounts.
	for _, namespace := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + namespace)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + namespace)
		if err != nil {
			t.Fatal(err)
		}
		if self == init {
			t.Fatalf("requires a private %s namespace; run with unshare --mount --net", namespace)
		}
	}
	// /sys belongs to the namespace in which it was mounted. Refresh it in
	// this private mount namespace so production carrier reads see our veths.
	if out, err := exec.Command("mount", "-t", "sysfs", "-o", "ro,nosuid,nodev,noexec", "sysfs", "/sys").CombinedOutput(); err != nil {
		t.Fatalf("mount isolated sysfs: %s: %v", out, err)
	}
	for _, command := range []string{"ip", "dnsmasq", "busybox"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatal(err)
		}
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	serverNS := fmt.Sprintf("wendy-server-%d", os.Getpid())
	clientNS := fmt.Sprintf("wendy-client-%d", os.Getpid())
	for _, ns := range []string{serverNS, clientNS} {
		ip("netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		ip("-n", ns, "link", "set", "lo", "up")
	}
	ip("link", "add", "brlan", "type", "bridge")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "brlan").Run() })
	ip("link", "set", "brlan", "up")
	for _, port := range []string{"agent", "server", "client"} {
		ip("link", "add", port+"0", "type", "veth", "peer", "name", port+"lan")
		t.Cleanup(func() { _ = exec.Command("ip", "link", "del", port+"lan").Run() })
		ip("link", "set", port+"lan", "master", "brlan")
		ip("link", "set", port+"lan", "up")
	}
	ip("link", "set", "agent0", "up")
	ip("link", "set", "server0", "netns", serverNS)
	ip("link", "set", "client0", "netns", clientNS)
	ip("-n", serverNS, "addr", "add", "192.0.2.1/24", "dev", "server0")
	ip("-n", serverNS, "link", "set", "server0", "up")
	ip("-n", clientNS, "link", "set", "client0", "up")

	dir := t.TempDir()
	leaseFile := filepath.Join(dir, "dnsmasq.leases")
	logPath := filepath.Join(dir, "dnsmasq.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, "ip", "netns", "exec", serverNS, "dnsmasq",
		"--conf-file=/dev/null", "--keep-in-foreground", "--pid-file=", "--user=root",
		"--port=0", "--no-hosts", "--no-resolv", "--bind-interfaces", "--interface=server0",
		"--dhcp-authoritative", "--dhcp-range=192.0.2.50,192.0.2.100,255.255.255.0,1h",
		"--dhcp-leasefile="+leaseFile, "--log-dhcp", "--log-facility=-")
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		stopServer()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopServer()
		_ = server.Wait()
		if log, err := os.ReadFile(logPath); err == nil {
			t.Logf("normal DHCP server:\n%s", log)
		}
	})
	waitFor(t, "dnsmasq startup", func() bool {
		log, _ := os.ReadFile(logPath)
		return strings.Contains(string(log), "DHCP, IP range")
	})

	reg := NewRegistry(filepath.Join(dir, "cameras.json"))
	manager := NewLinkManager(reg, zap.NewNop())
	listInterfaces := manager.listInterfaces
	manager.listInterfaces = func() ([]Interface, error) {
		ifaces, err := listInterfaces()
		if err != nil {
			return nil, err
		}
		for _, iface := range ifaces {
			if iface.Name == "agent0" {
				return []Interface{iface}, nil
			}
		}
		return nil, fmt.Errorf("fixture agent0 is missing")
	}
	watchStarted := make(chan struct{})
	manager.watch = func(ctx context.Context, link string, onPacket func(*Packet)) error {
		close(watchStarted)
		return watchDHCP(ctx, link, onPacket)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	manager.scanOnce(watchCtx)
	// The test binary runs alone in a disposable namespace. Cancelling the
	// watcher and deleting its veth are sufficient; process exit also closes
	// any raw packet read still waiting for traffic after cancellation.
	t.Cleanup(stopWatch)
	select {
	case <-watchStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not watch the addressless LAN port")
	}
	// Request broadcast replies, as a client without an address may do, so
	// every port on the shared LAN also sees the normal server's replies.
	// udhcpc retries until dnsmasq answers; no sleeps or injected DHCP packets
	// stand in for the kernel receive path or either end of the DHCP exchange.
	clientCtx, stopClient := context.WithTimeout(context.Background(), 8*time.Second)
	defer stopClient()
	out, err := exec.CommandContext(clientCtx, "ip", "netns", "exec", clientNS,
		"busybox", "udhcpc", "-B", "-i", "client0", "-x", "hostname:MacBooks-Air",
		"-n", "-q", "-t", "5", "-T", "1", "-s", "/bin/true").CombinedOutput()
	if err != nil {
		t.Fatalf("real DHCP client failed: %s: %v", out, err)
	}
	t.Logf("ordinary laptop DHCP client:\n%s", out)
	leases, err := os.ReadFile(leaseFile)
	if err != nil || !strings.Contains(string(leases), "MacBooks-Air") {
		t.Fatalf("normal DHCP server did not lease to laptop: %s: %v", leases, err)
	}
	t.Logf("normal LAN leases: %s", leases)
	waitFor(t, "foreign DHCP reply observed", func() bool {
		return manager.State("agent0") == LinkDisqualified
	})
	// Once the reply has traversed the watcher callback, the registry must
	// remain empty. A second scan must also leave the normal LAN unclaimed.
	manager.scanOnce(watchCtx)
	if manager.isClaimed("agent0") {
		t.Fatal("manager claimed a shared LAN with a normal DHCP server")
	}
	if cams := reg.List(); len(cams) != 0 {
		t.Fatalf("shared LAN clients registered as cameras: %+v", cams)
	}
	if iface, err := net.InterfaceByName("agent0"); err != nil {
		t.Fatal(err)
	} else if addrs, err := iface.Addrs(); err != nil {
		t.Fatal(err)
	} else {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				t.Fatalf("agent added IPv4 to shared LAN port: %v", addrs)
			}
		}
	}
}
