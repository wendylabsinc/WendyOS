//go:build linux

package rosbattery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"golang.org/x/sys/unix"
)

// Capture actual SPDP egress from Monitor.Run, including the wireless-only
// case which originally fell back to automatic RTPS interface selection. Veth
// paths stand in for wired/radio links; the test does not emulate radio hardware.
func TestAutomaticROSDiscoveryNeverUsesWirelessPath(t *testing.T) {
	if os.Getenv("WENDY_PACKET_BACKEND_NAMESPACE") != "1" {
		t.Skip("requires the disposable Linux network CI fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("packet capture fixture requires root")
	}
	for _, ns := range []string{"mnt", "net", "pid"} {
		parent := os.Getenv("WENDY_PACKET_PARENT_" + ns)
		current, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("packet capture fixture lacks an isolated %s namespace", ns)
		}
	}
	paths := []struct{ name, peer, address string }{
		{"ethfixture", "veth-wired", "192.0.2.1/24"},
		{"wlanfixture", "veth-radio", "198.51.100.1/24"},
	}
	var wired netlink.Link
	for _, path := range paths {
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: path.name}, PeerName: path.peer}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(link) })
		address, err := netlink.ParseAddr(path.address)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(link, address); err != nil {
			t.Fatal(err)
		}
		peer, err := netlink.LinkByName(path.peer)
		if err != nil {
			t.Fatal(err)
		}
		for _, side := range []netlink.Link{link, peer} {
			if err := netlink.LinkSetUp(side); err != nil {
				t.Fatal(err)
			}
		}
		if path.name == "ethfixture" {
			wired = link
		}
	}
	// Peripheral policy also requires physical-device metadata. Supply only
	// that metadata in this private mount namespace; interface enumeration,
	// routing, multicast sockets and both captured packet paths remain real.
	sysfs := t.TempDir()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(sysfs, path.name, "device"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount(sysfs, "/sys/class/net", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount("/sys/class/net", 0); err != nil {
			t.Error(err)
		}
	})

	// Each phase has its own DDS domain, so a queued packet from a positive
	// control cannot satisfy a later assertion or look like a policy leak.
	t.Run("explicit-wireless-control", func(t *testing.T) {
		counts, _ := captureBatteryDiscovery(t, Config{Enabled: true, DomainID: 211, Interfaces: []string{"wlanfixture"}})
		if counts[1] == 0 || counts[0] != 0 {
			t.Fatalf("explicit wireless override did not produce isolated SPDP egress: %v", counts)
		}
	})
	t.Run("automatic-wired-and-wireless", func(t *testing.T) {
		// The capture ends long before discovery would move on from the wired
		// candidate, so packets alone cannot show the wireless one was excluded.
		if candidates := candidateInterfaces(); !slices.Equal(candidates, []string{"ethfixture"}) {
			t.Fatalf("automatic discovery candidates are not the wired peripheral alone: %v", candidates)
		}
		counts, _ := captureBatteryDiscovery(t, Config{Enabled: true, DomainID: 212})
		if counts[1] != 0 {
			t.Fatalf("automatic discovery emitted RTPS on wireless path: %v", counts)
		}
		if counts[0] == 0 {
			t.Fatal("automatic discovery did not emit SPDP on the wired peripheral")
		}
	})
	if err := netlink.LinkSetDown(wired); err != nil {
		t.Fatal(err)
	}
	t.Run("automatic-wireless-only", func(t *testing.T) {
		counts, noCandidates := captureBatteryDiscovery(t, Config{Enabled: true, DomainID: 213})
		if counts[1] != 0 {
			t.Fatalf("automatic discovery emitted RTPS on wireless path: %v", counts)
		}
		if counts[0] != 0 || !noCandidates {
			t.Fatalf("wireless-only discovery did not finish without a participant: packets=%v noCandidates=%v", counts, noCandidates)
		}
	})
}

func captureBatteryDiscovery(t *testing.T, cfg Config) ([2]int, bool) {
	t.Helper()
	var descriptors [2]int
	for i, name := range []string{"veth-wired", "veth-radio"} {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal(err)
		}
		// Network-order ETH_P_IP. Binding the peer endpoint observes frames
		// that crossed the veth, rather than multicast socket loopback copies.
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(htonsROS(unix.ETH_P_IP)))
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htonsROS(unix.ETH_P_IP), Ifindex: iface.Index}); err != nil {
			t.Fatal(err)
		}
		descriptors[i] = fd
	}
	pool := rtps.NewPool()
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var noCandidates atomic.Bool
	monitor := NewMonitor(cfg, NewCache(time.Now), pool, func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "no wired multicast interface") {
			noCandidates.Store(true)
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); monitor.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("battery monitor did not finish after cancellation")
		}
	}()
	var counts [2]int
	poll := []unix.PollFd{{Fd: int32(descriptors[0]), Events: unix.POLLIN}, {Fd: int32(descriptors[1]), Events: unix.POLLIN}}
	packet := make([]byte, 65536)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := unix.Poll(poll, 25); err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		for i, fd := range descriptors {
			for {
				n, _, err := unix.Recvfrom(fd, packet, unix.MSG_DONTWAIT)
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if isBatterySPDP(packet[:n], cfg.DomainID) {
					counts[i]++
				}
			}
		}
	}
	return counts, noCandidates.Load()
}

func htonsROS(value uint16) uint16 {
	var network [2]byte
	binary.BigEndian.PutUint16(network[:], value)
	return binary.NativeEndian.Uint16(network[:])
}

func isBatterySPDP(frame []byte, domain int) bool {
	if len(frame) < 14+20+8 || binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_IP {
		return false
	}
	ip := frame[14:]
	ihl := int(ip[0]&15) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl+8 || ip[9] != unix.IPPROTO_UDP {
		return false
	}
	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[2:4]) != uint16(7400+250*domain) {
		return false
	}
	message, err := rtps.ParseMessage(udp[8:])
	if err != nil {
		return false
	}
	for _, submessage := range message.Submessages {
		if submessage.Kind != 0x15 { // RTPS DATA
			continue
		}
		data, err := rtps.ParseData(submessage)
		if err == nil && data.WriterID == 0x000100c2 { // SPDP builtin participant writer
			return true
		}
	}
	return false
}
